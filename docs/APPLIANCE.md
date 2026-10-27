# Minimal appliance engineering

Goal: smallest dependable Linux/K3s foundation across the supported virtual
hardware. Optimization must preserve networking, durable storage, graceful stop,
entropy, TLS, timekeeping and diagnosability. Recovery tools should be added only
where needed, not all baked into every consumer VM by default.

## Baseline

Alpha 49 compressed appliances: ARM64 789,236,297 bytes (789.2 MB); AMD64
1,002,270,998 bytes (1,002.3 MB), down from alpha 42's 877.2 MB and 1,091.6 MB.
Both contained NixOS, K3s/containerd and airgap images, the preloaded Kite image and
guest agent (Kite, the gateway and the demo are no longer preloaded; they are
platform workloads pulled at runtime, so these sizes are stale until re-measured). Defaults: 2 vCPUs, 4 GiB RAM, sparse 16 GiB data disk. These are
configuration defaults, not measured idle consumption or minimum requirements.
Guest runtime closures are 1.50 GB (ARM64) and 1.31 GB (AMD64) of uncompressed
Nix store bytes across 372 paths each.

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

Measured candidates from the alpha 49 closure inventory, largest first. Sizes are
uncompressed store bytes on ARM64; the appliance archive is already compressed.

| Candidate | Size | Verdict |
| --- | ---: | --- |
| Prune disabled airgap images (Traefik 49.4 MB, metrics-server 20.9 MB, klipper-lb 5.8 MB) | 76.1 MB | Do next. All three addons are disabled with `--disable` |
| Trim kernel module set (kernel 196.6 MB, modules 131.7 MB, initrd 23.3 MB) | 351.6 MB | Largest remaining lever. Needs a kernel build change plus the full platform matrix |
| Perl plus `gcc-lib` pulled by NixOS user activation (`setup-etc.pl`, `update-users-groups.pl`) | 78.2 MB | Investigated: referenced directly by the toplevel. Removing it means replacing NixOS user setup; deferred as too risky for an appliance |
| Thick K3s binary `k3s-bin` plus `k3s` wrapper | 387.6 MB | Required by upstream K3s. Only a custom K3s build would change this, which APPLIANCE.md forbids |
| `util-linux` full binaries (minimal variant already present) | 14.2 MB | Switch the shell environment to `util-linuxMinimal`; verify ext4 tools remain available |
| suid wrappers dragging in `linux-headers-static` | 7.4 MB | Drop setuid programs from the shell set; confirm nothing needs them |
| `klipper-helm` image | 67.8 MB | Required: Kite installs Helm charts, and klipper-helm runs them offline |
| `rdma-core` via libpcap via iptables | 6.6 MB | Small; only worth it if a firewall rebuild drops libpcap |

1. Verify the Go secret-generation change removes Python from the runtime closure;
   run fresh boot/restart/restore acceptance and compare both architecture sizes.
2. Prune the three disabled airgap images at build time, keeping CoreDNS, pause,
   local-path-provisioner, klipper-helm and busybox. Gate on cached/offline start,
   workload deployment, graceful reboot and backup/restore.
3. Re-measure both architectures and keep the largest-path report in CI so each
   further reduction is attributable.

## Acceptance

Each reduction repeats fresh boot, cached/offline startup, workload deployment,
graceful reboot and backup/restore with preserved keys/data. Architecture-specific
checks include DNS, CNI, storage mounts, entropy and encrypted management transport.
Repeat real hardware acceptance when changing kernel or VM devices. Set size and
idle-resource budgets after profiling; CI should flag regressions against them.
