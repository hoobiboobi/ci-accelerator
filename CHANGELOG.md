# Changelog

Each release upgrades in place from the previous one; see
[docs/adr/0027-upgrades-and-branches.md](docs/adr/0027-upgrades-and-branches.md).
Every version has **Upgrade notes**, even if they say "None". The release
workflow copies them into the GitHub release.

## Unreleased

### Upgrade notes

- **VictoriaMetrics and VictoriaTraces are no longer part of the box**
  (ADR-0039):
  - The upgrade removes their StatefulSets. Their volumes stay; delete
    them by hand once you no longer need the data
    (`kubectl -n <namespace> get pvc | grep -e victoria-metrics-single -e vt-single`).
  - To keep metrics and traces, run your own OTLP backend and set
    `buildkit.otel.metricsEndpoint` and `tracesEndpoint`. With
    `--reuse-values` the old endpoints point at the removed Services:
    upgrade with `--reset-then-reuse-values`, or set them to your backend
    or `""`. The install notes warn while old values remain.
  - The daemons restart when their endpoints change.
- **Runner configuration.** The box's runner TOML is now one template for
  every runner. Without a cache it renders exactly as before (the runner
  does not restart). With `--reuse-values` the main runner keeps the TOML
  of the release it was installed with, so `gitlab-runner.runners.cache`
  takes effect only with `--reset-then-reuse-values`.

- **Trace ids.** Every CI job now gets its own trace instead of sharing one
  per pipeline. Trace ids have a new layout:
  - `01` + 3 bytes from the GitLab URL;
  - the pipeline id and the job id, 6 bytes each.

  Traces stored by 0.1.0 keep the old layout. To find a pipeline's jobs
  in your trace backend, query the `ci.pipeline.id` resource attribute.
  Nothing to change in values.
- **Job pod annotations.** The runner configuration gains annotations, so
  the runner pod restarts once on upgrade; the build daemons do not. If you
  use `integrations/gitlab-runner/values.yaml` with your own runner, copy
  its new `pod_annotations` block. Job pods now carry the commit author's
  name and e-mail address, readable by whoever can read pods in the jobs
  namespace.
- **The console is on by default.** The box gains a `console` subchart
  (`console.enabled`, true): one Deployment and a 10 GiB volume from the
  default StorageClass (`console.persistence.*`), a client certificate from
  the daemons' CA, a Role that reads the pods of the jobs namespace, and
  one ingress rule on the daemons' NetworkPolicy. A cluster without a
  default StorageClass needs `console.persistence.storageClass` or
  `console.enabled=false`, or the install waits for the volume. With
  `--reuse-values` an upgrade keeps it off (the old values have no such
  key); `--reset-then-reuse-values` turns it on.
- **The daemons restart once.** Every daemon now carries its node as a
  worker label (`ci-accelerator.io/node`), and keeps 1000 build records
  (48 h) for the console instead of buildkit's 50 (`config.history`, in
  both charts). Both change the daemons' pod template: the upgrade rolls
  the DaemonSet; plan it between busy hours. With an `emptyDir` cache the
  daemons also start with an empty cache, so the next build of every image
  rebuilds every step. With either cache the console has no earlier build
  to compare with (0.1.0's records carry the old trace ids and their job
  pods are gone), so each image's next build that rebuilds a step is
  reported as its first.
- **The runner hook** gains a line that prints the console's link when
  `global.console.url` is set (nothing otherwise). With your own runner,
  copy the new `pre_build_script` lines from
  `integrations/gitlab-runner/values.yaml`.

### Changed

- Pinned components: the console image is built with Go 1.27.3 on the
  current distroless base, the browser checks run on Node 24.22.1 and the
  one-machine install (and the e2e) pin cert-manager v1.21.3 - security
  patches adopted under ADR-0041 before they were two weeks old.
- The box installs no metrics and traces storage (ADR-0039). The daemons
  push over OTLP only to an endpoint you set (`buildkit.otel.*`), none by
  default. Builds can no longer reach the box's observability data,
  because there is none.
- One trace per CI job (ADR-0028). With one trace per pipeline, BuildKit
  attached the spans of every concurrent job of a pipeline to each of its
  build records. Job ids beyond 2^32 (gitlab.com) now fit, and ids that
  are not numbers no longer produce a broken `TRACEPARENT`.

### Added

- The console shows its version at the bottom of the sidebar and with
  `console -version`: the image tag and the commit it was built from.
- Vulnerability checks in CI: `govulncheck` over the console's code for both
  release targets and `trivy` over the built image on every pull request
  (`make vulncheck`, `make image-scan`), plus a weekly run against `main`
  and the latest published image. Grace periods live in
  `.govulncheck-allow` and `.trivyignore.yaml` with an expiry and a reason.
  `SECURITY.md` says how to report a vulnerability.
- `quickstart.sh` (attached to every release): try the box on one clean
  Linux machine. It installs k3s, Helm, cert-manager and the box with its
  console; k3s, Helm, cert-manager and the chart are pinned and checked
  against their sha256 (the box's images come by tag, the daemons' by
  digest). `--uninstall` removes it all, the build cache included. Checked
  on every pull request on a clean Ubuntu runner: it installs,
  builds twice through the real runner (the second build cached), shows
  the build in the console, and uninstalls.
- `charts/console` and the box's `console.enabled` (on by default; false
  leaves it out): one pod that reads every daemon's build history and the CI job
  pods into SQLite and serves read-only pages and a JSON API on a
  ClusterIP Service. It answers one question: why an image build was slow
  or not cached, and whether the change or the build farm caused it
  (ADR-0037):
  - Overview: rebuild reasons grouped and ranked by the build time they
    cost;
  - Pipelines: each pipeline with its jobs by stage on a time axis, how
    long each waited for a runner and ran (from GitLab: read for the
    pipelines on the page, once when finished), the usual and longest
    wait; a pipeline opens into every job, each image build into its
    stages and steps;
  - Builds: every image build, filtered by branch, outcome, image or your
    own, optionally grouped by pipeline;
  - Images: cache rate, usual and slowest build time, the steps that
    rebuild most and why, over 24 hours, 7 or 30 days, and builds per
    week by outcome over the last year;
  - Nodes: each daemon's builds, running builds, cache by kind, cache
    policies, the runner cache bucket per project;
  - a build: one sentence, what changed since the previous build of the
    image, its steps on a list, a timeline and its Dockerfile; a step:
    source lines, output and history; a pipeline: why its images were
    rebuilt and a timeline of every job (from GitLab, or from the job pods
    without a token), each image build opening into its stages and
    steps.

  Five outcomes everywhere: from cache, your change, build farm, unclear,
  failed. A rebuild whose cache was on another node is settled with
  GitLab's comparison of the two builds' commits (ADR-0038): the files
  its step copies changed (your change) or did not (build farm); it stays
  unclear only without a GitLab token or sign-in. Search (Ctrl+K), keyboard shortcuts, light and dark themes, live
  updates; the pages work without JavaScript. Builds are kept 30 days
  (`console.retention.days`); each image's daily figures (builds by
  outcome, build times) are kept a year. With `global.console.url`, every job prints
  the link to its build. Without sign-in, keep it inside the cluster. Its
  image is published with every release
  (`ghcr.io/hoobiboobi/ci-accelerator-console:v<version>`, amd64 and
  arm64, signed); `make console-image` builds it for kind.
- S3 cache for GitLab's `cache:` keyword (`gitlab-runner.runners.cache`;
  ADR-0035, docs/runner-cache.md): one folder per release and GitLab in
  any S3-compatible bucket, shared by every runner of the box; job logs
  show each cache's size.
- Runner tiers (`runnerTiers`): more runners next to the main one, any
  number, each its own pod with its own GitLab runner (tags), job size,
  placement (build nodes or any node) and S3 cache. The install notes say
  what to create in GitLab.
- Sign-in through GitLab for the console (`console.signIn`, off by
  default; ADR-0034): each person signs in with a GitLab the runners serve
  and sees the projects whose jobs GitLab lets them read; GitLab is read
  with their own token. It needs the console's address
  (`global.console.url`), a Secret with a session key and, per GitLab, an
  OAuth application (confidential, scopes `openid read_api`; the install
  notes print its redirect URI). Admins (GitLab usernames) also see builds
  of no known project and the JSON API. Sessions end after 7 days unused
  and 30 days after sign-in at the latest; sign-out revokes the session's
  token at GitLab.
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
  console's pages in headless Chrome (`test/ui`), without and with
  sign-in (then also the sign-in page and signing out), both in the checks
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
