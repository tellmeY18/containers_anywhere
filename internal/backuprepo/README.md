# Independent host recovery repositories

Source implementation; CI and platform durability/restore acceptance are still
required. This is a **Full** export format, with no incremental or deduplication
claim. One backup contains the complete guest export, including the guest's own
compatibility manifest and full Btrfs send streams. The host treats that payload
as opaque, so system and workload/PVC recovery points use the same repository.
Only completed guest exports should be supplied. The guest must hold its source
point lease/lock throughout export and reject incomplete exports on import.

## Host enrollment and CLI

The destination must already exist, be empty for initial enrollment, and have an
absolute clean path with no symlink components. It must neither contain nor be
inside cluster state. Its random 128-bit ID is the only destination reference
sent to Kubernetes. Host-only enrollment is in
`<state>/recovery-backup-destinations.json`; the independent repository catalog is
outside `data.img` and needs neither enrollment nor a running cluster to read.

```sh
containers-anywhere recovery-backup destination add --state /real/cluster-state \
  --name 'External recovery' --path /Volumes/Recovery/repository \
  --key /separate-safe-location/recovery.key --keep 8
containers-anywhere recovery-backup destination list --state /real/cluster-state
containers-anywhere recovery-backup export --state /real/cluster-state \
  --destination RANDOM_DESTINATION_ID --point COMPLETED_GUEST_POINT_ID
containers-anywhere recovery-backup list --repository /Volumes/Recovery/repository
containers-anywhere recovery-backup verify --repository /Volumes/Recovery/repository \
  --id BACKUP_ID --key /separate-safe-location/recovery.key
containers-anywhere recovery-backup import --state /new/recovery-appliance-state \
  --repository /Volumes/Recovery/repository --id BACKUP_ID \
  --key /separate-safe-location/recovery.key --staging-dir /private/staging
```

`import` requires a running clean compatible recovery appliance. The guest checks
the compatibility manifest and safe empty target; this operation does not itself
boot or activate the replacement. Parent launcher lifecycle integration owns
those actions. `verify` proves archive integrity, not workload restore acceptance.

Enrollment can instead use `--recipient age1...`: writers then need only the
public recipient, with private key custody entirely separate. `--key` on add
creates a new key exclusively and refuses existing files; to reuse a key, supply
its public recipient. Generated keys are never stored in the repository or state,
and are retained if subsequent enrollment fails. If key publication itself fails,
the explicit destination is never deleted/overwritten, and the complete private
staged key is retained at the path reported in the host error. The file paths are separate;
the operator must arrange separate physical/offline custody. Removing enrollment
does not touch backups. Re-enrollment of an existing repository checks its
recipient and uses its original opaque ID.

`protect --id ID --protected=true` prevents automatic removal; `false` clears
that flag. Retention keeps at most `--keep` completed backups, always retaining
protected points and the most recently verified point. If they fill the limit,
new exports fail before writing. Retention runs only after a new backup commits;
an interrupted retention can be retried with `prune`. Exact pending deletions
are also resumed on subsequent repository open/export. The configured reserve
defaults to 20 GiB; the enforced reserve is the maximum of that configuration,
20 GiB, and 10% of the destination filesystem capacity, matching the host-growth
floor. It is checked before export/staging and periodically while streaming.
Copy publication preflights its additional full-size copy and checks the reserve
while copying too. Small drives unable to retain that floor report not ready.
Old backups are never pruned to make an in-progress export fit.

## Repository v1

* `repository.json`: version, random identity, public age X25519 recipient,
  retention count, and free-space reserve. It contains no private keys.
* `catalog.json`: completed entries, IDs, scope, `mode: "Full"`, transport,
  timestamps, plaintext/ciphertext sizes, encrypted SHA-256, protection and last
  integrity-verification time, plus a separate `pendingDeletions` journal of
  exact retired backup IDs. This metadata is readable without a key.
* `<backup-id>.age`: age-authenticated encryption of
  `CA-RECOVERY-BACKUP-1\n`, a big-endian uint32 JSON-header length, authenticated
  metadata JSON, then the exact full guest export through authenticated EOF.
  Transport is `guest-recovery-full-v1`, reserving explicit format discrimination
  for a future, independently tested incremental implementation.
* `.repository.lock`: persistent OS advisory lock, shared by readers, writers,
  verifiers, import staging and retention. Process exit releases the lock.

Writes use exclusive random staging files and sync their content. Filesystems
with hard links atomically link them under a never-overwritten final filename.
For known unsupported-link errors, including FAT/exFAT identified after the
platform's unsupported-operation error, publication instead exclusive-creates
the final filename, copies the staged bytes, checks the length, then syncs the
complete file and directory. Permission, collision, and unrelated I/O errors do
not trigger fallback. The destination is never overwritten or removed on error.
Copy publication has **non-atomic file appearance but atomic catalog visibility**:
only after successful publication does the catalog atomically replace/sync and
advertise completion. An interrupted partial copy remains an unlisted orphan,
and archive authentication/length checks reject partial data during import.

The catalog is the completed-publication boundary. Unix directory
entries are fsynced. Windows flushes file contents; directory fsync through
`os.File` is unavailable and filesystem power-loss ordering requires platform
acceptance. The portable copy path permits encrypted repositories on exFAT USB
disks without weakening exclusive-create or catalog completion semantics. Actual
filesystem capacity and file-size limits still apply (for example FAT32's 4 GiB
limit). Filesystems lacking the required locking/rename/sync semantics fail closed.
Private keys and plaintext staging must use storage supporting the required
owner-only permissions/ACLs. Windows copied keys are exclusive-created with a
protected owner-only security descriptor in `CreateFile` itself; there is no
temporarily inherited ACL or post-creation permission-narrowing window.

A crash can leave an unlisted `.pending-*` or `.age` orphan; it is not advertised
as completed, overwritten, or silently adopted. Retention atomically removes
advertised entries and journals their exact deletion IDs in the same catalog
commit, before deleting any file. The journal is cleared only after all deletions
and their directory entries are synced. Failed deletions remain journaled;
already-missing files are acknowledged on retry after a crash. Open, export and
prune resume only these journaled IDs, never scan/delete unrelated export orphans.
The catalog is
required by this version's supported recovery API; payloads are complete and
contain authenticated metadata, but automatic catalog reconstruction is not
implemented. Copy the whole repository and keep the key separately.

Restore decrypts every byte and authenticates EOF, checks the authenticated header
against the catalog and the complete ciphertext hash, then publishes a private,
fsynced plaintext staging file. Only that verified file is forwarded to the
guest. Ordinary failures remove staging; process/power loss can leave a private
plaintext staging directory requiring explicit cleanup. Staging needs a full
additional plaintext copy, or two temporarily when copy publication is needed.
Encrypted copy publication likewise temporarily needs both complete ciphertext
copies. The host never extracts guest archive members.

## API / bridge integration

`Enroll`, `Destinations`, `Forget`, `Resolve`, and path-free `Facts` manage host
enrollment. `Initialize` and `Open` provide direct standalone repository access.
Repository methods are `Export`, `List`, `Verify`, `Stage`, `Protect`, and `Prune`.
`Stage` returns `*StagedExport`; consume its `File`, then call `Close` to remove it.
Set `ExportOptions.ExpectedBytes = -1` for unknown-length streams; otherwise the
exact supplied length is enforced. `Export` may return both a committed `Entry`
and a retention error; callers must preserve/report that committed result.
`List()` exposes completed entries for deterministic-ID retry checks; callers
should match both backup ID and source point/scope before reusing a result. An
unlisted orphan with the requested ID is not a successful retry and is never
overwritten automatically.

Root `runRecoveryBackupCLI(args []string) error` owns CLI flags.
`runRecoveryBackup(ctx, client, RecoveryBackupOptions) (any, error)` supports host
callbacks. With a supplied authenticated VM client it uses
`GET /_agent/recovery/points/{id}/export` and `POST /_agent/recovery/import`.
With a nil client it uses the host-only loopback bearer capability and the same
suffixes under `/control/recovery`. Streaming removes the ordinary 30-second
total timeout, disables redirects, and obeys cancellation. The parent bridge
must propagate export stream errors by aborting the response, not returning
successful EOF; the guest must additionally validate its terminal manifest.

The controller must accept enrolled IDs, never host paths or key filenames.
`Facts` intentionally omits filesystem error strings, which can contain paths.
It exposes `minFreeBytes` (configured), `reserveBytes` (effective), total/free
capacity, `available`, `busy`, and `ready`. Ready additionally requires capacity
above the reserve, no active operation, and room under protected retention;
per-export preflight still accounts for the actual stream size. `EffectiveReserve`
is exported for host/controller capacity decisions.
For deterministic unit fixtures, `Config.DiskSpace` can supply a per-repository
runtime capacity provider. It is never serialized or accepted from JSON, and
production entrypoints leave it unset to use the real host filesystem. All
preflight and streaming capacity checks, including import staging and copy
publication, use the same provider without changing the enforced reserve policy.
Host callers can log detailed errors; Kubernetes status should use sanitized
reason codes. Parent integration owns operation-progress persistence and source
point leases. PVC sources must be resolved from validated CSI metadata in the
guest; no arbitrary host/guest paths should be accepted from workload requests.

## Security boundaries

`os.Root` anchors repository operations, and strict paths/regular-file checks
reject traversal and symlinks. A missing mounted root is never recreated;
resolution also checks the enrolled repository identity. Protection flags and
catalog verification dates are local policy, not immutable storage or signed
attestations. Age authenticates ciphertext integrity, **not sender identity**:
anyone with the public recipient can create new encrypted content. Host-owner
malware or an attacker able to rewrite repository contents can delete/replace
backups or policy. Off-device, offline/immutable copies remain separate concerns.

Capacity checks reuse the CARE Clinic-derived `internal/diskspace` helper.
Private Windows staging directories use the CARE Clinic private-ACL pattern
already attributed in `THIRD_PARTY_NOTICES.md`. Existing `atomicfile` was reviewed;
its create-parent/replace behavior is intentionally unsuitable for missing
removable roots and exclusive backup publication.
