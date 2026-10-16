# Minimal appliance engineering

Goal: smallest dependable Linux/K3s foundation across the supported virtual
hardware. Optimization must preserve networking, durable storage, graceful stop,
entropy, TLS, timekeeping and diagnosability. Recovery tools should be added only
where needed, not all baked into every consumer VM by default.

## Baseline

Alpha 42 compressed appliances: ARM64 877,226,807 bytes; AMD64 1,091,648,048 bytes.
Both contain NixOS, K3s/containerd and airgap images, the preloaded Kite image and
guest agent. Defaults: 2 vCPUs, 4 GiB RAM, sparse 16 GiB data disk. These are
configuration defaults, not measured idle consumption or minimum requirements.

Track independently: compressed bytes, extracted bytes, actual allocated disk,
peak install/upgrade/restore space, cold/warm boot time, host VM RSS, guest idle
memory, CPU and background writes. Publish hardware and measurement procedure.

## Order of work

### First slimming pass (pending build and boot acceptance)

The guest now uses the existing Go agent for first-boot Kite secrets instead of
shipping Python for that task. It reads the same persisted JSON keys, rejects
corrupt keys without regeneration, and atomically publishes private files using
the existing fsync/rename helper. Python remains a build-time dependency.

The configuration disables LVM, bcache, kexec, nano, pager integration and
fontconfig. The generic interactive core package list is replaced with Bash,
coreutils, findutils, gawk, grep, sed, procps and util-linux. Service-declared
dependencies, ext4 checking/resizing, networking, timekeeping, TLS trust,
systemd/journal diagnostics and all current preloaded images remain required.
Removing a package from the shell environment does not prove it disappeared from
the transitive runtime closure.

EROFS retains LZ4HC and tail packing but drops block deduplication, which forced
single-threaded construction in the pinned microvm builder. This is a build-speed
tradeoff: measure the resulting image size before accepting it. Each architecture
now uploads its guest closure inventory and reports file sizes and the 30 largest
store paths in the Actions summary. See [the CI reconnaissance](CI_PERFORMANCE.md).

### Next reductions

1. Inventory closure sizes and image layers by architecture. Identify retained
   build tools, redundant packages, firmware, locales and modules.
2. Verify the Go secret-generation change removes Python from the runtime closure;
   run fresh boot/restart/restore acceptance and compare both architecture sizes.
3. Audit K3s airgap image contents. Disabled Traefik/ServiceLB/metrics-server may
   still occupy archive space. Prune only after proving CoreDNS, local-path helper
   pods and recovery paths work without registry access.
4. Measure archive/containerd duplication. Moving preloads outside the system image
   can improve reclaimability and update granularity; it does not automatically
   reduce total first-launch bytes. Never garbage-collect the only offline recovery
   copy of a required image.
5. Separate optional management/recovery payloads from the minimal base if measured
   savings justify the extra lifecycle. Kite remains the current management UI.
6. Tune kernel and userspace for virtio, KVM/WHPX and Virtualization.framework only
   after a platform test matrix exists. Prefer upstream K3s packages to a custom
   Kubernetes fork. Do not trade recoverability for a headline image size.

## Acceptance

Each reduction repeats fresh boot, cached/offline startup, workload deployment,
graceful reboot and backup/restore with preserved keys/data. Architecture-specific
checks include DNS, CNI, storage mounts, entropy and encrypted management transport.
Repeat real hardware acceptance when changing kernel or VM devices. Set size and
idle-resource budgets after profiling; CI should flag regressions against them.
