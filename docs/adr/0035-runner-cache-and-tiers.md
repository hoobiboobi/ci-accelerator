# 0035. Runner cache in S3, and runner tiers rendered by the box

Date: 2026-10-22

## Context

GitLab's `cache:` keyword needs object storage with the Kubernetes executor: without it the cache is lost with each job pod. Users also want more than one runner from one installation (small pods for deploys, large ones for builds), each with its own tags, job size, placement and cache. The official runner chart registers one runner per release; its `configOverride` can hold several, but then one invalid token stops them all and the S3 keys cannot differ (the runner does not expand variables in them). Helm can include the runner chart only a fixed number of times.

## Decision

- **S3 cache** for the box's main runner: a `[runners.cache]` block in the box's runner TOML (Shared, one folder per release and GitLab, 4.5 GiB archive limit, sizes in job logs), keys from a Secret through the runner chart's own `runners.cache.secretName`. Checked live with Hetzner Object Storage and gitlab.com.
- **Runner tiers**, any number: the box renders one runner pod per tier itself (the official runner image and its `run` command, a ConfigMap with the TOML, a ServiceAccount and a Role in the jobs namespace). The token comes from `${RUNNER_TOKEN}` (a Secret), the S3 keys from `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` (a Secret): no secret in a ConfigMap. Tags are set in GitLab when the runner is created. The box's main runner stays the official chart.
- **One TOML text** for every runner (`templates/_runner.tpl`): same daemons, certificate, trace context, job annotations and namespace. The main runner's rendered configuration is byte for byte what it was, so an upgrade without a cache does not restart it.
- Jobs cannot raise their resources; a tier's size is explicit.
- Old caches are removed by the bucket's lifecycle rule (documented), not by the box.

## Consequences

- The box now owns a small runner deployment (for tiers only): runner releases need a look at the image's entrypoint and health endpoint.
- Tiers share a project's caches (same folder); a cache is no trust boundary within a project.
- Job pods must reach the store; with `--reuse-values` the main runner keeps its old TOML (use `--reset-then-reuse-values`).
