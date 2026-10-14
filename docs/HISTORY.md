# Project identity and archive

Canonical repository: https://github.com/tellmeY18/containers_anywhere

Containers Anywhere is an application-agnostic platform. It does not include a
CARE deployment preset, application catalog, or CARE-specific onboarding.

## Preserved CARE Anywhere history

The original CARE product is preserved at `archive/care-anywhere`, commit
`24b4f4341f4828dc3389e12bbc9e0e757a361e97`. That branch contains the native CARE
guest, frontend, build configuration and original documentation. Existing release
tags and assets remain historical records; they are not Containers Anywhere
updates. The legacy `main` branch remains at that same original commit.

The default branch is `containers-anywhere`. Its Go module already uses
`github.com/tellmeY18/containers_anywhere`. Display name: **Containers Anywhere**;
CLI and state-directory name: `containers-anywhere`.

## Compatibility through the rename

GitHub redirects the former `tellmeY18/care_anywhere` repository URLs. Alpha 42
contains immutable download URLs under that former name; retain the redirect and
the existing release assets. Never recreate a repository at the old address.
New packages derive their download URLs from the renamed repository in CI.
Renaming the repository does not change existing archive hashes or data identity.

Existing CARE data is incompatible with Containers Anywhere. Do not move it into
the new state directory. There is no in-place migration or cross-product restore.

The internal `clinic.lock` filename is retained for compatibility with released
launchers sharing the same state. Renaming a lock without a transition protocol
could allow old and new launchers to write the disk concurrently. Historical
internal certificate labels are also not user-facing product branding.

CARE Clinic attribution and copyright remain in THIRD_PARTY_NOTICES.md and reused
source files. Product rebranding does not remove third-party provenance.
