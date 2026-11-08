# Guest recovery engine

`New(Config)` creates the catalog at `/run/ca/snapshots/recovery`. That mount must
be the persistent `@snapshots` subvolume, outside the restored `/var/lib` tree.
Only the authenticated host agent transport may expose `Engine.Routes`.

## Host/controller contract

- `POST /_agent/recovery/operations`: `{id, action, scope, pointID?}`. `id` is the
  Kubernetes request UID, not its name. `uid` is an equivalent field; supplying
  both requires equality. Actions: `capture`, `restore`; scopes: `system`, `full`.
- `GET /_agent/recovery/operations/{id}`: durable `Operation`. Phases:
  `restart-needed`, `capturing`, `restoring`, `rolling-back`, `completed`, `failed`.
  Identity is in `request.id`/`request.uid`; results include `pointID`, `safetyID`,
  `error`, `createdAt`, and `updatedAt`.
- `GET /_agent/recovery/operations`: `{operations:[...]}`.
- `GET /_agent/recovery/restart-needed`: `{restartNeeded, operations:[...]}`.
  The launcher gracefully restarts the **whole appliance**. The guest never
  stops K3s from inside a workload or attempts a live system capture.
- `GET /_agent/recovery/points`: `{points:[Manifest...]}`.
- `GET|DELETE /_agent/recovery/points/{id}`.
- `GET /_agent/recovery/points/{id}/export`: complete self-contained tar.
- `POST /_agent/recovery/import`: tar body; returns the imported manifest.
  Import only publishes a local point. Activation is a separate restore request.

The serving engine has no `Config.Cold`. Before any boot-time recovery catalog
initialization or ownership changes, `NeedsRestoreResume(root)` reads and
validates the checksummed journals without creating directories or locks.
Ordinary boots, queued operations, and interrupted captures validate the live
format with `initDataFormat` first. Only already-destructive `restoring` or
`rolling-back` phases can run `ProcessCold` before that check: an interrupted
replacement may have removed the live marker. Source/safety snapshots are
validated before replacement, and the recovered live format is checked before
K3s starts. Corrupt or ambiguous journals fail closed before boot-time writes.

Capture failure durably becomes `failed` and leaves the source available for
normal boot. A partially replaced or unrepairable state blocks K3s. Restarting
replays the recorded restore or rollback; the source and safety points are
retained. A corrupt journal fails closed. Results remain outside the datastore
rewind, including when its Kubernetes request no longer exists.

Exports and submissions share a process and OS lock. Export refuses an already
pending cold operation; submission cannot return until an earlier export ends.
The launcher must restart only for the durable queued operations returned by
the polling endpoint.

## Capture and compatibility

System points capture `@system`, SQLite/WAL, token, node password, server TLS and
credentials, plus hashes of PV/PVC/CSI snapshot object mappings and driver state
and an exact volume/snapshot inventory. System-only restore refuses any mapping,
driver-state, or inventory change. This comparison is deliberately conservative
(even object resource-version changes can refuse a system-only restore).

Full points additionally capture the pool metadata subvolume, every CSI volume,
and every CSI snapshot subvolume. Restore replaces both CSI data directories and
driver state and reapplies volume capacity quotas. It does not leave newer CSI
snapshot data behind as untracked orphans. Unknown nested system subvolumes and
driver-state/inventory disagreement are rejected.

Restore requires exact appliance version, architecture, data format, and K3s.
The datastore adapter is versioned independently. Only SQLite is implemented;
etcd and other datastore configurations are rejected. SQLite inspection uses the
appliance `sqlite3` executable. Capturing a safety point currently also requires
a readable/valid current datastore; restoring over an already-corrupt datastore
is refused rather than discarding an unpreserved source.

Consistency is reported as `cold-after-graceful-shutdown` only when the durable
session marker indicates the shutdown unit ran after K3s stopped; otherwise it
is `cold-crash-consistent`. Neither means application consistency or PITR.

## Portable format and workload points

The tar starts with `manifest.json`: `{version, point, members:[{name,size,sha256}]}`.
Every remaining regular member is exactly `<capture>.btrfs`, in declared order.
Import rejects extra/missing members, traversal, links, sparse/PAX entries, bad
sizes, hashes, unsupported versions, or incompatible identities. Complete archive
validation precedes receive, and all receives finish before point publication.
The host must authenticate/decrypt the complete age archive before activation.

Streams are full Btrfs protocol-v1 sends, no incremental parents or outside clone
dependencies. Command CRCs and paths are validated; receive runs in a private
chroot. Device nodes, FIFOs, sockets, and data-less streams are explicitly
unsupported in portable exports/imports. Exports spool streams to determine
sizes and digests; imports stage streams and received subvolumes. Failed staging
is retained and is never listed as a completed point.

`POST /_agent/recovery/workload-snapshots` calls `RegisterSnapshots` with
`{id,members:[{pvcNamespace,pvcName,pvcUID,pvName,volumeHandle,snapshotHandle,
snapshotUID,snapshotContentUID,driver,metadata?}]}`. Driver must be `btrfs.csi.local`.
Handles must match actual ready, read-only snapshots and their source volumes in
the CSI driver's `state.json`, including `pvc-<PVC UID>` and
`snapshot-<VolumeSnapshot UID>` names. `snapshotContentUID` separately records the
bound VolumeSnapshotContent identity; it is not the CSI snapshot name suffix.
The controller supplies completed
resource metadata; arbitrary guest paths are never accepted. These points have
scope `workload` and can use the same export/import routes without a restart.
Their default consistency is **individual** CSI crash consistency. Registration
also accepts `consistency: quiesced-group-hook-contract` from the authenticated
controller after checked group preparation/capture/resume. This preserves the
declared capture contract through export, not a claim of atomic Btrfs group
snapshots or universal application consistency.

`POST /_agent/recovery/workload-restore` calls `PopulateWorkload` with
`{uid,pointID,namespace,namespaceUID,targets:[{sourceSnapshotHandle,
targetVolumeHandle,targetPVCUID,targetPVCName,targetPVName}]}`. The controller
must authorize the namespace/UID, create request-owned fresh PVCs, verify their
bound PV/claim identities, and fence consuming pods. The guest checks the actual
driver identities, original-versus-target separation, empty target subvolumes,
read-only sources, and live mount inventory. It reflink-copies each immutable
snapshot into the new CSI volume without replacing that volume, driver state,
or quota. The source remains retained.

`GET /_agent/recovery/workload-restore/{uid}` reports the durable population
journal. Per-target phases are `pending`, `copying`, `completed`, and `failed`.
Completed retries return their recorded result without copying again. A retry
finding a nonempty interrupted target is refused and preserves its data: create
fresh replacement PVCs with a new request UID. Guest mount checks supplement the
parent's admission fence, which must keep destinations out of use until the
whole population reports completed.
Workload points cannot be used as system/full restore sources.

## Integration and checks

`internal/guest/recovery_linux.go` defines `host.recoveryRoutes(mux)` for Main.
`initData` already invokes cold recovery. Nix must include `internal/recovery`
in the agent source set and `sqlite3`, `btrfs-progs`, and `coreutils` in both
init-data and serving-agent PATH. The existing storage UI routes project the new
catalog and queue cold operations.

Fake-filesystem tests exercise durable UID handling, corruption, complete capture
sets (including nested-subvolume exclusion), mapping refusal, safety retention,
interrupted replacement, failed rollback resumption, stream corruption, and CSI
identity validation. Tests/builds run in CI only. Real Btrfs send/receive, power
loss, reboot, quota, and restore acceptance remain necessary before release.
