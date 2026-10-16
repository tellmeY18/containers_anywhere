# Recovery options: exploratory notes

**Status: exploratory notes for later consideration, not a design or
implementation contract.** Nothing described here is built. The approved
recovery contract remains [DISASTER_RECOVERY.md](DISASTER_RECOVERY.md) and its
[ROADMAP](../ROADMAP.md) gates. No PITR, progressive snapshot, drill,
cross-machine restore or platform acceptance result is claimed by this file.

## Why this exists

Goal: workload durability and disaster recovery at a quality that Docker
Desktop and Rancher Desktop dev setups do not provide. Two concrete asks:

- An average user configures a local directory (Windows, Linux or macOS),
  receives recovery points, and can restore on a **new machine** by installing
  the appliance and copying that folder.
- Recovery is **API-first**, so a workload manager can drive it. Kite is the
  current candidate; so is CNCM, or a future self-managed implementation.
- Recovery points are **progressive**: more than one usable point, and a choice
  among them.

## Finding 1: persistent state is already confined to one directory

The guest root is EROFS (`nix/guest.nix:52-58`) and `/var/lib` is the only
writable volume (`nix/guest.nix:59-65`). Everything that must survive a reboot
lives in one directory, which is the host's `data.img`.

| Path under `/var/lib` | Holds |
| --- | --- |
| `rancher/k3s/server/db/` | kine SQLite: every Kubernetes object, Secrets, ConfigMaps, CRDs, RBAC, and revision history |
| `rancher/k3s/server/token` | encrypts confidential data inside the datastore; without it a restore is unusable |
| `rancher/k3s/server/cred/` | node client and server certs, bootstrap credentials, node name: cluster identity |
| `rancher/k3s/server/manifests/` | applied addon manifests |
| `rancher/k3s/agent/` | CNI config, kubelet state, containerd image store and overlay snapshots |
| `rancher/k3s/storage/` | all local-path PVCs, including a CNPG or Postgres PGDATA on a volume |
| `containers-anywhere/kite/` | Kite SQLite, plugins and keys (`nix/guest.nix:91`) |
| `rancher/node` | node password, symlinked from `/etc/rancher/node` (`nix/guest.nix:88-92`) |

Consequences:

- A database deployed on a local-path volume is already inside the folder. No
  per-workload backup wiring is needed for **coverage**.
- Cluster objects, identities, volumes and the Kite database can be restored
  **together and consistently by construction**, because there is one disk and
  one atomic point. That satisfies "restore resources and volume together"
  ([DISASTER_RECOVERY.md:117](DISASTER_RECOVERY.md)) without volume-to-PVC
  matching logic.
- Externally managed databases are correctly out of scope: that data is not on
  the disk.

## Finding 2: "copy that folder" is not a recovery kit

The bytes are sufficient; the recovery is not.

1. **Identity is not in the folder.** The age key is generated host-side during
   backup (`storage.go:161`, `storage.go:201`) and the appliance identity lives
   in `release.json` in the host state directory. A new machine cannot tell
   which release a folder belongs to, and restore refuses anything but the
   exact original appliance (`updates.go:22-25`).
2. **Atomicity.** A live folder copy is crash-consistent at best. Postgres
   recovers from WAL only if segments landed intact, which is why backup today
   requires the stopped VM and the lifecycle lock (`storage.go:147-157`).
3. **One point, not a choice of points.**
4. **Cost.** A flat, un-deduped copy of a 16 GiB disk is the reason cadence is
   manual: it cannot run often.
5. **No proof.** An unreadable backup and a readable one look identical until an
   incident.
6. **Same disk is not a backup.** A folder on the host disk protects against
   none of the failures that matter
   ([DISASTER_RECOVERY.md:85](DISASTER_RECOVERY.md)).

Quieter items worth an explicit decision:

- **Guest logs are not in the folder.** With an EROFS root, the systemd journal
  is volatile and lost on reboot; forensics depends on host-side `console.log`
  and `launcher.log`. A bounded log file on the data disk would let the kit
  carry recent history.
- **The containerd image store is in the folder and is largely re-derivable.**
  `rancher/k3s/agent/containerd` holds pulled images. Keeping them grows every
  recovery point; excluding them makes restore depend on network access, or on
  the airgap images already present in `system.img`.

## Finding 3: the appliance must not do transaction-level PITR

Point-in-time recovery to an arbitrary transaction is application work. The
appliance can produce labelled **recovery points** and let an operator choose
among them. The appliance-level vocabulary is the consistency ladder already in
[DISASTER_RECOVERY.md:32-34](DISASTER_RECOVERY.md):

| Consistency level | Appliance capability |
| --- | --- |
| stopped-machine | Implemented today: stop VM, copy `data.img`, age-encrypt (`storage.go:147-207`) |
| crash-consistent | Proposed: atomic point of `/var/lib` while the VM runs, journal replay on restore |
| application-consistent | Only via opt-in quiesce hooks a workload registers; belongs to the workload owner |

## Two levels

### Level 1: appliance recovery point

One mechanism covering everything on the data disk: atomic point, deduplicated
and encrypted export to a host-owned destination, packaged as a kit, restored
with `restore --kit` and proven by `restore-drill`.

Granularity is checkpoints, not transactions: the interval depends on snapshot
cost, and any retained point is selectable.

Two build steps, with very different reach:

- **1a: cold-stop plus a host-side dedupe and encrypt engine.** No appliance
  change, so `bundleID` does not move and **existing clusters can adopt it**.
  Cost per run is one read of a sparse 16 GiB image plus content-defined
  chunking, in a maintenance window, using the graceful poweroff path the agent
  already implements (`guest_linux.go:73-79`). This alone delivers the
  new-machine flow, with downtime.
- **1b: guest-side LVM snapshots** for points without stopping the cluster.
  `services.lvm.enable = false` today (`nix/guest.nix:47`), so this changes the
  appliance and therefore strands existing users until the upgrade path exists.

Platform asymmetry drives this choice. QEMU on Windows and Linux supports live
block-layer snapshots through QMP on a running VM, so the launcher could take
points itself. Virtualization.framework on macOS exposes **no snapshot API**, and
suspend and resume is unaccepted (README). The only primitive that behaves
identically on all three host operating systems is guest-side copy-on-write on
the data disk. A QEMU block-layer path is a Windows and Linux optimisation, not
the foundation.

### Level 2: workload PITR

Per-application and opt-in, for anything with transaction logs: CNPG with a
cloud backup plugin, MySQL binlogs, MongoDB oplogs, object versioning. This is
the only level that reaches an arbitrary timestamp, and the only one that can
coordinate a database with object files.

Division rule, to keep them from conflicting:

> Level 1 owns completeness and portability. Level 2 owns time granularity and
> application consistency.

Level 1 must never be described as PITR. Restore precedence must be documented:
a rolled-back appliance disk combined with a later-dated application backup is a
real incident shape, and the operator needs to know which one wins.

## Recovery kit

Required contents, with where each item lives today:

| Item | Location today |
| --- | --- |
| Data disk content | `data.img` payload of a cold backup |
| Appliance identity and `bundleID` | `release.json` in the host state directory |
| Architecture and versions | implied by identity; must be explicit |
| K3s server token and node credentials | inside `data.img`, but must be named in the manifest, not incidental |
| Pinned appliance artifact | launcher build pins; kit should record the expected identity |
| Recovery key | generated per backup; must never live only inside the kit |
| Last completed and verified point, drill record | does not exist yet |

Launcher behaviour that is missing and needed for the average-user flow: today
`restore` prints "Start with the matching release bundle", and the mismatch only
surfaces later at `serve` as "cluster data requires its original bundle". Restore
mode should instead **verify the pinned appliance against the kit before
publishing anything**, and fail early with an actionable message.

## Host destination and engine

This is the storage side of issue #3. Sketch, subject to that contract:

- Destination root is owned by the launcher; the guest may write only beneath
  it. No arbitrary host filesystem access, no traversal, no symlink escape, no
  read-back.
- Any helper is a pinned, hash-verified component, shipped the same way as QEMU
  (`scripts/runtime.py:22-25`, `vm_qemu.go:226-271`), with no host runtime
  prerequisite.
- Reuse `internal/diskspace` for free-space gating.
- Artifact protocol: staging, verify, then publish. Content-addressed names,
  temporary file plus rename, refuse to overwrite, mirroring the existing
  `O_EXCL` rules (`storage.go:178`, `storage.go:190`).
- Retention belongs to the caller; the launcher never deletes destination
  content.
- Label a same-device destination honestly. It is convenience, not disaster
  recovery.

Guest API surface, on the existing per-boot mTLS channel, fenced the way Kite
already is (`guest_linux.go:34-36`, `main.go:347-373`):

```
GET  /_agent/backup/status      per profile: last completed, last verified,
                                 earliest and latest recoverable, archive lag,
                                 consistency level, destination, degraded reason
POST /_agent/backup/run         start a job
GET  /_agent/backup/jobs/{id}   job state
POST /_agent/restore/plan       feasibility, expected data loss, dependencies
POST /_agent/restore/execute    isolated boot, explicit cutover, fence the old instance
```

Workload-originated calls must be authorised against the cluster, not only with
the host capability: verify a ServiceAccount token, and read nothing from the
pod, per [DISASTER_RECOVERY.md:135-137](DISASTER_RECOVERY.md).

## Availability is a separate problem from backup

A host reboot at 03:00 currently leaves the appliance stopped: the launcher is a
GUI application with no autostart contract, and suspend and resume is unaccepted.
For most clinics that is a more frequent disaster than data loss, and no amount
of recovery points addresses it. Treat launcher autostart plus guest crash
recovery as a priority in its own right.

## Decisions needed before any of this is a contract

1. Recovery point cadence and retention, and the cost model that supports it.
2. Whether the containerd image store is included, per destination.
3. Whether bounded guest logs are written to the data disk.
4. LVM in the guest, QEMU block-layer snapshots on Windows and Linux, or both.
5. Recovery key custody and export ergonomics.
6. Whether the upgrade and migration path is built before or after 1b, given
   that 1b changes the appliance.
7. Destination adapter order. The CNCM POC needs S3-compatible storage first;
   local folder and removable media are convenience adapters.

## Non-claims and acceptance still required

- 1b changes the appliance, which changes `bundleID`, so existing clusters
  cannot adopt it without the R4 upgrade and migration path
  (`updates.go:22-25`, AGENTS.md).
- Cross-architecture restore stays refused (`storage.go:326`).
- Off-device copies are required for real disaster recovery
  ([DISASTER_RECOVERY.md:85](DISASTER_RECOVERY.md)).
- Lifecycle and storage changes still require the real smoke test with a reboot
  and a backup and restore cycle, plus physical Windows WHPX and local macOS
  acceptance. Cross-compilation, packaging and TCG runs prove none of it.

## Appendix: upstream options considered for Level 2 context

Recorded so the research is not repeated. These are application-level or
datastore-level and do not belong to Level 1.

- **kine ships a `litestream` driver.** Selecting it via `--datastore-endpoint`
  hands WAL checkpointing to Litestream and enables continuous replication of
  the cluster datastore. Litestream is Apache-2.0 as of 0.5.x, ships linux-amd64
  and linux-arm64 builds, and supports a `file://` replica as well as S3 and
  others. Caveat recorded in the upstream discussion: if Litestream is not
  running, WAL growth is unbounded, and the proposed failsafe is not upstream.
  Owning that failsafe and its tests would be our work.
- **K3s requires the server token** alongside any datastore backup, because the
  token encrypts confidential data inside the datastore.
- **PostgreSQL plus pgBackRest or a cloud backup plugin** is the mature path for
  application PITR, and is what issue #3 assumes for CNCM. Moving the K3s
  datastore itself to PostgreSQL is possible and would make the cluster's own
  state continuously archivable, at the cost of adding a database to the
  appliance.