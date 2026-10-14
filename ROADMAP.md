# Containers Anywhere reliability roadmap

Status: architecture agreed for planning; the recovery features below are not
shipped unless explicitly marked available. No CARE-specific work is included.

## Available: alpha foundation

- Small desktop launcher, pinned resumable downloads, K3s and Kite appliance.
- Separate control/browser capabilities and loopback-only host listeners.
- Persistent data disk, exclusive lifecycle lock and manual encrypted cold backup.
- Empty-destination restore and exact-appliance compatibility checks.
- Linux AMD64 KVM boot/restart/restore acceptance; Windows emulated boot/restart.

## R1 — Prove and inspect recovery points

- Backup manifest: identity, timestamp, disk geometry, checksums, format and tool
  versions; preserve backward readability of published alpha backups.
- Standalone authenticated backup verification without activation; distinguish
  archive integrity from a successful application restore.
- Durable staging/publication journal, interruption cleanup and capacity checks.
- Recovery kit with exact appliance/runtime, launcher, recovery instructions and
  key export verification, usable without the original GitHub release.
- Automated isolated restore drill validating cluster identity, Kite login and a
  non-regenerable workload data marker.

Gate: process termination, disk-full and corrupted/truncated archives at each
publication boundary never overwrite data or report a partial backup as complete.

## R2 — Automatic off-device protection

- Explicit backup destination enrollment and tested key recovery.
- Scheduled cold recovery windows first; show expected downtime and missed runs.
- Encrypted incremental off-device repository using an existing backup engine,
  selected after benchmarking (restic is a candidate, not a committed dependency).
- Retention, repository integrity checks, restore drills and age/lag alerts.
- Optional immutable object storage with separate writer and retention credentials.

Gate: network outage, interrupted uploads, remote-full, lost credentials, host
replacement and ransomware-style local deletion; last good remote backup survives.

## R3 — Application-consistent online backup and PITR

- Explicit opt-in protection profiles for supported workloads; no implicit claims
  about arbitrary containers or generic copies of live databases.
- Start with PostgreSQL base backups plus continuous WAL archiving through a
  maintained integration. Evaluate CloudNativePG/Barman and pgBackRest rather than
  writing another WAL backup implementation.
- Display earliest/latest recoverable timestamps and log gaps, not just job success.
- Restore to a new isolated workload, validate, then explicitly cut over.
- Define coordinated recovery for database/object-store pairs and multiple services.

Gate: insert/update/delete history, recover-before-delete, missing WAL, timeline
changes, failed base backup and key loss. Restore must reach the requested point,
prove business-level consistency and never overwrite the source workload.

## R4 — Host failure and safe upgrades

- Physical macOS, Windows WHPX and Linux ARM64 acceptance.
- Power-loss, launcher crash, suspend/wake, network change, clock jump and
  low-storage testing; bounded diagnostics and guest storage-pressure reporting.
- Platform-supported startup/supervision with explicit ownership and graceful stop.
- Signed release metadata, platform signing, compatibility checks, pre-upgrade
  recovery point and a durable upgrade journal. Migration-specific rollback rules.

Gate: restore onto a replacement host; recover after interruption at every upgrade
phase; retain the original disk whenever compatibility or recovery is uncertain.

## In parallel — Minimal appliance

Follow [APPLIANCE.md](docs/APPLIANCE.md): closure inventory, remove Python-only
secret generation, prune unused preloads, measure duplicate storage, then optimize
kernel/userspace. A smaller image must pass the same recovery gates.

## Release evidence

Each release records tested platforms, failure scenarios, restore duration,
last successful drill and measured recovery-point age. RPO (maximum data loss)
and RTO (recovery time) are deployment objectives, not global promises. Performance
and restore claims include workload size, hardware and backup destination.
