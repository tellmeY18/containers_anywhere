# Kubernetes-managed recovery

Status: implemented in source; CI build and recovery acceptance are pending.
Build, boot, interruption and restore acceptance must pass before release. This is discrete recovery history, not
database transaction-log PITR. No platform durability SLA is asserted.

## Two independent dimensions

Recovery **scope** and backup **location** are different choices:

| Scope | Capture | Restore |
| --- | --- | --- |
| Workload | One PVC or a declared related PVC set | New PVCs; original volumes remain intact |
| System | K3s datastore, identity, keys and required platform/storage metadata | Cold system restore with volume identity checks |
| Full appliance | System state and the persistent volume/snapshot inventory | Cold coordinated restore |

| Location | Purpose | Failure boundary |
| --- | --- | --- |
| Btrfs data disk | Fast snapshots, clones and stable export sources | Shares the live disk's failure domain |
| Host-directory repository | Independent encrypted recovery sets outside `data.img` | Survives loss of the guest disk image; same-host physical disk loss still affects it |
| External/remote repository | Off-device recovery | Depends on the destination; remote transport adapters are placeholders |

## Ownership

The host app owns the recovery controller and ships the Kubernetes CRDs in
`platform/recovery.yaml`. It reconciles over the existing authenticated guest
transport. This permits app-only scheduling/policy updates without adding a new
container image or baking a recovery controller into the appliance.

The appliance owns Btrfs support, a small durable executor and the pre-K3s recovery
step. The standard CSI driver continues to own workload volumes and CSI snapshots.
Ordinary workloads receive neither host credentials nor a guest command socket.

The private controller transport is not the workload-facing API. Workloads use the
Kubernetes API and namespace RBAC. Cluster-level operations require cluster-level
permissions. Host directory enrollment is performed by the computer's owner in the
launcher/CLI; Kubernetes references opaque destination IDs, never host paths.

## Local layout

- CSI PVC snapshots: `/var/lib/btrfs-csi-pool/snapshots`, owned by the CSI driver.
- System/full recovery catalog and immutable points: the persistent `@snapshots`
  subvolume mounted at `/run/ca/snapshots`.
- The executor's journal lives outside the system subvolume being rewound.
- The app controller's journal lives in host cluster state, outside `data.img`.
- Exported recovery sets carry their own manifests and inventory in the enrolled
  host repository. They do not depend on the original guest catalog to be found.

`/run/ca/snapshots` is a persistent mount despite its name. All native snapshots
still consume the same underlying `data.img` capacity as live data. A native
Btrfs snapshot cannot be moved to another filesystem while retaining its shared
extents: export creates an independent copy.

## Consistency

Btrfs subvolume snapshots are not recursive. Taking several snapshots at once
through separate CSI requests does not establish a common multi-volume instant.

Individual live snapshots are labelled crash-consistent per volume. A coordinated
workload checkpoint needs all declared writers quiesced throughout capture.
Preparation and resume operations must be bounded and journaled; recovery must
resume writers even if a hook or capture fails. A failed member prevents publication
of a complete recovery set. An operator-managed workload is responsible for a
preparation mechanism its own reconciliation will respect.

System/full capture is performed at boot before K3s or workload containers start,
after a durable request and appliance restart. It captures the persistent state
left by shutdown. An unclean shutdown cannot be promoted to application consistency.
The storage does not include process memory or external service state.

An etcd snapshot, if supported in a future backend, will still contain Kubernetes
objects rather than PVC contents. The current supported datastore is SQLite.
Manifests record the backend and compatibility information; unsupported backends
must fail explicitly. SQLite-to-etcd conversion is a separate future lifecycle
operation, not an implicit restore transformation.

## Restore and interruption

Workload restores create new PVCs and publish a source-to-destination mapping.
Starting a recovered application and changing its traffic or operator configuration
is a separate step. Run validation workloads in isolation so email, webhooks,
payments and external writes cannot fire during a drill.

System restoration requires the guest executor because Kubernetes cannot run while
its own datastore is being replaced. Requests/results are durable outside that
datastore, including when a restored database predates the request. The launcher
remains the recovery entry point when the Kubernetes API is unavailable.

Before a destructive system restore, preserve a safety point and verify that the
selected point and appliance form a supported pair. A system-only restore must
not start against changed volume bindings or incompatible driver metadata.
Interrupted restoration resumes from its journal; an ambiguous partially restored
state must prevent cluster startup rather than be accepted as healthy.

## Host backups and remote extension

The first host repository format uses self-contained, age-encrypted full exports
of completed recovery points. It reuses age rather than introducing a new
cryptographic format. This initially costs more I/O and space than incremental
repository backups, but each exported recovery set is independent of the source
snapshots. Incremental/deduplicated export is a future engine integration, not a
current capability.

The recipient used for unattended encryption is distinct from the private recovery
identity. Keep an independent recovery-key copy outside both the guest and backup
repository. Restore verifies the encrypted stream before publication/import. A
truncated export is never listed as a completed backup. Missing removable media
must produce an unavailable destination, not a new directory on the system disk.

`BackupDestination` reserves adapters for S3-compatible storage, Azure Blob, Google
Cloud Storage, SFTP and repository services, in addition to enrolled host folders.
Remote adapters report `NotImplemented`; an endpoint declaration is not evidence
that any backup has been uploaded or can be restored.

Local capture, export and restore validation are separate states. A local success
cannot clear a failed export. Retention must not prune a recovery set required by
an active restore or unfinished export, and must consider both guest pool capacity
and actual host free space. Snapshot count alone is not a storage budget.

## Acceptance before release

Required evidence includes real CSI snapshot/clone contents, coordinated database
and object-store restore, system restart/recovery, host export/import with the
original image absent, and interruption/full-storage failures. Unit tests cover
catalog publication, identities, journal transitions and malformed input; passing
them does not establish platform boot or power-loss acceptance.

Use the opt-in recovery acceptance harness with a disposable CI-built appliance.
The default lightweight build/boot path remains unchanged. Record the exact
platform, appliance/app identities and which recovery checks ran in release notes.
