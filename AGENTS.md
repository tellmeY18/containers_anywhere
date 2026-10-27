# Containers Anywhere agent guidance

Read README.md first. Containers Anywhere is an independent, application-agnostic
K3s appliance. Recovery design is in docs/DISASTER_RECOVERY.md; delivery gates are
in ROADMAP.md. Planned recovery features must not be described as implemented.

- Until the user explicitly declares stability, backward compatibility is not
  a requirement: do not implement, maintain or run legacy/alpha compatibility,
  migration matrices or preservation drills solely for old releases. Prioritize
  fresh 0.1.0 installs and independently released current app/appliance contracts.
  Retain integrity/security checks and non-destructive incompatible-data errors;
  require an explicit reset/new state directory, never delete data automatically.
- Robust current-design updates are necessary and a priority for 0.1.0.
  Preserve independent app/appliance updates, installed-component selection and
  acceptance proving normal updates do not reset data. Do not confuse this with
  obsolete alpha format shims or migration matrices. Production is planned for
  v1.0.0; the user must explicitly declare stability before backward compatibility
  becomes a requirement.
- Small native Go host launcher, downloadable NixOS VM, single-node K3s and Kite.
- macOS uses Virtualization.framework; Linux/Windows use downloaded, hash-verified
  QEMU with KVM/WHPX. No host Docker, Python, Nix or Node runtime dependencies.
- Keep all persistent guest state under `/var/lib` (the data disk), including
  K3s/containerd, node identity, Kite SQLite/plugins/keys and local-path volumes.
- The appliance is a minimal, rarely updated root: NixOS, K3s (with its own airgap
  images) and the guest agent. Kite, the gateway and the demo are platform
  workloads in `platform/`, embedded in the app, pushed to the agent on every start
  and pulled at runtime by pinned digest. Never preload them into the appliance.
  Offline first boot is not yet supported. Handle connectivity failures. No runtime
  language package installation.
- The K3s minor is pinned in nix/guest.nix (`pkgs.k3s_1_35`); a minor change needs
  a tested upgrade path. Root-only changes use `update --operation swap`: it needs an
  identical K3s, copies no data and falls back to the old root automatically. A K3s
  patch release uses the full update transaction. The data disk carries a
  `data-format.json` marker that the guest checks before anything writes to it, and no
  root image may format it. BEFORE PRODUCTION (v1.0.0): pin the ext4 mkfs features of
  the data disk template, decide the data-disk filesystem (permanent once installed),
  and replace the cold-copy transaction with guest-side datastore snapshot/restore.
- Host capabilities offered to workloads go through the generic host API
  (`internal/hostapi`, `platform/hostapi.yaml`, docs/HOST_API.md): read-only status
  plus approval-gated requests, approved only in the launcher, with no host token ever
  handed to a pod. Do not add per-application endpoints or defaults.
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
- Until the production gate, default CI skips the full end-to-end lifecycle (it was
  over half of the slowest job). Every run still builds, packages and boots the
  appliance once until healthy. Run `appliance.yml -f e2e=true` (add `-f extended=true`
  for the wider checks) before anything production-bound, and state in the release notes
  whether it ran. `app.yml` always boots the published bytes of a release.
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
nix/guest.nix (appliance), platform/ (Kite/gateway/demo manifests), cmd/guest and internal/guest (guest agent),
scripts/package.py and app.yml/appliance.yml/release.yml (independent releases).
