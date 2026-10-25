# GitLab CI component: build an image on the build farm

`ci/gitlab/build.yml` is a GitLab CI/CD component: one job that sends the
build context to the BuildKit daemon on its build node, which builds with
the shared cache and pushes the image. The job itself only waits; it needs
a runner set up by the box (or by `integrations/gitlab-runner`), which
gives job pods `BUILDKIT_REMOTE_ADDR` and a client certificate.

What the job does for you:

- logs in to the project's registry with the job token, and ignores
  `DOCKER_AUTH_CONFIG`, which would otherwise override that token for the
  same registry and break the push;
- passes secrets by the name of a CI/CD variable, never as build arguments;
- stops the build after `timeout-minutes` (buildctl has no keepalive: a build
  node that disappears silently would hold the job until its own timeout);
- prints how much context it sent and how many Dockerfile steps came from
  the cache;
- hands the image digest to later jobs (`IMAGE`, `IMAGE_DIGEST`,
  `IMAGE_REF` as a dotenv report).

## Use it

From GitHub, pinned to a release and checked by its hash (GitLab 17.9 or
newer; without `integrity`, 17.0). Every release's notes print this block
with the hash filled in:

```yaml
include:
  - remote: https://raw.githubusercontent.com/hoobiboobi/ci-accelerator/v0.2.4/ci/gitlab/build.yml
    integrity: sha256-<from the release notes>
    inputs:
      runner-tags: [ci-accelerator]
```

That is the whole job for a `Dockerfile` at the repository root: it builds
and pushes `$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA`. The GitLab server
fetches the file when the pipeline starts, so it needs to reach GitHub.
Without that, copy `build.yml` into a project of your GitLab and use
`include: project` (or `include: local`); the file includes nothing else.

Add `.git` to `.dockerignore`: every job has a fresh clone, and with `.git`
in the context every step after `COPY . .` runs again.

## Examples

Several images of a monorepo, each its own job:

```yaml
include:
  - remote: https://raw.githubusercontent.com/hoobiboobi/ci-accelerator/v0.2.4/ci/gitlab/build.yml
    integrity: sha256-...
    inputs:
      job-name: build-api
      runner-tags: [ci-accelerator]
      context: services/api
      images: |
        $CI_REGISTRY_IMAGE/api:$CI_COMMIT_SHORT_SHA
        $CI_REGISTRY_IMAGE/api:latest
  - remote: https://raw.githubusercontent.com/hoobiboobi/ci-accelerator/v0.2.4/ci/gitlab/build.yml
    integrity: sha256-...
    inputs:
      job-name: build-web
      runner-tags: [ci-accelerator]
      context: services/web
      images: $CI_REGISTRY_IMAGE/web:$CI_COMMIT_SHORT_SHA
```

Build arguments and a secret (the value of the masked CI/CD variable
`NPM_TOKEN`, for `RUN --mount=type=secret,id=npmrc`):

```yaml
    inputs:
      build-args: |
        NODE_VERSION=22
      secrets: |
        npmrc=NPM_TOKEN
```

Check that merge requests build, push only on the default branch: change
the job by defining the same job name in your `.gitlab-ci.yml` (GitLab
merges the keys):

```yaml
build-image:
  rules:
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH
build-image-check:   # a second include with job-name: build-image-check and push: false
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
```

Use the digest in a later job:

```yaml
deploy:
  needs: [build-image]
  script:
    - echo "deploying $IMAGE_REF"
```

## Inputs

The inputs are declared and described in the `spec` of
[build.yml](build.yml), which GitLab validates: an unknown input or a
value outside `options` stops the pipeline before it starts.

| Input | Default | What |
|---|---|---|
| `job-name` | `build-image` | Name of the job; one per image |
| `stage` | `build` | Stage of the job |
| `runner-tags` | `[]` | Tags that send the job to a ci-accelerator runner |
| `context` | `.` | Build context directory |
| `dockerfile` | `<context>/Dockerfile` | Path of the Dockerfile |
| `target` | (last stage) | Dockerfile stage to build |
| `platform` | (the node's) | Platforms, comma-separated |
| `build-args` | | `KEY=VALUE` per line; never secrets |
| `secrets` | | `id=VARIABLE` per line, from CI/CD variables |
| `images` | `$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA` | References to push, one per line |
| `push` | `true` | `false` only builds |
| `registry-auth` | `job-token` | `job-token`, `docker-auth-config` or `none` |
| `labels` | `true` | OCI labels `source` and `revision` |
| `provenance` | `false` | Attach a SLSA provenance attestation |
| `timeout-minutes` | `60` | Stop the build after this |
| `buildctl-args` | | More `buildctl build` arguments, one per line |
| `buildkit-image` | the daemons' release | Image with buildctl for the job |

Values may use CI/CD variables (`$CI_COMMIT_REF_SLUG`); write `$$` for a
literal `$`.

## Limits

- The cache is per daemon: a project's first build on each build node is
  cold.
- Secrets reach RUN steps through BuildKit's secret mounts; build
  arguments do not, and they end up in the image history and in traces.
- Only Dockerfile builds (`dockerfile.v0`); other frontends through
  `buildctl-args`.

## Tests

`ci/gitlab/test/run.sh` (in `make component-test`) runs the job's script in
its own image against a fake buildctl, one case per input and failure.
