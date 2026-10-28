# Containers Anywhere 0.4.2 (beta)

Beta, not production-ready. App and appliance both change. **Use this instead of 0.4.0 and 0.4.1,
which are broken on a fresh install.** Like them it needs a new empty state directory; there is no migration.

## What was wrong, and the fix

Found by booting 0.4.1 on a Mac (Apple Virtualization.framework, arm64) and inspecting the running VM:

- **`/var/lib` was owned by the build user, not root.** `mkfs.btrfs --rootdir` copies the builder's
  ownership into the subvolume root. `systemd-tmpfiles` refuses to create anything beneath a directory owned by
  another user ("unsafe path transition") and still reports success, so the K3s links for the airgap images, the
  publication definitions and the storage definitions were never created: no `WorkloadPublication` type, no
  default StorageClass, and the demo and publications failed to apply. The agent now makes the mount roots
  root-owned before `tmpfiles` runs.
- **A crash during image extraction left empty binaries.** A kernel panic on the first boot (see below) hit
  while containerd was extracting layers. btrfs kept the files but not their data, containerd still trusted
  the layers, and CoreDNS, Traefik and Kite crash-looped with `exec format error` forever. The agent now
  records a session marker, removed on a clean shutdown; if it is still there at boot, the container image
  cache (which only mirrors registries) is discarded and re-downloaded. Volumes and the cluster database are never touched.

## Known and unresolved

- **A guest kernel panic seen once** on the Apple VM, 112 s after the first boot: a NULL dereference in
  `__cgroup_bpf_run_filter_skb` while receiving an IPv6 multicast packet (arm64 kernel 6.18.57). The guest
  reboots by itself; the trigger is unknown and nothing in the appliance configures cgroup BPF filtering.
  The image-cache reset above makes recovery clean. It was not seen on amd64/KVM in CI.

## Verified on real hardware

On the Mac (arm64, Virtualization.framework), with both fixes applied by hand: the cluster reached healthy with
the Kite console up, a PersistentVolumeClaim bound to `containers-anywhere-btrfs` as a btrfs subvolume, a
snapshot became a read-only subvolume, and a clone was independent of the original. The new code itself (not the
hand-applied equivalent) is covered by unit tests and the CI boot only; the full end-to-end suite did not run.
