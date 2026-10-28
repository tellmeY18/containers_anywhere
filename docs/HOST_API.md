# Host API

The host API gives a workload manager (Kite, Helm, an operator, a custom tool) a
small, application-agnostic way to read host truth and to *ask* for host actions. It
exists so that a manager does not need host storage, power or sizing code of its own.
It follows the `WorkloadPublication` pattern: custom resources in
`host.containersanywhere.dev/v1alpha1`, reconciled by the host over the existing
authenticated vsock/mTLS channel, with status written back under `uid` and
`resourceVersion` guards.

**Hard rules**

- Workloads never receive a host credential. They create or read Kubernetes objects
  with a role an administrator granted.
- Only a person in the launcher approves anything that changes the host. The launcher
  UI (or its authenticated control API for headless use) is the only approver.
- Status reports truth. A fact the launcher stopped reporting for 30 seconds becomes
  `Unknown`; the last value is never shown as current.
- Nothing here is application- or manager-specific. CNCM is one consumer.

## Where it lives

| Piece | Where | Why |
| --- | --- | --- |
| CRDs, roles, admission policies | `platform/hostapi.yaml` (app) | Pushed at runtime; adding an API does not need a new appliance. |
| Bridge (reads/patches resources) | guest agent, `internal/hostapi` | The only component that talks to the Kubernetes API for the host. |
| Facts, approval, effects | launcher, `host_api.go` | Only the host knows host truth and can act on it. |

The agent advertises the optional `host-api-v1` capability. An appliance without it
(for example the published 0.2.0) still runs and can still be updated by a newer app;
the bridge is simply off, the CRDs are unused, and `requests` reports that the
appliance predates the host API. Apps update more often than the appliance, so a new
feature must never strand an installed one: only the *required* capabilities in
`internal/protocol` decide whether an appliance can run.

## Resources

### `HostStatus` (cluster-scoped singleton named `host`, read-only)

App, appliance and `bundleID`, K3s version, architecture and OS, allocated and
requested CPU and memory against the host total, host state-directory free space,
data-disk size/used/free and a **disk-pressure** flag (free below the larger of 1 GiB
and 10 percent), uptime, clock skew against the host, launcher health, autostart state,
update state (below), gateway state (below), and storage use (images, volumes, the
console database, the journal) and journal summary. It never contains secrets or host
paths. Grant `containers-host-reader` to read it. Only the appliance's bridge identity
can write it: an admission policy denies everyone else.

**Update state** mirrors the journals: `idle`, `prepared`, `activating`,
`candidate-running`, `committed`, `cancelled`, or the swap phases `trying`, `committed`,
`rolled-back`, with the candidate version, retained recovery points and, when a pending
update blocks ordinary startup, the reason. A workload cannot activate or commit.

**Gateway state** carries the root-CA download URL and SHA-256 fingerprint (colon
separated, as `openssl x509 -fingerprint -sha256` prints it), HTTP/HTTPS ports, bound
addresses, whether the 8080/8443 fallback is in use, and the last LAN self-check.
Private keys are never exposed.

### `HostRequest` (namespaced; workload creates, launcher approves)

| `spec.type` | Parameters | Effect once approved |
| --- | --- | --- |
| `resources` | `cpus` 2–64, `memoryMiB` 2048–262144, optional `diskGiB` 8–256 | Saved as the size for the **next start**. Refused if it would oversubscribe the host. `diskGiB` grows the data disk at the next start (the host needs that much free space); it never shrinks and the disk is never recreated. |
| `power` | `action`: `shutdown` or `restart-appliance` | Recorded as Applied **before** it acts, then runs. Never replayed after a launcher restart. |
| `autostart` | `enabled` | Installs or removes the per-user launch entry. |
| `prune-images` | none | `crictl rmi --prune` in the guest: unreferenced images only, never volumes. Reports bytes reclaimed. |
| `recovery-point` | `destination`: the id of an enrolled `HostDestination` | Stops the appliance, writes an **encrypted whole-appliance copy** to the destination, reads it back, and starts the appliance again (a few minutes of downtime). Needs approval. Recorded as Applied **before** it acts and never replayed. Progress is in the read-only `HostRecoveryPoint` named in `status.result`. |
| `backup-schedule` | `enabled`; with it `destination`, `everyHours` 6–720, `keepLast` 1–30, `windowStartHour` 0–23, `windowHours` 1–24 | Cold recovery points on a timetable. **Approving it is the consent for the downtime**, which only happens inside the window (the launcher computer's local time), never in the first ten minutes after a start, and backs off for an hour after a failure. Retention removes only points the schedule made. `enabled: false` cancels it. |
| `lan-check` | none | Needs no approval. Dials this computer's own LAN addresses on the HTTPS port. **Not proof that other devices can reach it.** |

Phases: `Pending` → `Approved` → `Applied` or `Failed`; or `Rejected`, or `Expired` after
24 hours. A terminal phase never changes. The spec is **immutable** after creation
(admission policy), so what a person approved is exactly what is applied. A decision
lands only on the object with the same `uid` that the launcher saw. Status is written
only by the bridge. Grant `containers-host-requester` in the requester's namespace.

### `HostDestination` (cluster-scoped, read-only)

A place the launcher can write backups to, enrolled by a person in the launcher panel (or `destinations --operation add --name ... --path ...`). No workload can enrol, change or remove one, and **no host path is ever published**.
`status`: `name`, `kind` (`folder`), `available`, `freeBytes`, `totalBytes`, `sameDeviceAsData`, `checkedAt`, `lastError`, and an `Available` condition.
A destination on the same disk as the cluster's data reports `sameDeviceAsData: true`: a copy there is **not disaster recovery**. A missing removable disk is `available: false`, and a different disk mounted at the same place is refused.
Un-enrolling never touches the files in the folder. There is no S3 endpoint yet.

### `HostRecoveryPoint` (cluster-scoped, read-only)

One whole-appliance copy. `status`: `destination`, `trigger` (`request` or `schedule`), `phase` (`Running`, `Complete`, `Failed`), `consistency` (always `stopped`: the appliance was shut down first), `startedAt`, `finishedAt`, `sizeBytes`, `sha256`, `verified`, `error`.
A point is `verified` only after it was decrypted and read back completely, and is published under its final name only then. **This is not point-in-time recovery of any application.** Interrupted writes are reported `Failed` and leave no partial file.
Points are encrypted with a key that lives in the launcher's state directory, never in the cluster or the destination. `HostStatus.status.host.recoveryKit` says whether the key exists and whether the person confirmed saving a copy elsewhere (**without one, a lost computer means unrestorable points**). Showing the key is launcher-only.
`HostStatus.status.host.backupSchedule` reports `lastSuccess`, `lastAttempt`, `nextDue` and `objectiveBreached` (the newest verified point is older than the interval plus a grace period, for example because the destination was offline).
Each complete point carries a computed `restorePlan`: `feasible`, `blockers` (file unreachable, wrong architecture, not enough free space), `warnings` (the key is not on this computer), `needsFreeBytes`, `expectedDataLoss` and `precedence` (which appliance the restore needs relative to the one running). It changes nothing; restoring is never automatic.
The data disk is sparse and a copy carries only its data extents (a map plus the data), so a 100 GiB disk holding 3 GiB is a copy of about 3 GiB, and a restore keeps the holes. On a platform that cannot report holes (Windows today) the copy is dense. Restoring is the existing `restore` command into an empty state directory, with the key.

### `HostRegistryTrust` (cluster-scoped; administrator-created)

Approves one registry endpoint, for example `127.0.0.1:30500`, so the container
runtime can pull from an in-cluster registry over plain HTTP. The agent writes a
containerd `hosts.toml` beneath the data disk's K3s directory; no root image change and
no restart. Only loopback or node addresses are accepted: no names (other than
`localhost`), wildcards, schemes or paths. Two resources claiming one endpoint are both
withdrawn. Deleting the resource revokes trust. The agent only ever removes files it
wrote, and refuses to replace configuration it did not write. Workloads can read
status but have no role to create trust. Note that a registry on a NodePort is reachable
on the node's interfaces, which the VM firewall does not expose to the LAN.

## Approval

```sh
containers-anywhere requests --operation list --state <dir>
containers-anywhere requests --operation approve --id <uid> --state <dir>
containers-anywhere requests --operation reject  --id <uid> --state <dir>
```

The desktop panel lists pending requests with Approve and Reject buttons. Request text
comes from workloads and is only ever shown as text.

## Logs and accounting

The guest journal is stored on the data disk (`/var/lib/containers-anywhere/journal`)
and capped at 64 MiB / 14 days, so a forensic trail survives a reboot and can travel
with a recovery kit. Secret values must never be logged by the appliance's own
services; workload logs are the workload's responsibility.

## Autostart

`containers-anywhere autostart --operation enable|disable|status` installs a per-user
entry that runs `serve --state <dir> --no-open --port 0`: a launchd agent on macOS, a
systemd user unit on Linux (starting before login needs `loginctl enable-linger`), a
scheduled task at logon on Windows. It restarts after a crash but not after a clean
stop. Every entry carries a marker, and the launcher will not touch an entry it did
not create. The exclusive lifecycle lock already prevents two writers on one disk.

## Using it from a workload manager

This is the contract a manager such as CNCM builds against. Everything is plain
Kubernetes: no SDK, no host token, no callback. Group/version: `host.containersanywhere.dev/v1alpha1`.

### 1. Find out whether the host API is there

The CRDs ship with the app, so they can exist on an appliance that cannot serve them (an
appliance older than 0.3.0 has no bridge). "The CRD exists" is **not** enough. Treat the host
API as available only when the singleton reports fresh facts:

```sh
kubectl get hoststatus host -o jsonpath='{.status.conditions[?(@.type=="HostReported")].status}'
# True      -> available; values under .status.host are current (at most 30 s old)
# Unknown   -> reason NoHostFacts (launcher has not reported) or Stale (reporting stopped);
#              .status.host is absent on purpose. Show "unknown", do not reuse an old value.
# (no object, or the CRD is missing) -> this appliance or app predates the host API
```

A `HostRequest` created against an appliance with no bridge is never given a `status` at
all. **Give up if `.status.phase` is still empty after about 30 seconds** and report the host
API as unavailable; do not retry in a loop.

### 2. Grant access (an administrator does this once)

Workloads cannot grant themselves anything. In Kite or with `kubectl` as a cluster admin:

```yaml
# read HostStatus and HostRegistryTrust (cluster-scoped, so a ClusterRoleBinding)
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: cncm-host-reader}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: containers-host-reader}
subjects: [{kind: ServiceAccount, name: cncm, namespace: cncm-system}]
---
# create/read/delete HostRequest in one namespace
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: cncm-host-requester, namespace: cncm-system}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: containers-host-requester}
subjects: [{kind: ServiceAccount, name: cncm, namespace: cncm-system}]
```

No role allows writing `status` or editing a request: an admission policy lets only the
appliance's bridge identity (`system:admin`) do that, even for cluster administrators.

### 3. Read `HostStatus` (cluster-scoped, name `host`)

```json
{"observedAt":"2026-10-28T10:00:05Z","hostFactsAgeSeconds":3,"clockSkewSeconds":0.2,
 "conditions":[{"type":"HostReported","status":"True","reason":"Fresh"},
               {"type":"DiskPressure","status":"False","reason":"Sufficient"}],
 "host":{"appVersion":"0.3.0","applianceVersion":"0.3.0","bundleID":"<sha256>","k3sVersion":"1.35.8+k3s1",
   "arch":"amd64","os":"windows","launcherHealthy":true,
   "cpu":{"allocated":2,"hostTotal":12,"requested":4},
   "memoryMiB":{"allocated":4096,"hostTotal":16384,"requested":8192},
   "stateDirFreeBytes":322122547200,"stateDirTotalBytes":511705088000,
   "autostart":{"supported":true,"enabled":false,"mechanism":"Windows scheduled task"},
   "update":{"phase":"idle","availableVersion":"0.3.1","retainedRecoveryPoints":[]},
   "gateway":{"enabled":true,"rootCAURL":"http://192.168.1.20:80/root.crt",
     "rootCAFingerprintSHA256":"AB:CD:...","httpPort":80,"httpsPort":443,
     "boundAddresses":["0.0.0.0"],"fallbackPorts":false}},
 "guest":{"uptimeSeconds":812,
   "disk":{"sizeBytes":17179869184,"usedBytes":3221225472,"freeBytes":13958643712,"pressure":false},
   "storage":{"imagesBytes":1,"volumesBytes":1,"consoleBytes":1,"journalBytes":1,"measuredAt":"...","partial":false},
   "logs":{"persisted":true,"bytes":1,"capBytes":67108864,"boots":3}}}
```

- `cpu.requested` / `memoryMiB.requested` appear only while an approved resize is waiting for the next start.
- `update.phase` mirrors the launcher's journals: `idle`, `prepared`, `activating`, `candidate-running`,
  `committed`, `cancelled`, or the swap phases `trying`, `committed`, `rolled-back`. `blockedReason` says why
  ordinary startup is blocked. A workload cannot start, activate or commit an update.
- `guest.disk.pressure` is true when free space is below the larger of 1 GiB and 10 percent.
- `guest.storage` is re-measured about every five minutes and is labelled `partial` if a walk timed out.
- Fresh values arrive roughly every 5 seconds and the object is rewritten at least every 30 seconds.
  Prefer a `watch` over polling; do not poll faster than every 10 seconds.
- `rootCAFingerprintSHA256` is colon-separated uppercase, the same format `openssl x509 -fingerprint -sha256` prints.

### 4. Make a `HostRequest`

Create it in your own namespace. Use `generateName`; every request is a separate record and
the spec cannot be edited afterwards.

```yaml
apiVersion: host.containersanywhere.dev/v1alpha1
kind: HostRequest
metadata: {generateName: resize-, namespace: cncm-system}
spec:
  type: resources            # resources | power | autostart | prune-images | lan-check
  reason: "PostgreSQL and the frontend build need more memory"   # optional, shown to the approver, max 256
  resources: {cpus: 4, memoryMiB: 8192}                           # cpus 2-64, memoryMiB 2048-262144
```

Other types: `power: {action: shutdown | restart-appliance}`, `autostart: {enabled: true|false}`, and
`prune-images` / `lan-check` with no parameters. Exactly the parameter block for the declared type is accepted.

**Lifecycle.** `status.phase` moves `Pending` -> `Approved` -> `Applied` (or `Failed`); or `Pending` -> `Rejected`
or `Expired` (24 hours without approval). A terminal phase never changes. Typical timing: `Pending` within
5-10 seconds of creation; a person approves in the launcher; `Applied` within seconds after that. `lan-check`
needs no approval and goes straight to `Applied`. `status.reason` and `status.message` are for people;
branch on `phase`, and on `reason` only for the codes below. Records are **not** garbage-collected; delete
yours once you have read the result. Deleting an `Applied` request does not undo anything.

| `reason` | Meaning |
| --- | --- |
| `AwaitingApproval` | Pending; nobody has decided. |
| `ApprovedInLauncher` / `RejectedInLauncher` | A person decided. |
| `Refused` | The host would not do it (for example a resize that oversubscribes the computer). Nothing changed. |
| `InvalidRequest` | The spec failed validation. |
| `Expired` | Not approved within 24 hours. |
| `Applied` | Done. See `status.result`. |
| `Interrupted` | The launcher restarted between approval and applying a power request. Not replayed; create a new one. |
| `WriteFailed`, `AutostartFailed`, `PruneFailed`, `Unsupported` | The effect failed; `message` has detail. |

**`status.result` by type**

| Type | `result` |
| --- | --- |
| `resources` | `{"cpus":4,"memoryMiB":8192}`. It applies at the **next appliance start**, not now, and never changes the data disk. Look for `cpu.requested` in `HostStatus` until then. |
| `power` | `{"action":"shutdown"}`. `Applied` is recorded **before** the action runs, so you may lose your connection right after reading it. |
| `autostart` | `{"supported":true,"enabled":true,"mechanism":"launchd user agent","note":""}` |
| `prune-images` | `{"imageBytesBefore":N,"imageBytesAfter":M,"reclaimedBytes":N-M}`. Only unreferenced images; never volumes. |
| `lan-check` | `{"checkedAt":"...","results":[{"address":"192.168.1.20","reachable":true}],"note":"..."}`. It only shows this computer accepting connections on its own LAN addresses. It is **not** proof that other devices can reach it. |

### 5. Approve a registry (cluster administrator only)

```yaml
apiVersion: host.containersanywhere.dev/v1alpha1
kind: HostRegistryTrust
metadata: {name: local-registry}
spec: {endpoint: "127.0.0.1:30500", plainHTTP: true, description: "in-cluster Distribution registry"}
```

`endpoint` is a loopback or node IP (or `localhost`) with a port. No names, wildcards, schemes or paths.
`plainHTTP` must be `true`; HTTPS with a private CA is not supported yet. `status.phase` is `Trusted`,
`Rejected` (`reason`: `InvalidEndpoint`, `Duplicate`, `Unsupported`) or `Revoked`. Two resources claiming one
endpoint are both rejected. Deleting the resource revokes trust. Typical use: a Distribution `registry:2` with a
NodePort service, then pull `127.0.0.1:<nodePort>/<image>` from a Deployment. Workloads get read-only access and
cannot create trust. The registry is reachable on the node's interfaces; the VM firewall does not expose it to the LAN.

### 6. What a manager must not assume

- Approval is human and can take any amount of time. Never block startup on it; show the request as pending.
- A resize only changes the next start. Do not report the new size as live.
- Only `lan-check` is automatic. Every other type waits for a person.
- Facts describe *this* host. A restored or replacement host starts with its own facts.

## How to add another host API (for the remaining issues)

The pattern is deliberately repetitive so each piece is small and testable without a VM:

1. Types, validation and the phase rules go in `internal/hostapi` (portable Go; tests use a fake `kubectl` Runner).
2. The CRD, roles and admission policy go in `platform/hostapi.yaml` (app level, no appliance release).
3. Anything only the host knows goes into `Facts` in `host_api.go`; anything that needs a human becomes a new
   `HostRequest` type, applied in `host_api.go` `apply()` and approved in the desktop panel (`desktop.go`) or with
   `containers-anywhere requests`.
4. The guest agent endpoints live in `internal/hostapi` (`Routes`) and `internal/guest/hostapi_linux.go`.
5. Acceptance goes in `scripts/host_api_acceptance.py`, run with `appliance.yml -f extended=true`.

Rules that must hold for every new API: workloads read and request only; keys never enter the cluster; status
reports recoverability and truth (last completed, last verified, age, degraded reason), never only "a job ran";
missing host feedback becomes `Unknown`; nothing is specific to any one application.

**Existing pieces the backup issues can build on** (#14, #15, #16 are not started): `containers-anywhere backup` and
`restore` already make an age-encrypted whole-VM cold backup and restore it only into an empty directory
(`storage.go`); the update transaction already retains verified recovery points with durable journals under
`<state>/updates` (`update_transaction.go`, `swap.go`); the lifecycle lock prevents two writers on one disk.
Design questions still open for a host destination with an S3 endpoint: where the S3 server runs and how the guest
reaches it (this is a new host-to-guest trust boundary), where the writer and retention credentials live (the
writer in a cluster Secret, the retention credential only on the host), and the staging-versus-complete object
protocol. Those need a design review before code.

## Status of the issue set

| Issue | State |
| --- | --- |
| #11 `HostStatus` | Implemented. |
| #19 update state, read-only | Implemented. The *request to prepare* a candidate is not. |
| #13 CA and LAN status | Implemented, including the limited LAN self-check. Firewall guidance for each OS is not yet written. |
| #12 resource sizing | Implemented. |
| #10 registry trust (option A) | Implemented. Relies on containerd reloading `hosts.toml` without a restart. |
| #17 autostart and power | Autostart, crash restart and power requests implemented. **Not done:** keep-awake, suspend/wake and network-change handling. |
| #18 logs, accounting, prune | Persistent bounded journal, sizes in `HostStatus`, and approved prune implemented. There is no separate `HostLogs` resource; the summary is inside `HostStatus`. |
| #14 `HostDestination` / S3 endpoint | Not started. |
| #15 `HostRecoveryPoint` / schedules | Not started. |
| #16 quiesce hooks, kit, restore plan and drill | Not started. |

**Unverified until it runs on real hosts:** the per-OS autostart entries, the persistent
journal symlink, containerd's reload of `hosts.toml`, the exact identity that K3s's admin
kubeconfig presents to the admission policy (`system:admin`), and everything on macOS,
Windows and ARM64. CI exercises the bridge end to end on Linux AMD64 under KVM; the
wider checks (approval flow, immutability, registry push/pull) run with
`smoke.py --extended`.
