# 0039. No bundled metrics and traces storage

Date: 2026-10-23

Status: supersedes [ADR-0020](0020-bundled-observability.md); ends the limitation of [ADR-0021](0021-observability-access.md).

## Context

Since 0.1.0 the box installed VictoriaMetrics and VictoriaTraces for the daemons' OTLP metrics and traces. Three things changed the trade-off:
- The console now answers what people looked at them for: why a build was slow or missed the cache, how long jobs waited for a runner. It reads BuildKit's build records and GitLab, never the trace storage.
- The bundled storage was reachable by every build. RUN steps share the daemons' network, so untrusted code could read every project's traces (which include the buildctl command line) and add or delete data (ADR-0021). Closing that would need an authenticating proxy in front of both stores.
- It cost two stateful pods, 26 GiB of volumes and two more pinned components, one of them a 0.x release and the other with an open web UI issue.

## Decision

- The box no longer installs VictoriaMetrics or VictoriaTraces. The daemons still push metrics and traces over OTLP when `buildkit.otel.metricsEndpoint` / `tracesEndpoint` name the user's own backend. Both are empty by default, so nothing is sent.
- The runner still gives every job its own trace id. The console uses it to tie build records to pipelines and jobs, with no storage involved.
- Upgrades from 0.1.0:
  - Helm removes both StatefulSets; their volumes stay until deleted by hand.
  - The chart keeps the old Service-name helpers, so 0.1.0's endpoint values still render under `--reuse-values`.
  - The install notes warn while old storage values or endpoints remain.
- This departs from the compatibility policy (deprecate for one minor first, ADR-0027) on purpose: keeping the storage another minor would keep ADR-0021's exposure, and the release removing it came four days after the only one that had it.

## Consequences

- Without a backend of their own, users get no BuildKit metrics or trace views; the console covers build timing and cache answers.
- Trace data sent to the user's backend is still written through BuildKit by any build: it is untrusted input, and build arguments must not carry secrets.
- e2e: the upgrade scenario checks that the old storage goes and its volumes stay. The otel scenario still checks the OTLP push against a throwaway receiver.
