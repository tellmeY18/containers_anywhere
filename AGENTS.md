# Containers Anywhere agent guidance

Read README.md first. This branch intentionally pivots CARE Anywhere into a
K3s-based container appliance; the former "no containers" constraint is superseded.

- Small native Go host launcher, downloadable NixOS VM, single-node K3s and Kite.
- macOS uses Virtualization.framework; Linux/Windows use downloaded, hash-verified
  QEMU with KVM/WHPX. No host Docker, Python, Nix or Node runtime dependencies.
- Keep all persistent guest state under `/var/lib` (the data disk), including
  K3s/containerd, node identity, Kite SQLite/plugins/keys and local-path volumes.
- Preload K3s/Kite at build time. Workloads may pull images at runtime; handle
  connectivity failures. No runtime language package installation.
- Never expose unauthenticated management or bind host listeners to the LAN.
  Preserve vsock/per-boot mTLS and separate host control/browser capabilities.
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
  Push only when authorized. Preserve main as the original CARE product history.

Key files: main.go (host CLI/proxy), desktop.go (embedded bootstrap panel),
bootstrap.go (pinned resumable downloads), storage.go (disk/backup), vm_*.go,
nix/guest.nix and nix/kite.* (appliance), scripts/package.py and alpha.yml (release).
