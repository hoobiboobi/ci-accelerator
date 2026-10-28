# Architecture decision records

Short records of the decisions that shape the charts: context, decision, consequences. Dates are when the decision was taken; several come from measurements on a local cluster or from the production setup this project is derived from.

| ADR | Decision |
|---|---|
| [0001](0001-umbrella-and-standalone-charts.md) | An umbrella chart over standalone subcharts |
| [0002](0002-helm-and-gitops.md) | Helm 3 and 4, schemas, GitOps-friendly |
| [0003](0003-runner-integration-contract.md) | The runner integration is a contract |
| [0004](0004-shared-values.md) | Shared contracts live in global values |
| [0005](0005-build-node-label.md) | Build nodes are marked with our own label and taint |
| [0006](0006-privileged-rootful-daemon.md) | Rootful, privileged daemons by default |
| [0007](0007-storage-modes.md) | Storage modes and their guard rails |
| [0008](0008-typed-config.md) | A typed buildkitd.toml core with an escape hatch |
| [0009](0009-gc-policies.md) | The chart always renders the full GC policy set |
| [0010](0010-namespaces.md) | One box namespace and a separate jobs namespace |
| [0011](0011-routing.md) | Same-node routing with fallback |
| [0012](0012-probes.md) | Probes with an empty environment and tolerant timings |
| [0013](0013-stable-pod-template.md) | A pod template that chart releases do not change |
| [0014](0014-termination.md) | Draining builds on shutdown |
| [0015](0015-pinning.md) | Everything pinned and verified |
| [0016](0016-mutual-tls.md) | Mutual TLS is mandatory |
| [0017](0017-cert-manager-prerequisite.md) | cert-manager is a prerequisite, never a subchart |
| [0018](0018-otlp-only.md) | Metrics and traces only via OTLP push |
| [0019](0019-network-policy.md) | A NetworkPolicy for daemons and builds; no per-step network |
| [0020](0020-bundled-observability.md) | Bundled metrics and traces storage (superseded by 0039) |
| [0021](0021-observability-access.md) | Builds can reach the observability data (accepted for v0.1; ended by 0039) |
| [0022](0022-license.md) | Apache License 2.0 |
| [0023](0023-values-compatibility.md) | Values compatibility from 0.1.0 on |
| [0024](0024-github-actions-ci.md) | CI on GitHub Actions with the same make targets |
| [0025](0025-releases.md) | Releases as OCI charts on ghcr.io |
| [0026](0026-job-trace-context.md) | The runner sets every job's trace context |
| [0027](0027-upgrades-and-branches.md) | In-place upgrades, short-lived branches, releases when they matter |
| [0028](0028-one-trace-per-job.md) | One trace per job (supersedes the trace-id layout of 0026) |
| [0029](0029-console-gitlab-and-build-data.md) | Console: GitLab sign-in, job data from pods, Dockerfiles and logs kept |
| [0030](0030-console-store.md) | Console store: SQLite, a content-addressed step graph, daily output files |
| [0031](0031-console-packaging.md) | Console packaging: one pod, its own certificate, the runner's URL once |
| [0032](0032-console-pages-and-live-data.md) | Console pages: the agreed design, live data, a GitLab token for now |
| [0033](0033-console-jobs-without-builds.md) | Console: GitLab says how jobs went; pipelines without builds (superseded by 0037) |
| [0034](0034-console-sign-in.md) | Console: sign-in through GitLab, inside the console |
| [0035](0035-runner-cache-and-tiers.md) | Runner cache in S3, and runner tiers rendered by the box |
| [0036](0036-infrastructure-runs-in-the-console.md) | Terraform, OpenTofu and Terragrunt runs in the console (superseded by 0037) |
| [0037](0037-console-builds-only.md) | Console v2: image builds only, five sections, five outcomes |
| [0038](0038-files-or-node-from-gitlab.md) | Files or node: settled with GitLab's comparison of two commits |
| [0039](0039-no-bundled-observability.md) | No bundled metrics and traces storage |
| [0040](0040-vulnerability-checks.md) | Vulnerability checks in CI |
| [0041](0041-security-patch-exception.md) | Security patches skip the two-weeks wait |
| [0042](0042-gitlab-ci-component.md) | A GitLab CI component, included by URL from the release tag |
| [0043](0043-daemon-priority-and-disk-probe.md) | The daemons' priority, default requests and disk probe |
| [0044](0044-control-plane.md) | The control plane: a controller and a web, new names, no in-place upgrade from 0.2.x |
| [0045](0045-build-router.md) | The build router: each job to the daemon where its project is warm |
| [0046](0046-cache-reuse.md) | Cache shared across projects; no per-project namespaces |
| [0047](0047-figures-the-console-shows.md) | Figures the console shows: measured, recountable, from enough builds |
| [0048](0048-logs.md) | Logs: one JSON line per event, info bounded by the work, no secrets |
