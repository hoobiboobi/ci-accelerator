# Changelog

Each release upgrades in place from the previous one; see
[docs/adr/0027-upgrades-and-branches.md](docs/adr/0027-upgrades-and-branches.md).
Every version has **Upgrade notes**, even if they say "None". The release
workflow copies them into the GitHub release.

## Unreleased

### Upgrade notes

None.

### Added

- The e2e scenario `upgrade`: the last published release, upgraded in place
  to the tree under test and rolled back.

## 0.1.0 - 2026-10-19

First release: the `ci-accelerator` box and the standalone `buildkit` chart.

### Upgrade notes

First release. Pre-releases 0.1.0-rc.1 and 0.1.0-rc.2 upgrade to it in place
with `--reuse-values`.
