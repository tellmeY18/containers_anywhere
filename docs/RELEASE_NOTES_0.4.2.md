# Containers Anywhere 0.4.2 (beta)

Beta, not production-ready. App and appliance both change. **Use this instead of 0.4.0 and 0.4.1,
which are broken on a fresh install.** Like them it needs a new empty state directory; there is no migration. (0.4.0 and 0.4.1 could not run on a fresh install, so replacing their storage types with the standard ones is not a compatibility break for anyone.)

## Storage is now a standard Kubernetes CSI driver, run by the app

The appliance stays a vanilla K3s on a btrfs data disk. Volume management moved out of it:

- **The btrfs CSI driver** ([mporrato/btrfs-csi](https://github.com/mporrato/btrfs-csi), Unlicense) and the standard
  kubernetes-csi sidecars (provisioner, snapshotter, resizer, registrar) run as platform workloads that the app
  delivers on every start, every image pinned by digest. A new app release updates them without replacing the appliance.
- **Standard APIs, so existing tools work unchanged:** PersistentVolumeClaims become btrfs subvolumes with a hard size
  limit (simple quotas); `VolumeSnapshot` and `VolumeSnapshotClass` for snapshots; PVC `dataSource` for restore and
  clone; online expansion by editing the claim; `CSIStorageCapacity` for the scheduler.
- **Copy-on-write per volume,** at creation: the default class `btrfs` keeps it on; `btrfs-nodatacow` turns it off for
  new files (for databases). It cannot be changed on an existing volume. `local-path` is kept as an alias.
- **Replaces** our own guest-agent provisioner and the `PVCSnapshot` / `PVCSnapshotSchedule` types from 0.4.x.
- The pool is a btrfs subvolume bind-mounted at `/var/lib/btrfs-csi/default` by an init container of the driver's
  DaemonSet; the appliance needs no knowledge of the driver. See `docs/STORAGE.md`.

## What was broken in 0.4.0 and 0.4.1, and the fixes (appliance)

Found by booting 0.4.1 on a Mac (Apple Virtualization.framework, arm64) and inspecting the running VM:

- **`/var/lib` was owned by the build user, not root.** `mkfs.btrfs --rootdir` copies the builder's ownership.
  `systemd-tmpfiles` refuses to create anything beneath a directory owned by another user ("unsafe path transition") and
  still reports success, so the K3s links for the airgap images and add-ons were never created. The agent now makes the
  mount roots root-owned before `tmpfiles` runs.
- **A journal-flush / tmpfiles ordering cycle** could make systemd skip `tmpfiles` at boot. Removed.
- **A crash during image extraction left empty binaries** that containerd still trusted (CoreDNS, Traefik and Kite
  crash-looped with `exec format error`). A session marker, removed on a clean shutdown, now makes the agent discard the
  container image cache after an unclean stop. Volumes and the cluster database are never touched.

## Also new

- **One-tap update** in the desktop panel when the app recommends a newer appliance (root swap, automatic fallback).
- **Disk growth** through a host request (`diskGiB`), applied at the next start. The disk starts at 8 GiB (sparse).
- **A restore point** (read-only snapshots of the system and every volume) is taken the first time a different appliance
  version boots. There is no automatic restore yet.
- **Store (proof of concept)** and bare-namespace publication (`care.local`), from 0.3.x.

## Known and unresolved

- **A guest kernel panic seen once** on the Apple VM, 112 s after the first boot: a NULL dereference in
  `__cgroup_bpf_run_filter_skb` receiving an IPv6 multicast packet (arm64 kernel 6.18.57). The guest reboots by itself;
  the trigger is unknown. The image-cache reset makes recovery clean. Not seen on amd64/KVM in CI.
- `df` inside a pod shows the whole pool, not the volume's quota (the quota still enforces). The driver reports a snapshot
  `restoreSize` of 16Ki. Volumes bind on first use (`WaitForFirstConsumer`). No `VolumeAttributesClass`, group
  snapshots or scheduled snapshots yet.

## Verified on real hardware

On the Mac (arm64, Virtualization.framework), by hand: a scrub (clean), 4 KiB random writes with fsync and write-verify
(0 errors), three hard kills of the VM (every fsynced file intact; unsynced files present but empty), then the driver
itself: 19 file operations on copy-on-write and no-COW volumes, quota enforcement, expansion, snapshot, restore and
clone. The integrated 0.4.2 build is covered by unit tests and the CI boot only; the full end-to-end suite did not run.
