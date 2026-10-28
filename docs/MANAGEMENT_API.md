# Management through the Kubernetes API

Direction, not a description of what is built. Items marked **built** exist today; everything else is planned.

## Goal

Everything a workload manager (CNCM, or any other cloud-native manager for another application) needs from the appliance
and the host is reachable through the Kubernetes API. Managers never get a bespoke endpoint, a host token or a shell.

## Rules

1. **Standard Kubernetes first.** Storage, snapshots, clones, quotas, scheduling, disruption budgets and events use the
   upstream APIs (CSI, `VolumeSnapshot`, PVC clone/expand, `ResourceQuota`, `PodDisruptionBudget`). **Built** for storage.
2. **A CRD only where something crosses the host boundary:** host disk, host network, host files, power, appliance updates.
3. **Every CRD follows the Kubernetes contract:** `spec` and `status`, `status.conditions`, `observedGeneration`, Events,
   `v1alpha1` until stable, reconciled by a platform workload that the app pushes (never baked into the appliance).
4. **Approval is a status condition.** A workload creates the request; the CR shows `PendingApproval`; a person approves in
   the launcher; it becomes `Approved` or `Denied`. **Built** for `HostRequest`.
5. **Launcher-only actions stay launcher-only** but their state is readable: key export, destination enrolment, cutover and
   fencing, deleting recovery points, activating an update.
6. **No per-application endpoints or defaults.** Everything is generic.
7. **The launcher panel becomes a client of the same objects**, so the API can never lack something the UI can do.
8. No host token ever reaches a pod. The bridge (vsock, per-boot mTLS) stays the only path to the launcher.

## Catalogue

| Group | Resource | State |
|---|---|---|
| `host.containersanywhere.dev` | `HostStatus`, `HostRequest`, `HostRegistryTrust` | **built** |
| `networking.containersanywhere.dev` | `WorkloadPublication` | **built** |
| `host.containersanywhere.dev` | `HostDestination` (read-only status; S3-compatible endpoint with separate writer and reader secrets) | planned, #14 |
| `host.containersanywhere.dev` | `HostRecoveryPoint`, `HostBackupSchedule` | planned, #15 |
| `host.containersanywhere.dev` | `HostQuiesceHook`, `HostRestorePlan`, `HostRestoreDrill` | planned, #16 |
| `host.containersanywhere.dev` | `HostRequest` types `power`, `autostart`, `prune-images`; `HostStatus` autostart, logs and storage accounting | **built** in 0.3.0; #17 and #18 stay open only for unverified acceptance (tracked in #20) |
| `host.containersanywhere.dev` | `HostUpdate` (prepare a candidate; activation stays launcher-only) | planned, #20 |
| storage | `VolumeAttributesClass`; ceiling, current size and host free space in `HostStatus` | planned |

## Trust model: temporary, for the beta line

CNCM is a first-class citizen of the platform for now and manages the cluster with owner-level privilege. This is a
deliberate shortcut to build and break things quickly, not the production design.

- CNCM may hold broad (cluster-admin class) rights in its own cluster. It is the one workload with them.
- Host-affecting actions still need a person to approve in the launcher; the cluster role does not change that.
- Management listeners stay loopback-only and authenticated; nothing here exposes the Kubernetes API, Kite or launcher
  control to the LAN.
- Every grant of this kind carries a `TEMPORARY(beta)` note so it can be found and removed.

### Before production (v1.0.0)

- Replace owner-level rights with least-privilege roles per manager, scoped to the manager's own namespaces, with admission
  policies as guard rails. The API groups above are the stable surface managers are granted.
- Replace Kite with an end-to-end management engine built for this platform (everything Kite does plus the cloud-native
  features above), as a client of this same API.
- Optional approval policies (for example, auto-approve disk growth to a limit) on top of the manual default.
- Promote CRDs from `v1alpha1` with conformance tests per resource.
