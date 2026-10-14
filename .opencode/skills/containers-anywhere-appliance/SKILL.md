---
name: containers-anywhere-appliance
description: Use when building, boot-testing or debugging the Containers Anywhere K3s appliance, VM launcher, storage or recovery paths. Covers CI builds, platform acceptance and persistent-data contracts.
---

# Containers Anywhere appliance workflow

Read README.md, AGENTS.md, docs/DISASTER_RECOVERY.md and ROADMAP.md. The default
branch is containers-anywhere; archive/care-anywhere holds the retired product.
Do not use its native CARE, Django, Firecracker or layered-update instructions.

## Build and verification

- Format Go with gofmt and Nix with nixpkgs-fmt/treefmt.
- Compilation, race tests, appliance builds and packaging run in GitHub Actions.
- With authorization, push the intended jj bookmark or dispatch alpha.yml on
  containers-anywhere. Inspect every required job, not only the build summary.
- Linux AMD64 KVM smoke tests deploy through Kite, reboot and cold-backup/restore
  while checking the same account and non-regenerable PVC marker.
- Windows hosted CI uses explicit TCG; this is not physical WHPX acceptance.
- macOS signature/packaging checks are not Virtualization.framework boot acceptance.
- Real hardware checks use CI-built artifacts. Never claim a platform booted
  merely because it cross-compiled.

## Local artifact acceptance

Download the matching appliance and launcher from one tested build. Extract only
manifest.json, kernel, initrd, system.img and data.img; never the Nix build closure.
On Linux/Windows put the matching verified qemu directory beside bundle.
Use a new, isolated state directory for every test:

```sh
python3 scripts/smoke.py --binary /path/to/containers-anywhere \
  --bundle /path/to/bundle --state /path/to/new-test-state --restore
```

For manual diagnostics use status/logs/stop with the same --state. Inspect
console.log and guest journal/pod/event output. Never disable TLS, checksums or
locks to make a test pass. No forced power-off of user data during acceptance.

## Recovery contracts

All persistent guest state belongs under /var/lib. Cold backup requires the VM
stopped and the exclusive host lock. Restore accepts only an empty destination;
preserve the original disk and exact original appliance. Do not rename the legacy
clinic.lock filename without a compatibility protocol for released launchers.

PITR and automatic backup are roadmap features, not current functionality. A
cluster datastore snapshot does not protect PVs; a volume copy does not prove
application consistency. Recovery changes require corruption, interruption,
capacity, key-loss and isolated restore acceptance appropriate to their scope.

## Publication

Use the release workflow to prepare assets from one successful run. Publish only
when authorized, after verifying exact asset names, hashes and source commit.
Keep immutable download URLs and existing release artifacts alive through renames.
Canonical repository: https://github.com/tellmeY18/containers_anywhere.
