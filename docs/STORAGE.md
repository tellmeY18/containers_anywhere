# Storage

The data disk is a single btrfs filesystem. Persistent state lives under `/var/lib`, as before,
but it is now divided into subvolumes so that snapshots, restore points and per-volume operations
are cheap and independent. **This is new in 0.4 and has had little testing: use disposable data.**
There is no migration from 0.3.x; start 0.4.x with a new empty state directory.

## Layout

| Subvolume | Mounted at | Holds | In restore points |
| --- | --- | --- | --- |
| `@system` | `/var/lib` | K3s database, node identity, appliance state, `data-format.json` | yes |
| (nested) | `rancher/k3s/agent/containerd` | container images | no (re-downloadable) |
| (nested) | `containers-anywhere/journal` | guest logs (capped) | no |
| (nested) | `rancher/k3s/storage` | volumes; each PersistentVolumeClaim is its own child subvolume | per volume |
| `@snapshots` | `/run/ca/snapshots` | snapshots and restore points | n/a |

`@snapshots` is mounted outside `/var/lib` and is never visible to a pod. The disk starts at
8 GiB (sparse: the host only uses what is written). It grows through a host request (below);
it never shrinks. Copy-on-write is on by default, so snapshots are cheap and btrfs checksums
cover the data. A monthly `btrfs scrub` checks it.

## Volumes

K3s's bundled local-path provisioner is disabled. The guest agent provisions volumes instead:

| StorageClass | Use |
| --- | --- |
| `containers-anywhere-btrfs` (default) | a subvolume per claim |
| `containers-anywhere-btrfs-nocow` | the same with copy-on-write off for new files: for databases that do many small random writes. btrfs checksums do not cover these files, and a snapshot still copies a block on its first write afterwards. Choose it deliberately, and enable the database's own checksums. |
| `local-path` | an alias of the default, so existing manifests keep working |

Sizes in a claim are not enforced. A volume is deleted with its claim (as local-path did).

## Snapshots (workload API)

Group `storage.containersanywhere.dev/v1alpha1`, namespaced. A workload needs no host access.

```yaml
apiVersion: storage.containersanywhere.dev/v1alpha1
kind: PVCSnapshot
metadata: {name: before-upgrade, namespace: app}
spec: {persistentVolumeClaimName: data}        # a claim in the same namespace
---
apiVersion: storage.containersanywhere.dev/v1alpha1
kind: PVCSnapshotSchedule
metadata: {name: hourly, namespace: app}
spec: {persistentVolumeClaimName: data, intervalMinutes: 60, keep: 3}   # 5-10080 minutes, keep 1-10
---
# Restore is a clone into a new claim, never an in-place overwrite.
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: data-restored, namespace: app}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 10Gi}}
  dataSourceRef: {apiGroup: storage.containersanywhere.dev, kind: PVCSnapshot, name: before-upgrade}
```

`status.phase` is `Ready` or `Failed` (with a `message`). Limits: 16 snapshots per namespace, and
no new snapshot or clone below 15% free space on the data disk. Deleting a `PVCSnapshot` removes
its snapshot. Snapshots are **crash-consistent** (like a power cut), not application-consistent,
and they are on the same disk: they do not protect against losing it. They are not point-in-time
recovery: for that, a database needs its own WAL archive and base backups (see docs/DISASTER_RECOVERY.md).

## Restore points

The first time a different appliance version boots on a disk, before the cluster starts, the
guest takes a read-only snapshot of `@system` and of every volume under
`/run/ca/snapshots/restore-<time>-from-<version>`. The last two are kept. **There is no
automatic restore yet**: they preserve the previous state so a restore can be built; today
recovering from one is an operator task.

## Image cache and unclean shutdowns

The container image cache (`rancher/k3s/agent/containerd`) is a copy of what registries hold. After a
shutdown that was not clean (a panic, a killed VM, power loss), btrfs can keep a file that containerd
extracted but lose its data, so a binary is empty while containerd believes the layer is fine. The agent
keeps a session marker (`containers-anywhere/session`), removed by a unit that stops after K3s on a clean
shutdown. If the marker is present at boot, the cache is emptied and re-downloaded. Volumes and the cluster
database are never touched.

## Growing the disk

A `HostRequest` of type `resources` may carry `diskGiB` (8 to 256). After a launcher approval, the
next start extends the sparse image (refused if the computer lacks that much free space) and the
guest enlarges the filesystem when it mounts it. It never shrinks.

## Limits and risks

- btrfs can fail to allocate metadata on a nearly full small disk even when data space looks free.
  The 15% reserve and the cap are the first defences; a full disk has not been tested.
- Postgres and SQLite performance under copy-on-write has not been measured.
- Per-volume size limits (qgroups) are not enforced.
- Boot has been proven on Linux/KVM only (when CI ran it); macOS and Windows are unproven.

## Decisions

- **Our own provisioner, no CSI.** Volumes are direct subvolumes provisioned by the guest agent. We
  evaluated [btrfs-nfs-csi](https://github.com/erikmagkekse/btrfs-nfs-csi) (Apache-2.0, beta, moving to a
  fork called ButterStore). It mounts volumes only over NFS, which a single node does not need, and it
  brings an agent, a CSI controller and node driver, about nine pinned images, and a heavy dependency
  tree. We use it as a **reference only**: qgroup quotas (simple quotas on kernel 6.7+), online expansion
  and compression handling are the ideas to borrow, with its attribution kept in THIRD_PARTY_NOTICES.md if
  any code is copied. Revisit if it gains a direct-mount backend.
- **Own CRDs before the standard VolumeSnapshot API.** The standard API needs a CSI driver; revisit it if
  volume-snapshot backups (for example CNPG's) become a requirement.
- **Per-volume size limits are not enforced yet.**
