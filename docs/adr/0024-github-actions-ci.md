# 0024. CI on GitHub Actions with the same make targets

Date: 2026-10-18

## Context

Every check already is a `make` target. GitHub-hosted runners for a private repository on the Free plan have 2 vCPU, 8 GB of RAM and 14 GB of disk, and 2000 minutes a month; one end-to-end run takes about 25 minutes. Anonymous Docker Hub pulls are limited per IP address, and runners share addresses.

## Decision

- `checks.yml` runs `make lint unittest validate` on Helm 3 and Helm 4 for every push to main and every pull request.
- `e2e.yml` runs `make kind-up` and `make e2e` on pull requests to main and on demand, not on every push. It frees disk, raises the inotify limits kind needs for several nodes, logs in to Docker Hub when credentials are configured, and uploads the logs when it fails.
- Helm, kind and kubectl come from `make ci-tools`: pinned versions, checked against pinned sha256 sums, like every other tool. Actions are pinned by full commit SHA; workflows get read-only permissions and never run on `pull_request_target`.

## Consequences

- CI and local runs use the same commands; a failure in CI can be reproduced with `make`.
- The end-to-end run does not gate every push; a nightly run can be added when the repository is public and minutes are free.
- If anonymous pulls hit the rate limit, the images have to be pulled on the host with credentials and loaded into the kind nodes.
