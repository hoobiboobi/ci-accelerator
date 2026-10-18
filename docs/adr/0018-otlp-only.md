# 0018. Metrics and traces only via OTLP push

Date: 2026-10-18

## Context

buildkitd serves `/metrics` only on its debug server, next to pprof and cache endpoints. RUN steps share the daemon's network and could reach that server. buildkitd can push metrics and traces over OTLP instead.

## Decision

- The debug server is never enabled; there is nothing to scrape (no ServiceMonitor).
- The chart sets per-signal OTLP endpoints (http/protobuf by default) and always tags metrics and spans with the daemon's pod, node and namespace.
- CI jobs join a build to their pipeline's trace by setting `TRACEPARENT`.

## Consequences

- Go runtime and process metrics of the daemon are not exported (kubelet/cAdvisor cover the pod).
- Counters arrive as deltas; queries use `sum_over_time`.
