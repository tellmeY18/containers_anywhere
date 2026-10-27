# Containers Anywhere 0.3.0 (beta)

Beta, not production-ready. Production is planned for v1.0.0. The app and appliance are
released independently (`app-v0.3.0`, `appliance-v0.3.0`). K3s is unchanged (1.35.8).

## What's new

- **Host API for workload managers.** Application-agnostic resources in
  `host.containersanywhere.dev/v1alpha1`, delivered by the app and reconciled by the
  appliance over the authenticated host channel. No host credential is ever handed to a
  pod, and only a person in the launcher approves anything. See `docs/HOST_API.md`.
  - `HostStatus`: read-only host truth (versions, CPU and memory against the host total,
    data-disk usage and a disk-pressure flag, host free space, clock skew, autostart,
    update state, root-CA fingerprint and URL, ports, storage use). Facts that stop
    arriving become `Unknown`.
  - `HostRequest`: approval-gated requests for a resize (applies at the next start,
    refused if it would oversubscribe the host), power (shutdown or restart), autostart,
    pruning unused images, and a LAN self-check. Approve or reject in the desktop panel
    or with `containers-anywhere requests`. A request cannot be edited after creation.
  - `HostRegistryTrust`: an administrator approves an in-cluster registry endpoint so
    the container runtime can pull from it, with no appliance change.
- **Autostart with crash restart** for macOS (launchd), Linux (systemd user unit) and
  Windows (scheduled task): `containers-anywhere autostart`.
- **Guest logs persist** on the data disk, capped at 64 MiB and 14 days.
- **An older appliance still works.** `host-api-v1` is an optional capability, so the
  published 0.2.0 appliance still runs under this app and can be updated; the host API
  is simply off until the appliance is updated.

## Updating from 0.2.0

Install the 0.3.0 app. Your cluster keeps its 0.2.0 appliance and its data. To update
the appliance, K3s is identical, so either path is allowed:

- the desktop panel's **Prepare appliance update** (the full transaction: a cold copy of
  the data disk, then Activate and Commit with your own validation), or
- `containers-anywhere update --operation swap --state <dir>` (no data copy, automatic
  fallback to the old root). This is command-line only for now.

## Verification

The full end-to-end lifecycle and the wider host API checks passed on Linux AMD64 under
KVM at commit `6bb334f`, whose product code is identical to the release commits: fresh
install, Kite login, a PVC workload, a workload-account publication, the demo over
HTTPS, app replacement, encrypted backup and restore, a root swap with automatic
fallback from an unbootable root, request approval and refusal, immutability, and a push
and pull through an approved registry.

**Not verified:** boot on Linux ARM64, macOS or Windows WHPX; the per-OS autostart
entries on real hosts; and the update from a published 0.2.0 appliance to 0.3.0 (covered
only by unit tests until it is run on real hardware).

## Not included

Backup destinations with an S3 endpoint, scheduled recovery points, quiesce hooks, the
recovery kit, restore plan and restore drill, a request to prepare an update candidate,
keep-awake, and suspend/wake handling. A root swap needs an identical K3s version; a K3s
patch release uses the full transaction. A K3s minor change is not supported.

## Limits carried over

- First boot needs internet (Kite, Traefik and the demo are pulled at first start).
- The data disk is ext4 and permanent once installed.
- Retained roots and recovery points are not pruned automatically.
- An older app can downgrade the platform workloads on an existing cluster, since every
  start re-applies the manifests that app embeds.
- Default CI no longer runs the full end-to-end lifecycle; it was run for this release
  as described above.
