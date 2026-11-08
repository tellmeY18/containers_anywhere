# Recovery API controller integration

This package is app-delivered controller logic, with no Kubernetes client library
dependency. It is not a guest service or a new container image. Apply
`platform/recovery.yaml` alongside the existing platform snapshot resources.

## Host bridge contract

```go
controller := &recoveryapi.Controller{
    Dir: filepath.Join(stateDir, "recovery-api"),
    Run: kubeRunner,
    Backend: clusterBackend,
    HostDestination: destinationEnrollment,
    Export: exportPoint,
    RestoreImported: restoreImportedPoint,
    Prune: pruneRegisteredGuestPoint,
}
// Invoke periodically while the guest is reachable. Reconcile serializes calls.
err := controller.Reconcile(ctx)
```

- `Runner` is `func(context.Context, []byte, ...string) ([]byte, error)`. Forward
  arguments unchanged to authenticated `POST /_agent/recovery/kube` using
  `{args: [...], input: string(input)}`; return `output` and turn `error` or non-2xx
  responses into Go errors. Preserve `--as`, namespace flags, JSON stdin and `--`
  verbatim; never shell-expand. Arguments start with the normal verb; impersonation
  flags follow that verb, before the command's remaining arguments. Pruning uses
  `delete --raw=<resource-url> -f -` with a JSON DeleteOptions body containing exact
  UID and resourceVersion preconditions; preserve this body too. The controller reads namespace/PV/snapshot-content
  metadata and reconciles its own CRs with controller authority. Every workload
  PVC/snapshot/config read/create and pod exec uses namespace impersonation.
- `Backend` has `Submit(context.Context, ClusterOperation) (OperationStatus,error)`
  and `Get(context.Context, string) (OperationStatus,error)`. Operation fields:
  `UID`, `Action` (`Capture` or `Restore`), `Scope` (`system` or `full`), `RecoveryPointID` (restore source), and
  `Destination` (optional CR name, not a path). A successful capture must return
  `RecoveryPointID`. Accepted phases: `Pending`, `Running`, `RestartRequired`,
  `Succeeded`, `Failed`. The parent adapter owns authenticated guest queue calls,
  compatibility checking and `triggerPower("restart-appliance")` when needed.
  Backend submission **must deduplicate UID durably**; calls can repeat after a
  transport failure. Host records persist before submission and after responses.
  Nonterminal journaled operations continue to be polled even if a cold restore
  removes their Kubernetes CRs from the restored datastore.
  Map these to the engine request `{uid, action: "capture"|"restore", scope:
  "system"|"full", pointID}`. For restore, verify the imported/local immutable
  manifest has the requested scope and pin the source before queuing any action.
- `HostDestination` is
  `func(context.Context, string) (DestinationStatus,error)`; the string is the
  enrolled opaque HostDirectory ID. Return a `Ready` condition with `True`,
  `False`, or `Unknown`, plus a non-secret reason/message. Return no host paths.
- `Export` is
  `func(context.Context, Record, Destination) (ExportStatus,error)`. `Destination`
  contains `Name`, `UID`, and `Spec`; `Spec.HostDirectoryID` is the enrollment ID.
  `Record` includes completed member `PVC`, `UID`, `VolumeName`, `VolumeHandle`,
  `Snapshot`, `SnapshotUID`, `Content`, `ContentUID`, `SnapshotHandle`, requested `Storage`,
  access/volume modes, consistency, and optional ConfigMap capture. Snapshot
  handles are only accepted after validating the CSI driver, source volume
  handle, retained content and exact VolumeSnapshot UID reference. Register this
  workload set with the guest engine, which must independently verify driver
  state and snapshot identity before accessing data, then use normal encrypted
  export. Cluster records instead contain the guest `RecoveryPointID`.
  Deduplicate by `(Record.UID, Destination.UID)`. Return `Pending`, `Running`,
  `Failed`, or `Succeeded`; success requires a nonempty opaque `Receipt` after
  archive integrity/authentication has completed. Failures are retried and keep
  `ExportPending=true`. Local success and export success are separate statuses.
  The backend capture callback should not independently export a destination;
  this export callback owns the completion/retention gate.
- `RestoreImported` is
  `func(context.Context, ImportedRestoreRequest) (ImportedRestoreStatus,error)`.
  A namespaced RestoreRequest can specify `durablePointID` instead of the local
  `recoveryPoint`/`recoveryPointUID` pair. The callback receives `RequestUID`,
  `Namespace`, `NamespaceUID`, `ServiceAccount` (fixed `recovery-workload`),
  `PointID`, and `PVCNames`. It must load the authenticated imported inventory,
  reject cluster-scope points, verify that every member belongs to the requesting
  namespace and that mapping membership is exact, and durably protect the source.
  Create new request-owned PVCs with the supplied namespace identity (impersonate
  `system:serviceaccount:<Namespace>:<ServiceAccount>`), never node-admin workload
  authority. Before populating, verify each actual bound CSI volume is a fresh,
  empty, unconsumed target owned by this request and never overwrite an existing
  claim's data. Recheck namespace UID and source/target driver identities at every
  mutation. The guest engine owns idempotent populate transactions; deduplicate
  `RequestUID` durably and retry only targets owned by the same transaction.
  Return `Pending`, `Running`, `Succeeded`, or `Failed` with `RestoredPVCs`; success
  requires the exact complete mapping. The host controller journals before the
  callback and terminal UIDs never re-execute, even after a guest rewind. A nil
  adapter reports `Pending/NotImplemented`; this is not an implemented import
  engine by itself. Remote-provider imports remain unsupported.
- `Prune` is `func(context.Context, Record) error`. It removes the registered
  guest point (`Record.RecoveryPointID` for cluster points; `workload-<Record.UID>`
  for exported workload registrations). Treat an absent registration as success,
  reject active restore/export dependencies, and durably deduplicate retirement
  by point identity. For cluster points, recheck the actual guest catalog and
  refuse deletion of its last live complete point in that scope: a newer host
  operation result alone cannot prove its point survived a guest rewind.
  This callback must not remove CSI objects or arbitrarily
  delete their handles: the controller owns the separate verified CSI lifecycle.
  It runs after the host retirement intent is durable and before CSI mutation;
  returning an error pauses deletion. A nil callback blocks physical pruning.
- `Now` is an optional clock; `Store` optionally replaces `Dir` for adapters/tests.
  The file store uses existing private atomic-file writes and keeps a record per
  UID. Its host-side path must never be within the rewound guest data disk. The
  launcher must allow only one controller process to own that state directory.

Capture may take up to 300 seconds; checked resume is given an independent
10-minute cleanup context even when the caller's context is cancelled. Individual
hooks have 1–30 second timeouts. The host loop and HTTP transport must preserve
those deadlines and allow cancellation to reach the guest subprocess. A failed
reconcile should be logged/retried, not cause deletion or a journal reset.

## Authorization boundary

Installing the platform manifest creates **no workload grants or bindings**.
Administrators explicitly opt a namespace into recovery using
`docs/examples/recovery-rbac.yaml`. All workload operations impersonate the fixed
`system:serviceaccount:<namespace>:recovery-workload` identity. There is no
request-selectable service account and no fallback to admin credentials.
`pods/exec` is granted only for explicit pod names in the example. Creating a
RecoveryPoint that contains hooks delegates arbitrary commands against those
authorized pods: grant recovery request access only to equally trusted subjects.
Kubernetes author identity cannot be reconstructed from a CR, so the designated
service account's explicit Role is the authority boundary, not the CR author's
unrecorded credentials. Remove exec rules entirely for crash-consistent capture.

Namespaced CRs cannot request cluster restore or target another namespace. Pod
UIDs and the namespace UID are journaled and checked before prepare/resume/check;
replacement identities fail closed. Kubernetes exec does not offer a pod-UID
precondition, so a pod replacement between the check and exec is not an atomic
operation; RBAC still restricts it to the explicitly delegated pod name/namespace.

BackupDestination is cluster-scoped. An administrator must explicitly list every
permitted namespace in `spec.allowedNamespaces`; an empty list authorizes only
cluster requests. Export rechecks that list, the namespace UID and the destination
UID every time. The controller never reads destination credential Secrets.
Remote providers (`S3Compatible`, `AzureBlob`, `GCS`, `SFTP`, `RepositoryService`)
always report `Ready=False, reason=NotImplemented`; no remote export callback runs.

## Supported workflow and semantics

Cluster capture and policy specs require explicit `scope: system` or `scope: full`.
Namespaced specs reject this field. Exact cluster request JSON examples:

```json
{"scope":"system"}
{"scope":"full","schedule":"0 3 * * *","keepLast":3}
{"scope":"full","durablePointID":"imported-engine-point"}
{"scope":"system","recoveryPoint":"local-point-cr","recoveryPointUID":"original-cr-uid"}
```

The first is a ClusterRecoveryPoint, the second a ClusterProtectionPolicy, and
the last two are ClusterRestoreRequest specs. Direct `recoveryPointID` is also
accepted in place of `durablePointID`; only one source form is allowed. Imported
cluster restore requires no original capture CR or host catalog entry. It uses
`Backend`, while namespaced imported restore continues to use `RestoreImported`.

- Workload capture selects an **exact** named, label-matching set of Bound local
  btrfs PVCs (up to 64). Membership/UIDs are persisted before hooks and rechecked
  before every CSI snapshot. Capacity comes from the source PVC request, not the
  driver's currently unreliable `restoreSize`.
- `PerPVC` is crash-consistent per volume. `QuiescedGroup` runs bounded generic
  prepare hooks, snapshots sequentially while writes are held, then runs resume
  **and** check commands. It is not atomic group capture or automatic proof of
  application consistency. Hooks must cover all relevant writers and resume must
  be idempotent. Every attempted prepare is journaled before exec. Resume failures
  remain `ResumeFailed` and retry independently of the original CR's existence.
- Process interruption during capture never reruns prepare/capture for that UID.
  It resumes every attempted hook, marks the partial set failed, and retains it.
  A new request UID is required for another capture attempt.
- New requests publish a CAS-protected `Prepared` status before any side effects.
  On a replacement host with no `recovery-api` journal, historical CR status is
  evidence against replay. Workload requests become `Failed/HostJournalMissing`,
  retain cataloged identities, and are protected from automatic pruning. An
  in-progress/uncertain historical capture sets `ResumeUncertain`, blocking new
  prepares in that namespace until an operator independently verifies resume and
  explicitly resolves that host-journal flag. Never clear it merely to retry.
  Historical cluster requests use `Backend.Get` only; a proven terminal result
  may be recorded conservatively, with retention protection. A recovered capture's
  Ready condition remains Unknown because an operation result alone does not
  verify the current point data. No historical request
  is automatically resubmitted when the guest has forgotten its UID.
- Restore creates **new PVC declarations** from the recorded VolumeSnapshots,
  reports a source-to-new-name mapping and protects the source before creating
  anything. Repeated request UID/spec pairs never submit another completed
  restore, even when Kubernetes status was rewound. `NewPVCsCreated` means the
  declarations exist; `WaitForFirstConsumer` can defer provisioning/binding. Data
  verification, consuming pods and application/operator cutover are explicit.
  A historical successful capture does not prove its CSI handles still exist:
  after a cluster rewind, status checks current snapshot/content identities and
  reports `Ready=False/LocalSnapshotUnavailable` when they differ or are missing.
  Local restore independently rechecks those references and fails explicitly.
  Already exported backups remain independently usable through durable import.
- Optional selected ConfigMaps are saved in the host catalog/export metadata.
  They are not reapplied automatically; Secret, RBAC and arbitrary operator
  configuration capture is not supported. Config reads are sequential, not an
  atomic API-state snapshot.
- Five-field numeric cron supports ranges/lists/steps, standard restricted
  day-of-month/day-of-week OR semantics, UTC only, and a conservative minimum
  15-minute interval. Each reconcile coalesces missed slots to one within the last
  24 hours; policy UID/slot names and a host-side cursor prevent clock/rewind
  replay. Schedules are not a guarantee of capture while the app/guest is offline.

## Retention and deletion

`keepLast` actually retires eligible local points through the host-side deletion
journal. It preserves failed/partial sets, the newest successful sets independently
per scope/namespace identity, pending exports, and restore-referenced/historical
sets. Protected references are intentionally not removed automatically. Restore
requests are reconciled before policy retention so their source pins are recorded.
Workload sets count toward keepLast only while their original retained content
identities and readiness still verify; a missing post-rewind set cannot displace
the last surviving local copy. An administrator may call
`Controller.PruneEligible(ctx, uid)` for an independently cataloged orphan; it
rechecks recorded eligibility, current retained identities and last-set protection.

Retirement first verifies all CSI catalog identities and checks live PVC snapshot
references. It records `Pruning` before invoking the guest prune callback. Next it
deletes each exact VolumeSnapshot with DeleteOptions UID/resourceVersion checks,
leaving its content `Retain` until the snapshot and protection finalizers are gone.
Then it durably arms the content transition, JSON-patches only the exact original
content UID/resourceVersion/source volume handle/snapshot UID/snapshot handle from
`Retain` to `Delete`, and sends another preconditioned DELETE. CSI performs backend
cleanup; no finalizers are forcibly removed. Failures pause and retry from the
journal. Final `Pruned` tombstones remain permanently, report Ready=False, and
prevent old restored CRs from triggering another capture or deleting replacement
objects. Filesystem space reclamation after CSI deletion may be eventual.

The dedicated `recovery-btrfs` VolumeSnapshotClass has `Retain`. Snapshots have no
owner references to policies/requests. Deleting a namespace leaves retained CSI
content and the independent host catalog. Policy retention can safely delete that
orphaned content by its exact recorded identity, without accessing replacement
namespace objects. Restoring those orphaned contents into
a new namespace requires administrator-driven static snapshot import; the
controller refuses same-name namespace replacements. Static orphan import remains
administrator-driven. Local snapshots still share the appliance disk failure domain.

The focused Go tests are intended for CI. No local builds/tests or platform boot
acceptance are implied by this implementation.
