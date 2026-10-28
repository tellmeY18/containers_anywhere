# Containers Anywhere 0.4.0 (beta)

Beta, not production-ready. App and appliance both change. **There is no migration from 0.3.x:**
the data disk is now btrfs, so 0.4.0 needs a new empty state directory. Use disposable data.

## What's new

- **btrfs data disk** with subvolumes for the system, container images, logs, each volume, and
  snapshots (`docs/STORAGE.md`). It starts at 8 GiB (sparse) and grows on request.
- **Volumes are subvolumes.** A guest-agent provisioner replaces local-path. Default class
  `containers-anywhere-btrfs`, plus an opt-in `-nocow` class for databases.
- **Snapshot API for workloads.** `PVCSnapshot`, `PVCSnapshotSchedule` and clone-restore through
  `dataSourceRef`, with a per-namespace cap and a free-space reserve.
- **Restore point on a root change.** The first boot of a different appliance version snapshots the
  system and all volumes before the cluster starts (last two kept). Restoring from one is not
  automated yet.
- **One-tap update.** When this app recommends a newer appliance, the panel offers **Update now**:
  it replaces only the root, keeps your data, and returns to the current version by itself if the new
  one is not healthy. A K3s change still needs the careful update under **Advanced**, which makes a
  safety copy first.
- **Disk growth.** A host request may carry `diskGiB`; the launcher grows the disk at the next start.

## Verification

Unit tests and builds pass. In CI the appliance boots to healthy on Linux/KVM (AMD64): the btrfs
template and mounts, the online grow from 256 MiB to 8 GiB, K3s and the Kite console. **Not exercised on a
real cluster:** the volume provisioner (the platform's own workloads use no claims), the snapshot API and
schedules, clone-restore, the restore point on a version change, disk growth through a host request, and the
one-tap update. The full end-to-end suite did not run. macOS, Windows and Linux ARM64 boots are unproven.
btrfs behaviour under a full disk, repeated crashes and database load has not been tested.
Treat this release as an experiment on disposable data.

## Compatibility

None with earlier versions: 0.4.0 needs a new empty state directory, and there is no migration.
