# Changelog

Each release upgrades in place from the previous one; see
[docs/adr/0027-upgrades-and-branches.md](docs/adr/0027-upgrades-and-branches.md).
Every version has **Upgrade notes**, even if they say "None". The release
workflow copies them into the GitHub release.

## Unreleased

### Upgrade notes

- **Trace ids.** Every CI job now gets its own trace instead of sharing one
  per pipeline. Trace ids have a new layout:
  - `01` + 3 bytes from the GitLab URL;
  - the pipeline id and the job id, 6 bytes each.

  Traces stored by 0.1.0 keep the old layout. To find a pipeline's jobs,
  query the `ci.pipeline.id` attribute; the install notes show how.
  Nothing to change in values.

### Changed

- One trace per CI job (ADR-0028). With one trace per pipeline, BuildKit
  attached the spans of every concurrent job of a pipeline to each of its
  build records. Job ids beyond 2^32 (gitlab.com) now fit, and ids that
  are not numbers no longer produce a broken `TRACEPARENT`.

### Added

- The e2e scenario `upgrade`: the last published release, upgraded in place
  to the tree under test and rolled back.
- `CONTRIBUTING.md`, `docs/releasing.md` and ADR-0027: how changes reach
  `main`, when and how releases are cut, what an upgrade must keep.
- `docs/design/console.md`: design draft of the v0.2 console.

## 0.1.0 - 2026-10-19

First release: the `ci-accelerator` box and the standalone `buildkit` chart.

### Upgrade notes

First release. Pre-releases 0.1.0-rc.1 and 0.1.0-rc.2 upgrade to it in place
with `--reuse-values`.
