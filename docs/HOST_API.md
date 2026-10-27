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

The agent advertises the `host-api-v1` capability, so an appliance without it is
refused by an app that needs it.

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
| `resources` | `cpus` 2–64, `memoryMiB` 2048–262144 | Saved as the size for the **next start**. Refused if it would oversubscribe the host. The data disk is never resized, shrunk or recreated. |
| `power` | `action`: `shutdown` or `restart-appliance` | Recorded as Applied **before** it acts, then runs. Never replayed after a launcher restart. |
| `autostart` | `enabled` | Installs or removes the per-user launch entry. |
| `prune-images` | none | `crictl rmi --prune` in the guest: unreferenced images only, never volumes. Reports bytes reclaimed. |
| `lan-check` | none | Needs no approval. Dials this computer's own LAN addresses on the HTTPS port. **Not proof that other devices can reach it.** |

Phases: `Pending` → `Approved` → `Applied` or `Failed`; or `Rejected`, or `Expired` after
24 hours. A terminal phase never changes. The spec is **immutable** after creation
(admission policy), so what a person approved is exactly what is applied. A decision
lands only on the object with the same `uid` that the launcher saw. Status is written
only by the bridge. Grant `containers-host-requester` in the requester's namespace.

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
