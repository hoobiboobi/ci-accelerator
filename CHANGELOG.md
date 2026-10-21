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
- **Job pod annotations.** The runner configuration gains annotations, so
  the runner pod restarts once on upgrade; the build daemons do not. If you
  use `integrations/gitlab-runner/values.yaml` with your own runner, copy
  its new `pod_annotations` block. Job pods now carry the commit author's
  name and e-mail address, readable by whoever can read pods in the jobs
  namespace.
- **Console (preview, off).** The box gains a `console` subchart, off by
  default (`console.enabled`). Its daemons' NetworkPolicy gains one ingress
  rule for console pods of the release namespace.
- **The daemons restart once.** Every daemon now carries its node as a
  worker label (`ci-accelerator.io/node`), and keeps 1000 build records
  (48 h) for the console instead of buildkit's 50 (`config.history`, in
  both charts). Both change the daemons' pod template: the upgrade rolls
  the DaemonSet. With an `emptyDir` cache the
  daemons start with an empty cache; plan the upgrade between busy hours.
- **The runner hook** gains a line that prints the console's link when
  `global.console.url` is set (nothing otherwise). With your own runner,
  copy the new `pre_build_script` lines from
  `integrations/gitlab-runner/values.yaml`.

### Changed

- One trace per CI job (ADR-0028). With one trace per pipeline, BuildKit
  attached the spans of every concurrent job of a pipeline to each of its
  build records. Job ids beyond 2^32 (gitlab.com) now fit, and ids that
  are not numbers no longer produce a broken `TRACEPARENT`.

### Added

- `charts/console` and the box's `console.enabled` (preview, off by
  default): one pod that reads every daemon's build history and the CI job
  pods into SQLite and serves read-only pages and a JSON API on a
  ClusterIP Service (ADR-0032):
  - the pipeline list beside every page, with each job's verdict, jobs in
    progress, and filters for rebuilt and node-related jobs;
  - a pipeline: its time and the job that set it, the jobs' timeline, why
    each took its time, a waterfall from pipeline to step, the Dockerfile
    with one chip per job;
  - a build: the verdict, where the job's time went, BuildKit's time
    against the target's usual, its steps, Dockerfile and timeline, the
    previous build of the target step by step;
  - a step: source lines, output and history;
  - project insights and build nodes (each daemon's builds, running
    builds, cache by kind, cache policies);
  - search (Ctrl+K), keyboard shortcuts, light and dark themes, live
    updates.

  The pages work without JavaScript. With a GitLab token
  (`console.gitlab.token.existingSecret`), they also show the time before
  and after each build and jobs that ran elsewhere; GitLab is asked only
  while a page shows a pipeline. With `global.console.url`, every job
  prints the link to its page. No sign-in yet: keep it inside the cluster.
  Its image is not published yet; `make console-image` builds it for kind.
- Job pods carry the job's data from GitLab's CI variables as annotations
  under `ci-accelerator.io/`: pipeline source and creation time, project
  path, job stage and start time, commit title and author, merge request.
  A commit title reaches the pod as text, never expanded.
- How the console explains a build (`internal/history`): each step is
  cached, built or received from another job running it at the same time,
  and the first rebuilt step gets a reason. A rebuild is blamed on the
  build node when the step reads no files, or when another job of the same
  pipeline proves it. Each executed step's output is kept masked (secret
  build arguments and known token formats, also in step names) and clipped
  to 64 KiB. Tested on recorded builds (`test/history/generate.sh`) by
  `make go-test`.
- Checks for contributors: `make go-test` (Go) and `make ui-test`, the
  console's pages in headless Chrome (`test/ui`), both in the checks
  workflow; the e2e box scenario runs the browser checks against the box's
  console.

- The e2e scenario `upgrade`: the last published release, upgraded in place
  to the tree under test and rolled back.
- `CONTRIBUTING.md`, `docs/releasing.md` and ADR-0027: how changes reach
  `main`, when and how releases are cut, what an upgrade must keep.
- `docs/design/console.md`: design of the v0.2 console, and ADR-0029:
  sign-in and permissions through GitLab, job data from pod annotations,
  the Dockerfile and step output kept per build.

## 0.1.0 - 2026-10-19

First release: the `ci-accelerator` box and the standalone `buildkit` chart.

### Upgrade notes

First release. Pre-releases 0.1.0-rc.1 and 0.1.0-rc.2 upgrade to it in place
with `--reuse-values`.
