# Containers Anywhere 0.5.0 (beta)

Beta, not production-ready. App and appliance both change. This release needs a new empty state directory; there
is no migration from earlier releases.

## Experimental Kubernetes-managed recovery

Recovery is managed through Kubernetes resources and stored on the btrfs data disk. See [RECOVERY.md](RECOVERY.md).

- **System-level recovery points** for the K3s datastore (SQLite) and full-guest cold capture/restore.
- **PVC and group recovery** using CSI snapshots, with `ProtectionPolicy` scheduling, retention and capacity checks.
  `RecoveryPoint` and `RestoreRequest` always restore into a new PVC; they do not rewind the source.
- **Independent encrypted host-directory exports** stored outside `data.img`, with import into a fresh image.
  Tampered or truncated archives are refused.
- `HostStatus`, `HostRequest` and `HostRecoveryPoint` expose recovery state and launcher-approved requests.
- `BackupDestination` cloud adapters are placeholders and return NotImplemented.

## Limits

- Exports are full exports, not incremental.
- The datastore adapter boundary is ready for etcd, but only SQLite is supported.
- This is discrete recovery history, not database transaction-log PITR. Application-consistent recovery for
  PostgreSQL, MySQL and object stores has not been qualified.

## Verification

- The appliance was built with `recovery=true`. The Linux AMD64/KVM drill passed CSI and CRD restores,
  host export/import, system and full restores, refusal of tampered archives, and restore into a fresh image without
  the original `data.img`.
- The `extended` checks were not run.
- Native vet and race tests pass on macOS and Windows. VM boot and recovery on Virtualization.framework and WHPX
  hardware have not been verified.
