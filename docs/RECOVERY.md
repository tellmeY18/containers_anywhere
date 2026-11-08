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

## Kubernetes usage

The API group is `recovery.containersanywhere.dev/v1alpha1`:

| Namespaced resource | Cluster-scoped counterpart | Purpose |
| --- | --- | --- |
| `ProtectionPolicy` | `ClusterProtectionPolicy` | UTC schedule and retention |
| `RecoveryPoint` | `ClusterRecoveryPoint` | Manual/scheduled capture and independent export status |
| `RestoreRequest` | `ClusterRestoreRequest` | New-PVC restoration or cold system/full restoration |

`BackupDestination` is cluster-scoped. The platform creates roles but no workload
bindings. An administrator explicitly enrolls namespaces using
[recovery-rbac.yaml](examples/recovery-rbac.yaml). Workload operations impersonate
that namespace's fixed `recovery-workload` service account; hook exec permissions
must name the permitted pods. There is no fallback to cluster-admin execution.

Examples:

```sh
kubectl apply -f docs/examples/recovery-rbac.yaml
kubectl apply -f docs/examples/recovery-point.yaml
kubectl get recoverypoints -A
kubectl apply -f docs/examples/recovery-restore.yaml
kubectl get restorerequests -A
```

Adapt example namespaces, exact PVC membership and labels to the application.
`PerPVC` captures independent live snapshots; `QuiescedGroup` requires explicit
prepare/resume/check hooks. ConfigMap capture is opt-in. Namespace-local recovery
does not automatically capture Secrets or application/operator definitions; full
guest recovery includes the Kubernetes datastore containing them.

For a full cold checkpoint, apply:

```yaml
apiVersion: recovery.containersanywhere.dev/v1alpha1
kind: ClusterRecoveryPoint
metadata:
  name: before-maintenance
spec:
  scope: full
```

Use `scope: system` for system-only capture. Both restart the appliance to capture
before K3s starts. Inspect `status.recoveryPointID` and `status.phase`:

```sh
kubectl get clusterrecoverypoints
kubectl get clusterrestorerequests
```

A cluster restore may rewind away its own request. Its durable operation can still
be inspected through authenticated launcher control:
`GET /control/recovery/operations/<request-UID>`. The launcher panel lists points
and can request a full capture or restore. Scheduling examples are in
[recovery-policy.yaml](examples/recovery-policy.yaml) and
[recovery-cluster.yaml](examples/recovery-cluster.yaml).

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

Standard CSI restores can remain unbound until their first consumer is scheduled.
For a host-imported workload point, the controller provisions fresh targets on the
single appliance node and holds a PVC consumer admission fence during population.
It verifies that the API server enforces the fence before copying data, checks for
existing consumers, and releases it only after all target populations complete.
Interrupted nonempty targets remain fenced and preserved for inspection; use new
target names and a new request UID after a failed population.

Importing into a replacement namespace with a new UID requires an administrator's
explicit grant on that Namespace:

```sh
kubectl annotate namespace my-app \
  recovery.containersanywhere.dev/import-points=IMPORTED_POINT_ID
```

Then use a `RestoreRequest` with `durablePointID` and an exact `pvcNames` mapping;
see [recovery-restore-imported.yaml](examples/recovery-restore-imported.yaml).
The grant permits the selected point in that namespace; it is not a host path or
credential. Multiple point IDs can be comma-separated. Namespace users cannot
grant this permission by editing their namespaced recovery request.

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

Full exports currently send each captured subvolume independently, including CSI
snapshot history for full guest points. Shared extents can therefore appear in
more than one stream. Guest staging and host capacity must accommodate the export
size, which can exceed the Btrfs pool's uniquely allocated data. Capacity refusal
is reported without pruning the last good point to make a pending export fit.

### Enroll, export and verify a host destination

Use existing real directories outside cluster state, with a separate location for
the key. The enrollment command creates a new key exclusively:

```sh
containers-anywhere recovery-backup destination add --state /cluster/state \
  --name 'External recovery' --path /external/repository \
  --key /separate/recovery.age.key --keep 8
```

The result includes an opaque destination ID. Bind it to Kubernetes with a
`BackupDestination` whose `spec.provider` is `HostDirectory` and
`spec.hostDirectoryID` is that ID. Administrators list authorized namespaces in
`spec.allowedNamespaces`; an empty list authorizes only cluster captures. Set
`spec.destination` on a point or policy to the **BackupDestination CR name**.
See [recovery-destinations.yaml](examples/recovery-destinations.yaml).

Host CLI operations also work without a policy:

```sh
containers-anywhere recovery-backup export --state /cluster/state \
  --destination ENROLLED_ID --point COMPLETED_POINT_ID --scope full
containers-anywhere recovery-backup list --repository /external/repository
containers-anywhere recovery-backup verify --repository /external/repository \
  --id BACKUP_ID --key /separate/recovery.age.key
```

Export completion and integrity verification are distinct from a successful
application restore. Copy the whole repository, including `repository.json` and
`catalog.json`; preserve unlisted interrupted files for explicit investigation.
The repository supports a portable exclusive-copy publication path for filesystems
without hard links, including removable-disk filesystems. In both paths the
catalog publishes a backup only after the encrypted data has been synced.

### Replacement-image restore

Start a new empty state directory using the compatible original appliance. In a
second terminal, import the independent backup and explicitly activate its point:

```sh
containers-anywhere recovery-backup import --state /new/cluster-state \
  --repository /external/repository --id BACKUP_ID \
  --key /separate/recovery.age.key --staging-dir /private/staging
containers-anywhere restore-point --state /new/cluster-state \
  --operation restore --id IMPORTED_POINT_ID --accept-data-loss
```

The original `data.img`, enrollment file and Kubernetes API are not required to
list or authenticate the backup. Import needs a running compatible guest to
receive Btrfs data; it does not activate the point by itself. A full/system restore
restarts that guest. Keep the matching appliance/runtime artifacts independently.
These are **guest** recovery sets: host Caddy CA/configuration, launcher enrollment
and downloaded runtime artifacts are not included. Recover host settings
separately or enroll replacement settings and client trust deliberately.

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

Interrupted export/import stages are bounded and retained for diagnosis. Inspect
and explicitly remove only unpublished stages through the host CLI:

```sh
containers-anywhere restore-point --state /cluster/state --operation staging
containers-anywhere restore-point --state /cluster/state --operation remove-staging --id STAGE_ID
```

Cleanup is refused during active recovery. Published points and live volumes are
not staging cleanup targets.

## Acceptance before release

Required evidence includes real CSI snapshot/clone contents, coordinated database
and object-store restore, system restart/recovery, host export/import with the
original image absent, and interruption/full-storage failures. Unit tests cover
catalog publication, identities, journal transitions and malformed input; passing
them does not establish platform boot or power-loss acceptance.

Use the opt-in recovery acceptance harness with a disposable CI-built appliance:

```sh
gh workflow run alpha.yml --ref FEATURE_BRANCH -f recovery_platforms=true
gh workflow run appliance.yml --ref FEATURE_BRANCH -f recovery=true
```

The first command adds native macOS/Windows vet/race tests to the standard source
checks. The second explicitly enables Linux KVM lifecycle/recovery acceptance.
It exercises CSI restore, namespace recovery CRDs, workload host export/import,
system/full cold restore, tampered/truncated backup refusal, and full restore into
a fresh guest while the original `data.img` is absent. Evidence is retained with
diagnostic artifacts; repositories, private keys and data images are not uploaded.
The default lightweight build/boot path remains unchanged. Record the exact
platform, appliance/app identities and which recovery checks ran in release notes.
