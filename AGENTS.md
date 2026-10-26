# Containers Anywhere agent guidance

Read README.md first. Containers Anywhere is an independent, application-agnostic
K3s appliance. Recovery design is in docs/DISASTER_RECOVERY.md; delivery gates are
in ROADMAP.md. Planned recovery features must not be described as implemented.

- Small native Go host launcher, downloadable NixOS VM, single-node K3s and Kite.
- macOS uses Virtualization.framework; Linux/Windows use downloaded, hash-verified
  QEMU with KVM/WHPX. No host Docker, Python, Nix or Node runtime dependencies.
- Keep all persistent guest state under `/var/lib` (the data disk), including
  K3s/containerd, node identity, Kite SQLite/plugins/keys and local-path volumes.
- Preload K3s/Kite at build time. Workloads may pull images at runtime; handle
  connectivity failures. No runtime language package installation.
- Management listeners remain loopback-only and authenticated. Workload gateways
  may bind configured LAN interfaces; never implicitly expose Kubernetes, Kite,
  Caddy administration or launcher control. Preserve vsock/per-boot mTLS and
  separate host control/browser capabilities. Advertise only reachable endpoints.
- Never weaken bundle hashes, download pins, archive traversal/link rejection,
  data locks, refuse-overwrite backups or empty-destination restore.
- VM changes must remain buildable on Darwin ARM64, Linux ARM64/AMD64 and Windows
  AMD64. Never silently fall back to software emulation.
- Existing cluster data requires its exact original appliance in this MVP.
  An upgrade path needs K3s migration and recovery acceptance before activation.
- Check CARE Clinic's filesystem/OS helpers before reinventing them. Preserve
  attribution in THIRD_PARTY_NOTICES.md for reused atomicfile/diskspace code.
- Release builds, compilation tests and packaging run in GitHub Actions.
  Format with gofmt; run go vet and go test -race in CI. Changes to lifecycle or
  storage also require the real smoke test with reboot and backup/restore.
- Do not claim platform boot acceptance from compile/packaging checks.
- Do not commit dist/, .state/, *.img, node_modules or downloaded artifacts.
- This is a jj repository. Use jj bookmarks and noninteractive --no-pager commands.
  Push only when authorized. The default branch is develop (the general platform); care-mvp is the CARE-bundled line. Preserve
  archive/care-anywhere as the original CARE product history; do not rewrite it.
- Whole-disk recovery points and cluster datastore backups are not workload PITR.
  PITR claims require tested application-native logs, base backups, keys and a
  verified recovery window. Restore drills must isolate outbound side effects.

Key files: main.go (host CLI/proxy), desktop.go (embedded bootstrap panel),
bootstrap.go (pinned resumable downloads), storage.go (disk/backup), vm_*.go,
nix/guest.nix and nix/kite.* (appliance), scripts/package.py and alpha.yml (release).
