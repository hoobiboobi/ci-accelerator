# 0049. CI runs what a change touches

Date: 2026-10-28

## Context

Every pull request push ran everything: the chart checks on two Helm
versions, the Go tests, the image build and scan, the browser checks,
four end-to-end groups on kind and the one-node quickstart, about 80
runner-minutes and 18 minutes of waiting, whatever the change was. A
merge ran the cluster-less checks once more on main, on the tree the
pull request had just checked (main requires a pull request to be up to
date before it merges). The release day had five pushes to one pull
request, two of them cancelled by the next push. The repository is
public, so the minutes are free; the cost is the waiting, and a fix
after a red run waits again.

## Decision

- A reusable workflow (`changes.yml`) lists the paths a change touched
  (a pull request's merge commit against its base, a push against the
  commit before) and `scripts/ci-changes.sh` turns them into what runs:
  documentation alone runs nothing; Go code runs the Go tests, the image,
  the browser checks and the two e2e groups that run the control plane;
  the charts, the integrations, the e2e, the quickstart and the CI
  component run the chart checks, the component's tests, the quickstart
  and every e2e group; anything else (the Makefile, the workflows, the
  image's recipe, an unknown path) or no base to compare with runs
  everything. `test/ci-changes.sh` pins the rules.
- A job that is not needed is skipped, which branch protection counts as
  passed. Each workflow ends in one aggregating job (`checks passed`,
  `e2e on kind`) that is red when a job failed or when the change could
  not be classified: the jobs branch protection should require.
- The cluster-less checks no longer run on pushes to main.
- Pushes to `wip/**` and runs on demand keep running everything the
  change needs; on demand there is no base, so everything.

## Consequences

- A documentation change costs nothing; a Go change about half of
  before (no chart checks, two e2e groups instead of four); a chart
  change skips the Go jobs. A merge costs nothing beyond the pull
  request.
- The rules can be wrong in one direction only when a path is unknown
  (everything runs); a path wrongly listed as documentation would skip
  checks, so the lists name directories, not patterns of what to skip.
- The e2e matrix lives in `scripts/ci-changes.sh`; `test/e2e-matrix.sh`
  reads it there.
