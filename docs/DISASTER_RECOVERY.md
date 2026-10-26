# Disaster recovery architecture

**Design status: proposed implementation contract.** Manual encrypted cold backups,
empty-destination whole-appliance restore, and the
[explicit current-contract update transaction](UPDATES.md) are implemented in source.
The update transaction adds a local cold recovery point and separate recovery export,
not isolated validation or automatic rollback; platform acceptance remains gated.
This document defines the path toward mission-critical durability, not a claim
that the alpha already provides PITR, continuous backup or high availability.

## Protection boundaries

The local device is one failure domain. VM isolation does not protect against
host disk failure, theft, power loss, malware with host privileges or lost keys.
Availability through loss of that device needs another host and an explicit
failover design. Single-node K3s does not provide that by itself.

Recovery comprises four different layers:

| Layer | Captures | Does not imply |
| --- | --- | --- |
| Whole-appliance cold recovery point | Stopped data disk, K3s state, Kite, local volumes and keys on disk | Arbitrary-time PITR or uninterrupted availability |
| Cluster datastore backup | Kubernetes objects, credentials and control-plane state | Persistent-volume contents or external service data |
| Workload-native backup and transaction logs | Supported application's data and recoverable timeline | Consistent recovery of other applications |
| Off-device recovery kit | Backup, exact runtime dependencies and independently recoverable keys | Validity until an actual restore has been tested |

The current K3s server uses SQLite. Etcd snapshot tooling is not applicable to
that configuration. Do not change datastore merely to obtain a snapshot command.
For cold recovery the whole stopped disk is the coherent cluster recovery unit.

## Recovery contract

1. Preserve original data. Restore into a new empty destination, validate there,
   and explicitly activate. No automatic rewind of a live disk.
2. Name the consistency level: stopped-machine, crash-consistent,
   application-consistent, or coordinated multi-service. Filesystem freeze alone
   does not establish database/application consistency.
3. Authenticate complete backup contents before publication. A partial upload is
   never a completed recovery point. Publication and interruption handling are
   journaled and retryable without guessing which disk is authoritative.
4. Capture dependencies: appliance identity, CPU architecture, disk format,
   Kubernetes/K3s versions, encryption material, image digests and external data
   references. Retain exact artifacts independently of release hosting.
5. Report recoverability, not just backup execution: last completed point, last
   verified point, last successful restore drill, destination, age and failures.
6. Keys are part of recovery. Encrypt before transfer; export a recoverable key
   separately from backups and prove recovery without the original machine.

## PITR: workload-aware by necessity

Point-in-time recovery means selecting a timestamp or transaction boundary within
a verified recovery window. Periodic VM snapshots provide discrete recovery
points, not continuous application PITR. Kubernetes resource backups and tools
such as Velero can complement recovery, but do not independently provide database
transaction recovery or consistent snapshots for this local-path storage setup.

For the initial PostgreSQL profile, require a supported base backup and a complete
WAL chain through the requested target on the appropriate timeline. Use maintained
database tooling. An operator-managed workload can use its supported integration;
an existing unmanaged database needs explicit configuration and verification.
Do not claim protection merely because PostgreSQL was discovered in a pod.

The profile must expose:

- Supported application/version and workload identity.
- Backup repository, encryption key reference and independently tested credentials.
- Base backup validity, continuous archive lag and earliest/latest recoverable time.
- Missing segments, timeline forks and retention dependencies.
- Restore prerequisites, isolation, validation and explicit cutover procedure.

MySQL binlogs and other databases need their own integrations and acceptance.
Applications without native logs may only support discrete consistent checkpoints.
For database/object-store combinations, coordinate checkpoints or define a tested
reconciliation procedure. Never promise cross-service atomic PITR without one.

## Off-device backups and retention

Use an existing encrypted, deduplicating backup engine where it meets the
contract. Keep the consumer launcher free of host runtime prerequisites: any
helper is pinned and shipped/downloaded as a verified component. Select the engine
after testing large sparse disks, interruption behavior and all host platforms.

Start with scheduled cold backups during agreed downtime; online disk copies are
not a safe shortcut. Later application-consistent online protection can avoid
whole-VM downtime for supported workloads.

Aim for independent copies, off-site protection and at least one immutable or
offline copy. A second folder or partition on the same disk is not off-device.
Object lock is optional destination configuration with verified retention, not a
label in the UI. Separate backup writers from deletion/retention authority.

Retention is dependency-aware: keep required base backups, transaction logs,
artifact versions and keys for every advertised recovery window. Never prune the
last verified point because a new job merely started. Handle quotas, clock skew,
missed schedules, battery/offline periods and destination outages explicitly.
Display a breached recovery objective until successful protection resumes.

## Restore and recovery drills

1. Select incident type and recovery point; show expected data loss and dependencies.
2. Check architecture, artifact availability, key access and staging/free-space budget.
3. Restore to a fresh directory/cluster and preserve the original unchanged.
4. Boot in isolation. Block outbound side effects (email, payments, webhooks,
   scheduled jobs), external writes and accidental membership in the live cluster.
   The current diagnostic `CONTAINERS_NO_NETWORK` switch alone is not a complete
   automated drill isolation design and may prevent normal K3s networking on macOS.
5. Validate identities, secrets, PVC contents, application login and domain checks.
6. Report evidence and elapsed recovery time; require explicit cutover. Fence the
   old instance before enabling external writes on its replacement.

Exercise recovery on another machine with only the recovery kit and separately
held key. A same-host boot test is insufficient for bare-metal disaster recovery.
Keep logs free of keys and sensitive workload data.

## Incident matrix and acceptance

| Incident | Required response / test |
| --- | --- |
| Deleted records | Workload PITR before deletion; validate newer unrelated data policy |
| Lost volume or namespace | Restore resources and volume together; check identities/references |
| Failed app migration | App-specific recovery; Helm rollback cannot undo schema changes |
| Corrupt K3s datastore | Restore coherent appliance point; preserve the failed disk |
| Host disk loss/theft | Replacement-host drill from off-device kit and separate key |
| Ransomware/local deletion | Remote immutable/offline point survives compromised writer |
| Power loss during disk/backup publication | Retry or reject ambiguous state; never reformat existing data |
| Disk full, inode exhaustion | Early pressure signals, bounded logs, failed writes surfaced |
| Suspend/wake or network change | Reconnect without duplicate VM writers or silent backup gaps |
| Lost key / missing transaction logs | Explicit unrecoverable window; never show a false healthy status |
| Interrupted upgrade | Durable phase journal; exact old artifacts and recovery point retained |
| Both original and restored host active | Fence before cutover; prevent duplicate external side effects |

Record RPO and RTO from measured workload-specific drills. A proposed continuous
archive objective is not achieved until the complete chain is durable off-device.
No default numerical SLA is published before measurements and failure testing.

## Security and updates

Protect backup credentials from ordinary workloads. Do not mount repository keys
into arbitrary pods or expose them through Kite's workload proxy. A guest or host
administrator remains inside the relevant trust boundary; document this clearly.

Add publisher-signed metadata, rollback/replay protection and platform signing
before automatic upgrades. Require datastore migration and recovery acceptance
for each upgrade path. Until then, retain the alpha rule: exact original appliance
for existing disks. Never reinterpret that rule as universal downgrade support.

Implementation sequence and gates: [ROADMAP.md](../ROADMAP.md).
