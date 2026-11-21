# Containers Anywhere app 0.6.0-rc.1 (release candidate)

Release candidate for the first public beta. App only: it recommends the appliance 0.5.0 that is already
published, so the appliance, K3s and your data format are unchanged. Not production-ready.

## A new launcher panel

The launcher panel is redesigned on the [letsimpl.fyi brand system](https://brand.letsimpl.fyi), desktop first.
See [DESIGN.md](../DESIGN.md).

- A side rail with five sections (Cluster, Workloads, Recovery, Store, Updates), Diagnostics and display settings.
- One status headline with one primary action: Start, Open Kite, Retry, Set up, or Restart.
- Recovery points and store reviews are shown as receipts. Every browser `confirm()` and `prompt()` is replaced by a
  dialog that repeats what will happen. Risky actions are dashed and carry an icon, not just a colour.
- Light, dark, large text, high contrast and reduced colour, from the Display button.
- Black and white with four status colours that always come with a word and an icon.
- The panel is served from files embedded in the app with a strict content security policy (no inline script or
  style). Fonts are bundled; nothing is loaded from the internet.

No endpoint, capability or approval rule changed. Approving requests, restoring, exporting the recovery key and
enrolling destinations remain launcher-only, behind the same token.

## Verification

- Vet and race tests for the panel package and the launcher's panel tests pass locally.
- CI builds and packages all four hosts. The default (light) CI path was used; the full end-to-end lifecycle and the
  `extended` checks were **not** run.
- Not yet verified: the panel against a running cluster on macOS, Windows and Linux hardware. That is the purpose
  of this candidate.

## Limits

Unchanged from 0.5.0: recovery is experimental, cloud backup destinations are placeholders, there is no
point-in-time recovery, and VM boot on Virtualization.framework and WHPX hardware is unverified. A new empty state
directory is still needed when coming from releases before 0.5.0.
