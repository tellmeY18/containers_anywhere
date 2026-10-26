# Independent component releases

The host app and appliance have separate SemVer identities: `app-v0.1.0` and
`appliance-v0.1.0`. `VERSION` is the app version; `APPLIANCE_VERSION` is the
appliance version. A version identifies one tested source/artifact set, never a
mutable channel. These workflows do not publish automatically.

The 0.1.0 releases are beta releases, published with ordinary SemVer tags and
without GitHub's prerelease flag. They are not production-ready; production
go-live is planned for v1.0.0. The user has explicitly authorized this first
independent release sequence with the deferred capabilities and unverified
platforms listed below. Until stability is explicitly declared, do not build or
run legacy/alpha compatibility, migration matrices or old-release preservation
drills. Existing data must still fail safely, without automatic deletion or
migration. App/appliance independence applies to the current supported component
contract, not an old-version support promise.

Robust current-design update flow is necessary and a priority for 0.1.0, not
optional backward-compatibility work. Installed-component selection and
acceptance proving normal app replacement does not reset data remain here.
Transactional appliance update implementation is a separate workstream; this PR
does not replace it with a reset-only upgrade design. Production is planned for
v1.0.0, and backward compatibility starts only when the user declares stability.

The integrated `develop` defaults provision Kite, not CNCM/CARE, and use an
explicitly configured workload gateway. The `care-mvp` branch is untouched.
The WorkloadPublication API manages Caddy/Traefik, mDNS and local CA or HTTP
publication for authorized workload service accounts. Native host
supervision/resource/health handling and scheduled off-device recovery are
deferred. The 0.1.0 beta includes explicit operator-controlled appliance update
transactions, with operational limits and platform acceptance gaps recorded in
the release notes; it makes no production reliability, HA or PITR claim.

## App replacement versus appliance update

A packaged app embeds exact HTTPS URLs and SHA-256 pins for a recommended
appliance and QEMU runtime. The recommendation is only for **new clusters**.
An existing cluster's `release.json` remains authoritative: the app first finds
and fully hashes that exact appliance in the local cache, without consulting
the new recommendation. New downloads also record `appliance.json`, allowing
re-download of the original pinned archive if its cache is lost.

An unsupported protocol or capability set, without a supported update path,
requires a fresh empty state directory;
the launcher does not automatically delete old data. No alpha migration or backup
compatibility acceptance is provided. Download receipts support the selected
current appliance; they are not a recovery kit and are not included in backups.

QEMU has an independent content-addressed cache at `runtimes/<sha256>/qemu`.
An app build can reuse QEMU from a different explicit component release; updating
the app or recommendation does not overwrite a previously cached runtime.
Trusted local `--bundle` usage retains the sibling `qemu/` convention.

Protocol 1 guarantees status, logs, shutdown and the authenticated Kite proxy.
Current appliances declare these capabilities explicitly, with the gateway
capability additionally advertised by their status response. An unknown protocol
or missing declared core capability is rejected, including manifests without
capabilities. Compatibility is
established by testing the selected app/appliance/runtime tuple, not by comparing
SemVer patch numbers. A patch-compatible control protocol **never authorizes
changing the appliance attached to an existing disk**, migration, downgrade or
automatic rewind after workloads have accepted external writes.

## Build inputs and acceptance

The guest binary is `cmd/guest`, built only from `internal/guest`,
`internal/boottls`, `internal/protocol`, `internal/atomicfile` and the Go dependency
lock files. Root host Go files and desktop assets are not Nix guest inputs.
Appliance changes use Nix; app-only builds never run Nix or restage QEMU.
Shared protocol/dependency changes intentionally remain guest build inputs.

`alpha.yml` now runs source checks only. `appliance.yml` builds both architectures,
stages QEMU, and packages a test app for all four supported host targets.
`app.yml` consumes already-published appliances and runtimes, resolving their
publisher release descriptors once into an immutable Actions dependency artifact.
Every package job hashes downloaded bytes against that same lock.

Before the first component publication, `appliance.yml` additionally locks its
accepted, checksummed same-run artifacts and replays the app-only path across all
four hosts, without Nix or QEMU staging. These candidate locks identify their
source and Actions run and are explicitly rejected by release promotion.
Candidate-artifact acceptance exercises the same pinning, packaging and selection
logic but is **not actual published-dependency acceptance**: GitHub release
descriptor resolution and first-run public downloads still require an approved
published component. That gap must be recorded until a real tuple is accepted.
On actual `app.yml` builds, Linux acceptance additionally starts the packaged app
in a fresh state **without `--bundle`**, forcing the Go launcher's public pinned
appliance and QEMU downloads, then checks the selected manifest and download
receipt. The candidate path must not claim that public-bootstrap result.

Both paths run Go vet/race tests and build macOS ARM64, Linux AMD64/ARM64 and
Windows AMD64 packages. Linux AMD64 KVM acceptance exercises Kite authentication,
a PVC-backed workload, restart, replacement by an app with an unreachable different
appliance recommendation, and encrypted backup/restore. The replacement must retain
the original appliance identity and workload marker without downloading the new VM.
The WHPX machine/CPU configuration is replayed under KVM.

Windows TCG boot acceptance remains disabled because it exceeds hosted-runner
readiness limits. Windows WHPX, Linux ARM64 KVM and physical macOS boot acceptance
are **not** established by packaging or compilation. They remain unverified beta
limitations, not evidence of platform acceptance or production readiness.

## Maintainer usage

Run builds on `develop` after review. Appliance PRs also run their build/acceptance
path when guest or release machinery changes. Host-only PRs run source checks;
dispatch the app workflow for the intended published compatibility tuple.

```sh
# Appliance candidate (no release/tag mutation):
gh workflow run appliance.yml --ref develop

# App-only candidate; reuses the exact released VM and runtime:
gh workflow run app.yml --ref develop \
  -f appliance_tag=appliance-v0.1.0

# Optionally reuse QEMU from an earlier appliance release:
gh workflow run app.yml --ref develop \
  -f appliance_tag=appliance-v0.1.1 -f runtime_tag=appliance-v0.1.0

# After a successful build, promote its artifacts to a NEW draft:
gh workflow run release.yml --ref develop -f build_run_id=<run-id>
```

Until the first independent appliance release is published, app-only builds have
no published dependency to resolve. Validate initial candidates using the appliance
workflow's test-app packages and subsequent candidate-app reuse jobs. Do not
manufacture a public 0.1.0 dependency or reuse an alpha tag to bypass published
dependency acceptance.

## Ordered first-component publication and acceptance

These are the authorized operator steps for the first independent beta release.
There is no circular dependency: build/test the appliance with a candidate app
first, publish the appliance, and only then build the public-dependent app.

1. Integrate the reviewed source into `develop`, including the chosen networking
   scope. Set `VERSION` and `APPLIANCE_VERSION` independently to `0.1.0` and record
   the exact commit. Confirm neither release tag exists. Run all required CI and
   fresh-install acceptance gates. Record physical-platform and deferred-feature
   limitations explicitly; compilation and packaging do not establish platform
   boot acceptance.
2. Dispatch `appliance.yml` on `develop`, record its run ID and `head_sha`, and
   require a successful first attempt including both guest derivations, four
   packages and the full candidate-app reuse pass. Retain the artifacts and logs.
   The run must contain the final intended source; a PR run cannot be promoted.
3. Dispatch `release.yml` with that appliance build run ID. It creates **only**
   `appliance-v0.1.0`, targeting that tested commit, with two appliance archives,
   three QEMU archives, their five `.sha256` files and `appliance-release.json`
   (11 assets). Verify the draft's source, hashes, version, protocol and exact
   asset set. Do not rebuild assets.
4. Publish the complete appliance draft without making it the application's
   latest release:
   `gh release edit appliance-v0.1.0 --repo tellmeY18/containers_anywhere --draft=false --latest=false`.
   Enable repository release immutability before publication where available.
   Once published, never move the tag or replace bytes.
5. Run `python3 scripts/components.py resolve --appliance-tag appliance-v0.1.0
   --output <new-lock-path>` with publisher read access. This must resolve an
   actual non-draft GitHub release, verify its descriptor against the tag's source
   commit, and record its release ID and fixed artifact hashes. Then dispatch
   `app.yml --ref develop -f appliance_tag=appliance-v0.1.0` (the default runtime
   source is that same appliance release). Record the app run ID and commit.
6. Require all four app package jobs to pass against those public pinned bytes,
   including Linux KVM `--download` bootstrap into an empty state with no installed
   QEMU, current protocol checks, generic PVC workload, restart/app replacement
   and encrypted restore. Verify the log contains
   `PASS: actual public pinned appliance/runtime bootstrap into fresh state`.
   This is the **actual published-dependency acceptance** absent from candidate
   testing. A download, descriptor or boot failure blocks app promotion; fix the
   app and run again, or use a new appliance version if its published bytes are bad.
7. Promote the successful first-attempt app run through `release.yml`. Inspect
   `app-v0.1.0`: four installers, four `.sha256` files and `app-release.json`
   (9 assets). Its dependency lock must match the accepted published release IDs,
   source and hashes and must not contain `candidate`. Packaging alone is not
   physical installer/boot acceptance; list unverified platforms as beta
   limitations rather than implying they passed.
8. Publish the complete app draft:
   `gh release edit app-v0.1.0 --repo tellmeY18/containers_anywhere --draft=false --latest`.
   Download the public installers/checksums, compare them with `app-release.json`,
   and verify the public app's fresh-install path. Record both component IDs,
   commits, build/acceptance runs, runtime hashes and any platform limitations in
   the release notes. Announce the app release, not an appliance-only download.

Candidate CI is not a substitute for actual published-dependency acceptance in
steps 5-6. Failure after publication never permits clobbering an identity or
deleting state; a corrected component needs a new version and fresh acceptance.
If a draft/tag already exists after an interrupted promotion, follow the
non-overwrite rule below.

Promotion requires a successful first-attempt `workflow_dispatch` run on `develop`
from `app.yml` or `appliance.yml`. PR builds, CARE builds, legacy alpha builds,
failed runs and mixed-attempt reruns cannot be promoted. Version and source come
from the tested commit, not the publisher checkout. Checksums, complete platform
asset sets and appliance identities are verified before an atomic new tag is
created. Nothing is rebuilt; existing tags/assets are never replaced.

An appliance release contains both VM archives, three QEMU archives, checksums,
and `appliance-release.json`. An app release contains only four installers,
checksums, and `app-release.json` with the tested dependency lock. Runtime artifact
identity is its fixed URL and SHA-256, not the app version. The dependency resolver
also accepts a `runtime-v<semver>` descriptor with the exact three runtime assets;
there is no standalone runtime publication workflow in this milestone.

Promotion prepares a **draft only**. On interruption after tag creation, retain
the partial draft/tag for inspection; do not rerun with overwrite or move the tag.
Use a new version and a new successful build for another promotion. Before
publication, inspect the complete asset set and release evidence. Publish 0.1.0
only after all required CI and published-dependency acceptance gates pass;
document the accepted beta scope and unresolved platform/deferred-feature
limitations.

## 0.1.0 beta scope and limitations

The launcher GUI can prepare an update from the pinned appliance recommendation,
activate or retry it, commit it, cancel it before execution, and report status.
Selecting a trusted local candidate and exporting a recovery point are CLI-only.
Once activated, workloads can immediately cause inbound or outbound side effects;
there is no isolated validation environment or automatic disk rewind. Commit
checks the appliance contract and requires the operator to attest that workloads
and persistent volumes are consistent. Platform health does not validate
application behavior.

The update path supports only matching architecture and disk geometry,
`var-lib-v1`, K3s/Kite versions, and the current workload-publication protocol.
It does not migrate arbitrary K3s/Kite data. Recovery requires a stopped VM, an
explicit data-loss acknowledgement, and an empty destination outside the original
state directory. The original and failed disks are retained; recovery fences the
source and starts no VM. There is no automatic rewind of the current disk.
Update rollback points are unencrypted, local full logical-disk copies with no
pruning, so they consume disk space and are not off-device backups or workload
PITR. Manual encrypted cold backup and restore remain available.

Native host service supervision, operational health checks and scheduled
off-device recovery are deferred. Physical macOS boot, Windows WHPX, Linux ARM64
KVM, power-loss behavior, and multi-device LAN/client CA trust have not been
verified. The beta is a single-node local environment and makes no production
reliability, high-availability or PITR promise.

## Trust

No custom signing-key provisioning is required here. The explicit trust anchor is
the `tellmeY18/containers_anywhere` GitHub publisher account, its protected source
and workflow execution, and the app binary obtained from that publisher. Component
resolution rejects other publishers and mutable/unversioned references. HTTPS and
fixed archive hashes authenticate downloads relative to that trusted build.
Archive path/link rejection and data locks remain in force.

Release metadata is not independently signed. A compromised publisher can issue a
malicious new app or descriptor; hashes are not protection against that authority.
Use GitHub release immutability and protected publication permissions operationally;
the workflows themselves refuse to reuse tags or clobber assets. macOS ad-hoc
signing and optional configured notarization are unchanged; Windows remains
unsigned. Publisher-signed update channels and automatic appliance upgrades are
not implemented.
