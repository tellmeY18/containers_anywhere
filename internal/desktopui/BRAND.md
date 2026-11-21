# Vendored brand files

`assets/tokens/`, `assets/css/{base,components}.css` and `assets/fonts/*.woff2` are
copied unmodified from https://brand.letsimpl.fyi (letsimpl.fyi brand tokens
**0.3.0**, fetched 2026-11-21). The brand CSS is AGPL-3.0, like this repository.
The fonts are SIL OFL 1.1; their notices sit beside them in `assets/fonts/`.

Do not edit them here. To update, re-download the files, review the diff, and change
the hashes in `ui_test.go` deliberately. Product layout lives in `assets/css/panel.css`
and uses brand tokens only: it adds no colours.

Not vendored: Atkinson Hyperlegible Next and Noto Serif Malayalam, which `base.css`
references. They load only for long-form prose and Malayalam text, which the panel
does not use yet.
