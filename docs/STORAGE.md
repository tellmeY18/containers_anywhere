# Storage

The data disk is a single btrfs filesystem. **This is new in 0.4 and has had little testing: use disposable data.**
There is no migration from earlier versions; start with a new empty state directory.

## Who does what

| Layer | Owns |
| --- | --- |
| **Appliance** (rarely updated) | Vanilla K3s with its bundled local-storage disabled (so there is one default class), the btrfs data disk and its mount, and the few boot-time duties that must run before K3s: a root-owned mount root, the image-cache and journal subvolumes, the unclean-shutdown rule, and the restore point on a version change. |
| **App** (updated freely) | The btrfs CSI driver and its sidecars, the snapshot controller and CRDs, the StorageClasses, and the pool mount (an init container of the driver). `platform/btrfs-csi.yaml`, `platform/snapshots.yaml`, `platform/storage.yaml`. |
| **Workloads** | Ordinary PersistentVolumeClaims and `VolumeSnapshot`s. No host access. |

The driver is [mporrato/btrfs-csi](https://github.com/mporrato/btrfs-csi) (Unlicense, single-node), with the standard
kubernetes-csi sidecars. Every image is pinned by digest in the platform manifests. The driver pod is privileged (it needs
btrfs ioctls and mount propagation): a compromise of it is root on the node.

## Disk layout

| Subvolume | Mounted at | Holds | In restore points |
| --- | --- | --- | --- |
| `@system` | `/var/lib` | K3s database, node identity, appliance state, `data-format.json` | yes |
| (nested) | `rancher/k3s/agent/containerd` | container images | no (re-downloadable) |
| (nested) | `containers-anywhere/journal` | guest logs (capped) | no |
| (nested) | `btrfs-csi-pool`, bind-mounted at `/var/lib/btrfs-csi/default` | the CSI pool: `volumes/<id>`, `snapshots/<id>`, `state.json` | volumes yes, copied explicitly |
| `@snapshots` | `/run/ca/snapshots` | appliance restore points | n/a |

`@snapshots` is mounted outside `/var/lib` and is never visible to a pod. The disk starts at 100 GiB (sparse: the host only uses what is written) and can grow to 256 GiB
through a host request; it never shrinks. A monthly `btrfs scrub` checks it.

## Using it

```yaml
# a volume (the default class is btrfs; bound when a pod first uses it)
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: data}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 10Gi}}        # a hard limit (btrfs simple quotas); edit it to expand online
---
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: before-upgrade}
spec: {source: {persistentVolumeClaimName: data}}
---
# restore or clone: a new claim from the snapshot, or straight from another claim
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: data-restored}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 10Gi}}
  dataSource: {name: before-upgrade, kind: VolumeSnapshot, apiGroup: snapshot.storage.k8s.io}
```

| StorageClass | Use |
| --- | --- |
| `btrfs` (default) | copy-on-write on: cheap snapshots, btrfs checksums cover the data |
| `btrfs-nodatacow` | copy-on-write off for new files, for databases with many small random writes. btrfs checksums do not cover them. Chosen when the volume is created; it cannot be changed afterwards. |
| `local-path` | an alias of `btrfs`, so manifests that ask for the old K3s class keep working |

Snapshots are **crash-consistent** (like a power cut), not application-consistent, and they are on the same disk:
they do not protect against losing it. They are not point-in-time recovery: a database needs its own WAL archive and base
backups (see docs/DISASTER_RECOVERY.md). Snapshots and volumes share the disk's free space; keep headroom.

## Restore points

The first time a different appliance version boots on a disk, before the cluster starts, the agent takes a read-only
snapshot of `@system` and of every pool volume (and keeps the driver's `state.json`) under
`/run/ca/snapshots/restore-<time>-from-<version>`. The last two are kept.

### Restoring one (launcher only)

Capability `system-restore-v1`. A person lists the points and restores one in the launcher panel, or with
`restore-point --operation list` and `restore-point --operation restore --id <point> --accept-data-loss`. No workload can ask
for it. Only a point taken under **the appliance version now running** can be restored (K3s must not move backwards under
its database); to go back across an appliance update, run the matching appliance first.

Asking records the request on the disk and restarts the appliance. The restore then runs in `init-data`, before K3s or
anything else writes to the disk (`/var/lib` is mounted in the initrd, so it is replaced in place, not swapped):

1. The state being replaced is first kept as a restore point of its own (`...-from-before-restore`). Nothing is deleted
   before that point exists, and the point being restored from is never pruned.
2. `/var/lib` is emptied except the image cache, the live journal and the pool; the point's `@system` snapshot is copied over
   it (reflinks, so it is cheap); every pool volume is replaced by the point's copy and the driver's `state.json` is the
   point's; the image cache is emptied and re-downloaded.
3. If it fails part-way, the point from step 1 is restored in its place. If that fails too, the cluster does **not** start
   (the console says why); both states are still in `@snapshots`. A power cut mid-restore resumes it at the next boot.

What a restore does not cover: CSI `VolumeSnapshot` data (the snapshots directory in the pool is left as it is, so snapshots
made after the point become orphans that still occupy space), anything outside the disk (images, registries, external
systems), and application-level consistency: it is the disk as it was, not point-in-time recovery of any database. It
discards everything written since, including workloads installed since.

## Image cache and unclean shutdowns

The container image cache is a copy of what registries hold. After a shutdown that was not clean (a panic, a killed VM,
power loss), btrfs can keep a file that containerd extracted but lose its data, so a binary is empty while containerd
believes the layer is fine. The agent keeps a session marker (`containers-anywhere/session`), removed by a unit that stops
after K3s on a clean shutdown. If the marker is present at boot, the cache is emptied and re-downloaded. Volumes and the
cluster database are never touched.

## Growing the disk

The data disk is a **sparse file** on the host: its size is a ceiling, and the host only uses the blocks the VM has actually
written. A fresh disk is **100 GiB** (set by the appliance bundle). A `HostRequest` of type `resources` may carry `diskGiB`
(up to **256**, the current ceiling). After a launcher approval, the next start extends the file (refused if the computer has
less free space than the growth) and the guest enlarges the filesystem when it mounts it. It never shrinks.

Where each part lives: the starting size is the **appliance's** (`data_bytes` in its manifest); growing the file is the **app's**
(the launcher, on approval); enlarging the filesystem is the **appliance's** (at boot); and the ceiling is a constant compiled
into the app and the guest (`MaxDiskGiB`) and mirrored in the HostRequest schema in `platform/hostapi.yaml`.

Things to know: host usage only grows (deleted data inside the VM is not guaranteed to shrink the file); backups and copies
encrypted backups and recovery points keep the holes (a map plus the data extents; Windows archives densely), but the update safety copy does not yet preserve sparseness; and Finder, Time Machine and cloud sync can see
the apparent size.

### Direction for production (not built)

- **Ceiling as a launcher setting**, chosen by the person who owns the computer (stored with the cluster state, edited in the
  panel with a confirmation), with a high hard cap in code. Workloads can only request growth inside it.
- **Built:** `HostStatus.status.host.dataDisk` reports `sizeBytes`, `allocatedBytes` (the real cost on this computer), `ceilingBytes`, `hostFreeFloorBytes` and `maxGrowthBytes`; a growth that would leave the computer with less than the floor (the larger of 20 GiB and a tenth of the volume) is refused at approval with the reason. Still planned: the ceiling as a launcher setting and optional preallocation.
- **A host-free floor**: refuse growth if it would leave the host below a reserve (for example 10% or 20 GiB), not only if the
  growth does not fit.
- **Optional reservation**: preallocate the space (macOS `F_PREALLOCATE`, Linux `fallocate`, Windows allocated size) so the VM can
  never run the host out of space, at the cost of using it immediately. Thin (sparse) stays the default.
- **Request more, approve in the launcher**: keep the existing `HostRequest` flow; surface ceiling, current size and host free
  space in `HostStatus` so a manager like CNCM can show real headroom.
- Sparse-aware backup and copy.

## Limits and risks

- `df` inside a pod shows the pool, not the volume's quota (the quota still enforces). The driver reports a snapshot
  `restoreSize` of 16Ki. No `VolumeAttributesClass` (so compression and copy-on-write cannot be changed live), group
  snapshots, or scheduled snapshots.
- btrfs can fail to allocate metadata on a nearly full small disk even when data space looks free. A full disk has not been
  tested.
- The driver keeps its own `state.json`; what happens when it and the disk disagree has not been tested.
- Boot has been proven on Linux/KVM in CI and by hand on an Apple-silicon Mac; Windows is unproven.

## Decisions

- **A standard CSI driver, not our own provisioner.** We first built a guest-agent provisioner and custom snapshot types,
  then replaced them with the upstream driver so every tool that speaks CSI and `VolumeSnapshot` works unchanged, and so
  the appliance stays vanilla. We evaluated [btrfs-nfs-csi](https://github.com/erikmagkekse/btrfs-nfs-csi): it mounts
  volumes only over NFS, which a single node does not need, and it brings far more to run. It is a reference only.
- **Scheduled snapshots:** none in core Kubernetes; use each tool's own schedule (CNPG, Velero) until there is demand.
