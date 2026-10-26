# Explicit current-contract appliance updates

Implemented in source, pending the platform acceptance gates below. App replacement
continues to retain the selected appliance and data. Installing an app does not
install its recommended appliance into an existing cluster.

This is an **operator-controlled update**, not an isolated trial boot. The candidate
can run cron, webhooks and external writes immediately, even before any inbound
gateway becomes ready. There is **no automatic disk rollback**, before or after
commit. Gateway publication authorization remains unchanged.

## Supported boundary

Both bundles must satisfy the launcher's current protocol/capability requirements,
declare `data_contract: "var-lib-v1"`, and have identical architecture, data disk
geometry, K3s version and Kite version. Version ordering/SemVer is not used to infer
data compatibility. This deliberately supports current-contract image changes,
not K3s/Kite schema migrations. Missing or different contracts are rejected without
deleting data; use a new empty state directory for unsupported changes.

Release downloads use the app's embedded HTTPS URL and SHA-256 pins. A caller
supplying `--bundle` or `--original-bundle` is explicitly supplying a trusted local
artifact; member hashes detect corruption, not publisher authenticity. No custom
signing/channel infrastructure or arbitrary shell updater is added.

## Operator flow

```sh
# Gracefully stops a running cluster and takes the same exclusive lifecycle lock.
# Without --bundle, stages this app's pinned recommendation, not a mutable latest.
containers-anywhere update --operation prepare --state /cluster \
  --bundle /trusted/candidate/bundle --original-bundle /selected/bundle
containers-anywhere update --operation status --state /cluster

# Foreground VM session, equivalent to serve. Other commands use another terminal.
containers-anywhere update --operation activate --state /cluster \
  --accept-external-effects

# Verify actual workload/PVC markers, credentials/login and any application-native
# consistency checks through the candidate, then explicitly attest that validation.
containers-anywhere update --operation commit --state /cluster \
  --accept-workload-validation
```

The authenticated loopback desktop panel offers prepare, activate/retry, commit
and pre-execution cancel with confirmations. Preparation may take several minutes
and temporarily stops the cluster. CLI status exposes the durable phase; normal
startup cannot bypass a pending operation. The activation foreground session keeps
the lifecycle lock until the VM exits. Commit runs through that session's separate
authenticated control capability, checks live VM/K3s/Kite health and protocol, and
records the operator's workload-validation attestation and timestamp. It does not
pretend to automatically validate arbitrary applications.

Preparation retains a full cold disk, selected manifest, gateway configuration and
Caddy CA/keys, both exact bundles, preparing launcher, its pinned Caddy helper when
packaged, and verified QEMU runtime on Linux/Windows. Copies are private, exclusive,
file-synced and directory-synced before the prepared journal is published. Storage
preflight budgets full logical disk size plus all artifacts and a 256 MiB reserve;
the source sparse disk does not imply a cheap recovery copy. Recovery contents are
hashed and checked before activation/export. The preparing launcher is the current
supported app, not a reconstructed historical launcher.

**This unencrypted local recovery point shares the host failure domain.** It is
not off-device protection or workload PITR. Keep normal encrypted off-device
backups and separately stored keys. No automatic retention pruning is implemented;
successful commit does not delete old recovery points or failed candidate evidence.

## Durable state machine

`update.json` is the launch gate; `release.json` remains the sole selected identity.
Each operation has a unique `updates/transaction-*` directory.

| Phase | Selected identity and permitted next operation |
| --- | --- |
| `preparing` | Original unchanged. Resume preparation/download or cancel. Incomplete copies remain evidence; changed/partial retained files require cancel and a fresh preparation. |
| `prepared` | Original unchanged, complete recovery point. Explicit activation or cancel. Ordinary start is blocked so the cold point cannot silently become stale. |
| `activating` | Journal precedes selection publication and removal of the old download receipt. A crash may leave either identity; ordinary start is blocked. Explicit activation reconciles to the recorded candidate. |
| `candidate-running` | Persisted **before** VM boot. Candidate is selected; the current disk may contain candidate writes even without a health response. Explicit retry uses that same disk; commit requires a live candidate and validation. |
| `committed` | Ordinary launch selects the verified retained candidate. All recovery material remains. |
| `cancelled` | Only reachable before activation. Original identity/disk remain selected; evidence is retained. |
| `exporting` | Gate on a partial recovery destination. It cannot be cancelled into a bootable state. Preserve it and retry into another new directory. |
| `recovery-exported` | Source remains blocked with current/failed disk untouched; separate cold copy exported. No VM is automatically started. |

Publication is idempotent under the exclusive lock, including a crash immediately
after `release.json` publication. A corrupt/unknown journal or inconsistent selected
identity fails closed. Retrying candidate execution retains previous console/gateway/
launcher logs before another VM boot. The phase `candidate-running` means execution
*may have occurred*, not proof of health or proof a process is still alive.

## Deliberate recovery, never in-place rewind

Stop the candidate. Inspect diagnostics and reconcile/fence any external effects
(payments, notifications, remote databases, object stores, scheduled jobs).
Only then request a cold export:

```sh
containers-anywhere update --operation recover --state /cluster \
  --destination /new/recovered-cluster --accept-data-loss
# Do not run the source and recovered identity together.
# After resolving outbound effects, use the retained exact bundle explicitly:
containers-anywhere serve --state /new/recovered-cluster \
  --bundle /new/recovered-cluster/recovery/bundle
```

The destination must not exist and must be outside the source state. Partial exports
are retained and fenced. The original/failed current disk is never replaced, renamed
away or removed. The exported disk intentionally excludes all writes after preparation;
the acknowledgement is not evidence that those writes or external effects are safe
to lose. The retained `recovery/launcher` (`launcher.exe` on Windows), Caddy helper
and `recovery/qemu` allow exact-artifact recovery without downloading a release.

## Acceptance and remaining gates

Deterministic tests inject interruption after each preparation copy/journal boundary
and each activation publication boundary. They cover lock exclusion, insufficient
space, incompatible contracts, corrupted/link artifacts, resumption, commit health/
attestation checks, retained CA/data identity, separate recovery and partial-export
fencing. Existing downloader tests cover partial transfers and checksum failures.

The CI KVM drill builds a second **distinct NixOS root image**, with unchanged
K3s/Kite/data contracts, not an alpha fixture or merely edited version string.
It creates a real PVC-backed workload and administrator, updates, validates marker/
login/CA continuity, explicitly commits, restarts, exports the old generation and
boots that disposable recovery. The normal KVM smoke also runs encrypted backup/
restore. These are disposable workloads without external business side effects;
they do not justify automatic recovery of arbitrary workloads.

Until that workflow passes, this is not boot-accepted. Physical Darwin ARM64,
Linux ARM64 KVM and Windows WHPX update/power-loss acceptance remain separate gates;
cross-compilation and Unix directory-sync tests do not establish them. Windows
uses the existing write-through atomic metadata helper; real storage/power-loss
behavior still needs platform evidence. Independent app release selection is covered
by the existing replacement-app smoke.

Next layers are tested K3s/Kite migration contracts, machine-enforced outbound
fencing for any automatic rollback proposal, application-specific validation
profiles, and off-device recovery management. None is claimed implemented here.
