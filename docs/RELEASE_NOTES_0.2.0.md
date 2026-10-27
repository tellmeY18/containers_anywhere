# Containers Anywhere 0.2.0 (beta)

Beta, not production-ready. Production is planned for v1.0.0. Appliance and app are
released independently (`appliance-v0.2.0`, `app-v0.2.0`) and must be used together.

## What changed

- **A smaller, rarely-updated appliance.** The VM image now holds only NixOS 26.05
  (Perl-free, no setuid wrappers), K3s 1.35 with its own system images, and the guest
  agent. Compressed size: amd64 about 945 MB (was 1,002 MB), arm64 about 737 MB
  (was 789 MB).
- **Kite, the gateway and the demo are app-level.** They are embedded in the app,
  pushed to the guest agent on every start and pulled at runtime by pinned digest. A
  new app release updates them without replacing the appliance.
- **One permanent data disk, swappable root.** The data disk is created once and never
  recreated. The guest refuses to start on a data disk that holds cluster data but no
  matching `data-format.json`, and no root image can format it.
- **Root swap with automatic fallback.** `update --operation swap` replaces only the
  read-only root. It copies no data, commits when the candidate is healthy, and falls
  back to the previous root on failure, timeout or crash. It requires an identical K3s.
- **K3s minor is pinned.** A K3s patch release uses the existing full update
  transaction; a minor change and any downgrade are refused.

## Known limits

- **First boot needs internet.** Kite, Traefik and the demo images are pulled at first
  start. There is no offline mode yet; without a connection the launcher reports a boot
  timeout after up to 45 minutes.
- **Update boundaries.** A swap needs an identical K3s version. A K3s patch release needs
  the full transaction (a cold copy of the data disk, manual activate and commit).
  Datastore snapshot/restore for K3s upgrades is not built.
- **Fallback does not rewind data.** It reselects the previous root; the data disk keeps
  anything the candidate wrote. The data-format marker and the identical-K3s rule are
  what make that safe.
- **Retained roots are not pruned automatically.**
- **Filesystem.** The data disk is ext4. The filesystem is permanent once installed; a
  later change means a new state directory.
- **An older app can downgrade the platform workloads** on an existing cluster, since
  every start re-applies the manifests that app embeds.
- **Unverified platforms.** Boot acceptance ran on Linux AMD64 under KVM. Linux ARM64,
  macOS and Windows WHPX are compiled and packaged but not boot-tested. The Windows TCG
  boot test is disabled.
- **Reduced acceptance.** CI runs the essential lifecycle only. The full publication
  suite (permission and admission denials, conflicts, TLS modes, rename, delete) and the
  other wide checks are available behind `smoke.py --extended` but did not run.
- **K3s is not yet pinned to a patch release** (it follows the pinned nixpkgs revision).

## Upgrading from 0.1.x

Existing 0.1.x data is not migrated. The guest refuses to start on a 0.1.x data disk
(it holds cluster data without the format marker). Use a new empty state directory.
Nothing is deleted automatically.
