# 0042. A GitLab CI component, included by URL from the release tag

Date: 2026-10-25

## Context

Using the build farm from a job meant writing the buildctl call by hand,
and the two steps that break a push (registry credentials for the daemon,
and `DOCKER_AUTH_CONFIG` overriding them) were documented, not done for
the user. GitLab's CI/CD components (`include: component`) work only within
one GitLab instance: a component on gitlab.com cannot be included by a
self-managed GitLab without mirroring the project, and pull mirroring is a
paid feature. `include: remote` works on every GitLab that can reach the
URL, validates `inputs` like a component, and checks an `integrity` hash
(GitLab 17.9). Both were checked on gitlab.com through its CI Lint API
(substituted inputs, an unknown input and a wrong hash refused, a file from
raw.githubusercontent.com at a tag fetched).

## Decision

- One self-contained file, `ci/gitlab/build.yml`, versioned with the box
  (it depends on the runner contract: `BUILDKIT_REMOTE_ADDR`,
  `BUILDKIT_TLS_DIR`). Included from
  `raw.githubusercontent.com/<repo>/<tag>/ci/gitlab/build.yml` with the
  hash the release notes print; tags are immutable. Copying it into a
  project works for GitLab without access to GitHub.
- Inputs reach the script only as CI/CD variables, never as script text;
  list inputs are one item per line; the job name and stage are inputs
  (several images per pipeline); no global keywords. Users change the job
  by defining the same job name (GitLab merges the keys).
- The job logs in with the job token and unsets `DOCKER_AUTH_CONFIG` by
  default, passes secrets by variable name (checked as a name before use),
  bounds buildctl with a timeout, counts Dockerfile steps from the plain
  progress, and writes the digest to a dotenv report. Its image is the
  daemons' BuildKit release by digest (a test keeps them equal).
- The CI/CD Catalog on gitlab.com (group `ci-accelerator` reserved) is a
  later, additional channel.

## Consequences

- Any GitLab 17.9+ with access to GitHub uses it unchanged; older ones
  drop `integrity` (17.0+), offline ones copy the file.
- `make component-test` runs the script against a fake buildctl in its own
  image; real pipelines on gitlab.com with the box's runner covered push
  with the job token, a secret, two images, `push: false`, a failing build
  with `allow_failure` set by job name, and the digest in a later job.
