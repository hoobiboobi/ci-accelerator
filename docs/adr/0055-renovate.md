# 0055. Renovate, self-hosted, with the checksums recomputed from upstream

Date: 2026-10-31

## Context

Every version in the repository is pinned, by a rule: at least two weeks
old, a sha256 or a digest next to it, bumped by hand with a reason and a
date. Some forty pins across the Makefile, the e2e, the quickstart, the
charts, the CI component and the workflows, plus Go modules and GitHub
Actions. Bumping them by hand meant a pass over releases and checksum
files every few weeks, and a pin forgotten for months when nothing forced
it. Dependabot updates versions but knows nothing of a sum written next
to one; Renovate's regex managers find such pins, but no bot can
recompute a checksum.

## Decision

- **Renovate, self-hosted in GitHub Actions**, once a week, pinned by
  image digest and bumping itself. Not the Mend-hosted app: it forbids
  `postUpgradeTasks`, and the sums are the point. The run authenticates
  as a GitHub App of the repository (an installation token), so its pull
  requests get the checks; commits go through GitHub's API, signed as the
  App.
- **The sums follow the version through one script**,
  `scripts/update-pins.sh`, run by Renovate after a bump and by hand
  otherwise, which reads each sum from where its project publishes it
  (a CHECKSUMS file, SHASUMS256.txt, go.dev's listing, a `.sha256sum` next
  to the archive, a chart layer's digest in its OCI manifest) and rewrites
  only the hex in the tree. ShellCheck publishes none: computed from the
  download, and said so. The same script carries the two consequences a
  BuildKit bump has (the CI component's integrity, the chart's list of
  earlier default digests). Its `--check` mode compares the tree's sums
  with upstream's; the checks run it when Renovate's files change.
- **Annotations, not a parallel list**: `# renovate: datasource=...
  depName=...` above each pinned assignment, an image written after a bare
  digest. Four regex managers read those shapes. A check runs Renovate's
  own image over the tree (`--platform=local --dry-run=extract`) and
  compares what it finds with `test/renovate-deps.txt`, so a pin that
  loses its annotation, or a new pin without one, fails a pull request.
- **One PR per subject** (the Go toolchain in its three places, BuildKit
  in its six, a chart with the image it runs), `minimumReleaseAge: 14
  days`, at most three PRs open, no automerge, no security PRs from
  Renovate (GitHub's Dependabot security updates are on already).
- **By hand still**: kind's node images (kind's release notes list them
  with digests), `KUBE_VERSIONS`, Helm 3 until its end of life, the `go`
  directive, the project's own images.

## Consequences

- A pin older than two weeks shows up as a pull request on Monday with
  its sums done and a note on what to check; the reviewer reads the
  release notes and the changelog line, as before.
- Registries without release dates (ghcr, quay, gcr, registry.gitlab.com)
  cannot be held to the two weeks: those PRs are labelled
  `age-unverified` and the date is checked by hand.
- The repository gains a weekly Actions run of a few minutes and the
  checks of the PRs it opens; grouping and the limit of three keep that to
  a few PRs a week.
- The GitHub App is one more credential to keep: a private key in a
  secret, revocable from the App's page.
