# Independent component releases

The host app and appliance have separate SemVer identities: `app-v0.1.0` and
`appliance-v0.1.0`. `VERSION` is the app version; `APPLIANCE_VERSION` is the
appliance version. A version identifies one tested source/artifact set, never a
mutable channel. These workflows do not publish automatically.

This is the first release-engineering milestone, **not completion or authorization
to publish final 0.1.0**. Universal profile separation, native host supervision and
resource/health handling, off-device cold recovery and replacement-host drills,
journaled appliance updates, and physical platform acceptance remain release gates.
The existing CNCM/CARE bootstrap in this source is unchanged by this milestone.
The `care-mvp` distribution and its installed workload state are not modified.

## App replacement versus appliance update

A packaged app embeds exact HTTPS URLs and SHA-256 pins for a recommended
appliance and QEMU runtime. The recommendation is only for **new clusters**.
An existing cluster's `release.json` remains authoritative: the app first finds
and fully hashes that exact appliance in the local cache, without consulting
the new recommendation. New downloads also record `appliance.json`, allowing
re-download of the original pinned archive if its cache is lost.

Legacy installs without a download receipt retain their original cached bundle.
If it is missing, start with the exact original `--bundle`; do not substitute
the latest appliance. Encrypted alpha backups remain readable and still require
the exact original appliance after restore. Download receipts are not a recovery
kit and are not included in the legacy backup format.

QEMU has an independent content-addressed cache at `runtimes/<sha256>/qemu`.
An app build can reuse QEMU from a different explicit component release; updating
the app or recommendation does not overwrite a previously cached runtime.
Trusted local `--bundle` usage retains the sibling `qemu/` convention.

Protocol 1 guarantees status, logs, shutdown and the authenticated Kite proxy.
New appliances declare these capabilities explicitly, with gateway and CNCM
capabilities additionally advertised by their status response. Legacy protocol-1
manifests without capabilities retain their historical core contract. An unknown
protocol or missing declared core capability is rejected. Compatibility is
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

Both paths run Go vet/race tests and build macOS ARM64, Linux AMD64/ARM64 and
Windows AMD64 packages. Linux AMD64 KVM acceptance exercises Kite authentication,
a PVC-backed workload, restart, replacement by an app with an unreachable different
appliance recommendation, and encrypted backup/restore. The replacement must retain
the original appliance identity and workload marker without downloading the new VM.
The WHPX machine/CPU configuration is replayed under KVM.

Windows TCG boot acceptance remains disabled because it exceeds hosted-runner
readiness limits. Windows WHPX, Linux ARM64 KVM and physical macOS boot acceptance
are **not** established by packaging or compilation. Those are remaining gates.

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

# Once authorized, promote successful build artifacts to a NEW draft:
gh workflow run release.yml --ref develop -f build_run_id=<run-id>
```

Until the first independent appliance release is published, app-only builds have
no published dependency to resolve. Validate initial candidates using the appliance
workflow's test-app packages and subsequent candidate-app reuse jobs. Do not manufacture a public 0.1.0 dependency or reuse
an alpha tag to bypass the remaining release gates. An explicitly versioned
prerelease can bootstrap this sequence only with separate publication approval.

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
Use a new version and a new successful build for another promotion. Before any
separately approved publication, inspect the complete asset set and release
evidence. Do not publish 0.1.0 while its remaining gates are open.

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
