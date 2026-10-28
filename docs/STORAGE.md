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

`@snapshots` is mounted outside `/var/lib` and is never visible to a pod. The disk starts at 8 GiB (sparse) and grows
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
`/run/ca/snapshots/restore-<time>-from-<version>`. The last two are kept. **There is no automatic restore yet**: they
preserve the previous state so a restore can be built; today recovering from one is an operator task.

## Image cache and unclean shutdowns

The container image cache is a copy of what registries hold. After a shutdown that was not clean (a panic, a killed VM,
power loss), btrfs can keep a file that containerd extracted but lose its data, so a binary is empty while containerd
believes the layer is fine. The agent keeps a session marker (`containers-anywhere/session`), removed by a unit that stops
after K3s on a clean shutdown. If the marker is present at boot, the cache is emptied and re-downloaded. Volumes and the
cluster database are never touched.

## Growing the disk

A `HostRequest` of type `resources` may carry `diskGiB` (8 to 256). After a launcher approval, the next start extends the
sparse image (refused if the computer lacks that much free space) and the guest enlarges the filesystem when it mounts it.
It never shrinks.

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
