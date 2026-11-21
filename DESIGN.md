# Containers Anywhere design

**Status: v0.3, decisions recorded, awaiting go-ahead to implement.** Nothing here is implemented yet. Target release: 0.6.0, the first public beta.

Containers Anywhere is a flagship letsimpl.fyi product, so it **consumes** the
[letsimpl.fyi brand system](https://brand.letsimpl.fyi) (tokens v0.3.0) rather than
defining its own. This file records only how that system applies to the launcher
panel (`desktop.go`) and what we add on top. Where this file and the brand site
disagree, the brand site wins.

> v0.1 of this file proposed chaikadi-style yellow/blue/green crayons. The brand
> site dropped those in v0.3, so that proposal is withdrawn.

## What the brand says (inherited, not restated in full)

- **Black and white. Nothing has a brand colour.** Colour exists only to say what a
  state is, and never alone.
- Primary action is a **solid ink button**. Links are underlined ink; outbound links end
  in `↗`. Controls and key rules are 2px ink; corners are square; no shadows,
  gradients or translucency except the 50% scrim.
- Four **status fills** (Paul Tol light): `--danger #EE8866`, `--warning #EEDD88`,
  `--success #44BB99`, `--info #99DDFF`, always with `#111111` text. On the bare page
  use the `--*-ink` variants. Every state also carries a word, an icon and a distinct
  border shape (error: 8px left bar; warning: double border; info: hairline;
  destructive: dashed edge plus triangle icon).
- Type: **Atkinson Hyperlegible Mono** for the interface, **Courier Prime** for
  receipts only, Atkinson Hyperlegible Next for long prose (not needed in the panel).
  18px root, 560px measure, spacing 4/8/12/16/24/32/48/64, touch targets 44px minimum.
- Light `#FFFFFF` and dark `#000000`, following the OS, with `data-theme` override.
  Display settings `data-text="large"`, `data-contrast="high"`, `data-colour="reduced"`.
- Motion: 120ms states, 200ms sheets, nothing loops, nothing carries meaning alone.
- Navigation is a plain, always-visible list; no hamburger menu.
- The wordmark, logo, mark and favicon assets do not exist yet in the brand system.

## How we consume it

Vendor, do not link (the panel is loopback-only, must work offline, and its CSP is
`default-src 'self'`):

```
internal/desktopui/
  tokens.css        copied unmodified from brand.letsimpl.fyi/tokens/tokens.css
  base.css          copied unmodified from brand.letsimpl.fyi/css/base.css
  components.css    copied unmodified from brand.letsimpl.fyi/css/components.css
  fonts/            the three WOFF2 files base.css references (~56 KB)
  panel.css         only product-specific layout; uses brand tokens, adds no colours
  index.html, panel.js (+ small modules), icons.svg
  BRAND_VERSION     "0.3.0" and the date copied
```

Embedded with `go:embed` and served under `/assets/`. No Node, bundler or CDN. A Go
test checks the three brand files are byte-identical to the recorded hashes, so
drift is a deliberate update, not an accident. Fonts and brand CSS are credited in
`THIRD_PARTY_NOTICES.md` (fonts are SIL OFL; the brand CSS is AGPL-3.0, same as this repo).

Product classes use the brand's `ls-` components only: `ls-btn`, `ls-btn--primary`,
`ls-btn--danger`, `ls-btn--outbound`, `ls-card`, `ls-callout--*`, `ls-tag--*`,
`ls-chip`, `ls-field`, `ls-input`, `ls-sheet`, `ls-toast`, `ls-receipt`,
`ls-slot`. New product components are prefixed `ca-` and live in `panel.css`.

## Voice

Plain, short, honest. Lowercase wordmark. Sentence-case labels, short verbs, no
exclamation marks, no marketing. Say what happens to data next to the action ("Your
data stays on this computer."). The product is beta (0.x): say so in the footer and
never describe unbuilt recovery features as available.

Tagline: **Your Kubernetes. On this computer.**

## Panel structure

Desktop first. The panel is used in a window on a laptop or desktop, so it is a side
rail plus a wide content area, not a phone column:

```
+-- rail (16.5rem, sticky) --+-- content (up to 80rem total) -----------------+
| containers anywhere  beta  |  Status headline + tag        [primary action]  |
| Cluster                  1 |  one sentence of detail                         |
| Workloads                1 |  ---------------------------------------------  |
| Recovery                   |  +-- card ------------+  +-- card ------------+  |
| Store                      |  | Cluster            |  | Requests           |  |
| Updates                  ! |  +--------------------+  +--------------------+  |
| Diagnostics  [Display]     |                                                  |
| versions, letsimpl.fyi     |                                                  |
+----------------------------+--------------------------------------------------+
```

- The rail holds the text wordmark, a plain vertical list of sections with badges,
  Diagnostics and Display, and the footer (versions, "a letsimpl.fyi project", source).
- The status block is a band: headline, tag and sentence on the left, the one primary
  action on the right.
- Content is `ls-card`s in a two-column grid from 1100px up; recovery points are a
  grid of receipts; Store entries are a grid of cards. Dialogs are centred sheets.
- Below 1100px the grid is one column; below 800px the rail stacks above the content
  with a horizontal nav. Narrow windows are supported, not the target.
- The brand 560px measure is for prose. Controls, cards and receipts use the wider area;
  paragraphs inside cards stay readable because cards are at most half the content width.

The panel is a local app, not a content site, so client-side sections are allowed
by the brand's "unless the product is an app" rule. Navigation stays a visible list,
not a hamburger. On narrow windows it becomes the brand `ls-bottombar`.

### Status block and states

Always visible. The state is a word, an icon and a status tag, never a bare dot.

| State | Tag | Headline | Primary action |
| --- | --- | --- | --- |
| stopped | none, outlined | Stopped | Start |
| starting / preparing | `ls-tag--warning` | Starting | none; progress text |
| ready | `ls-tag--success` | Ready | Open Kite `↗` |
| updating | `ls-tag--warning` | Updating | none |
| setup needed (Windows) | `ls-tag--warning` | Setup needed | Set up this computer |
| restart needed (Windows) | `ls-tag--warning` | Restart needed | Restart Windows |
| error | `ls-callout--error` | Error | Retry |

Exactly one `ls-btn--primary` per screen. Destructive actions (Stop with unsaved
work is not destructive; Restore, Replace, Remove destination, Cancel preparation
are) use `ls-btn--danger` and a confirmation sheet.

### Sections

- **Cluster:** detail, Stop, versions, state directory with Copy, and pending
  workload **requests** (Approve / Reject, each repeating the ask in a sheet).
- **Workloads:** endpoint rows with `ls-tag` state and `↗` links; gateway
  diagnostics as `ls-callout--warning`. Empty state is an `ls-slot`.
- **Recovery:** restore points, backup destinations, the key block. Points render as
  **`ls-receipt`** (date, kind, appliance, volumes, restorable or reason). The
  "same disk as your data is not disaster recovery" warning is an `ls-callout--warning`.
  Copy states that this is not point-in-time recovery.
- **Store:** unverified catalogue. Each row has an `ls-chip--unverified` and a risk
  tag. "Review" opens an `ls-sheet` with the manifest summary as an `ls-receipt`;
  high risk requires typing the repository name in an `ls-input` inside the sheet.
- **Updates:** Update now first; the careful path (Prepare, Activate, Commit,
  Cancel) in a disclosure, each with its existing warning text in a sheet.
- **Diagnostics** (header link, not a nav section): launcher, console and gateway logs in `<pre>` with a 2px border, Copy and
  Refresh.

`confirm()` and `prompt()` are removed everywhere in favour of `ls-sheet` dialogs
(native `<dialog>` for focus containment). Keep is the solid primary; the risky
action is the dashed danger button.

## Accessibility

Inherits the brand rules: 3px ink focus ring, 44px targets, 200% text, `prefers-*`
media queries, forced-colors support, colour never alone. Additionally: the status
block is a polite live region announcing the word; section navigation uses real links
with `aria-current="page"`; sheets close on Escape; errors are associated with their
fields. Tested with the brand's `data-colour="reduced"` and `data-contrast="high"`.

## Security constraints (from AGENTS.md, unchanged)

The overhaul changes presentation only. Loopback bind, bearer token, Host and Origin
checks, launcher-only approval, restore, key export and destination enrolment, and the
explicit acceptance to activate an update are untouched. Serving assets from
`/assets/` lets the CSP drop `'unsafe-inline'`
(`default-src 'self'; frame-ancestors 'none'; base-uri 'none'`). The token stays in
the URL fragment and `sessionStorage`; `/assets/` is served without the token (static
files only, no state). No endpoints are added unless a screen cannot work without one.

## Tests

- Go: assets embedded; brand files match recorded hashes; CSP has no `unsafe-inline`;
  existing desktop tests pass unchanged.
- Contrast and font budget checks as in chaikadi's `design.test.js`, using the brand tokens.
- Browser smoke using the existing `CONTAINERS_NO_BROWSER` flow. Default CI e2e
  coverage is not changed.

## Out of scope for the first pass

A native (Raven) client; a product logo or favicon (the brand has none yet; use the
brand wordmark text and a plain `favicon` placeholder until it does); translations
(strings are kept in one `strings.js` so Malayalam and others can follow).

## Decisions recorded

1. Brand CSS licence: AGPL-3.0, matching this repo. Vendoring is allowed.
2. Five nav sections; logs live behind a Diagnostics link in the header (it is a support action, not a place users work in, and it keeps the always-visible nav short).
3. Footer carries "a letsimpl.fyi project" linking to letsimpl.fyi.
4. Text-only wordmark and no favicon artwork until the brand ships assets.
5. The redesign ships as 0.6.0, the first public beta.
