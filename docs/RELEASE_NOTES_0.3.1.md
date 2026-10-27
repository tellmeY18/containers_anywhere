# Containers Anywhere 0.3.1 (beta)

Beta, not production-ready. App-only release: the appliance is unchanged (`appliance-v0.3.0`,
K3s 1.35.8) and no appliance update is needed. Install over 0.3.0; cluster data is untouched.

## What's new

- **Store (proof of concept).** The desktop home screen lists deployments found on GitHub under
  the topic `containers-anywhere` that ship a manifest, and installs a reviewed one through the
  existing platform add-on path. **There is no trust mechanism yet**: every entry is unverified and
  installing one runs its publisher's code with whatever access its manifest asks for. The panel shows
  a risk summary and the manifest SHA-256, requires typing the repo name for high-risk entries, refuses
  a manifest that changed since review, and blocks reserved namespaces. No uninstall yet.
  See `docs/STORE.md` for the publisher convention, limits and trust roadmap.
- **Fix:** `http://<lan-ip>/root.crt` returned 403, so the root CA could not be fetched by address
  (and `rootCAURL` in `HostStatus` was wrong). It is now served for any host (GET/HEAD only).
- Docs: macOS Local Network permission and the hosts-file fallback for devices that cannot resolve `.local`.

## Verification

Unit tests (`go test -race`) and compile checks for Linux AMD64/ARM64 and Windows AMD64 pass.
CI boots the published bytes. The full end-to-end lifecycle (`e2e=true`) was **not** run for this release.
The Store has not been run against a live cluster.
