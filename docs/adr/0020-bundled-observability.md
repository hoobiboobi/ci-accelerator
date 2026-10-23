# 0020. Bundled metrics and traces storage

Date: 2026-10-18

Status: superseded by [ADR-0039](0039-no-bundled-observability.md): the box no longer installs the storage.

## Context

The product promises build metrics and traces out of the box, without making Grafana or a monitoring stack part of it.

## Decision

- The umbrella installs VictoriaMetrics single and VictoriaTraces by default, each switchable off in favour of the user's own OTLP backend.
- Endpoints are derived from the release; a switched-off storage that the daemons still point at is a render error.
- Grafana is not bundled; the built-in UIs are reachable through port-forward.

## Consequences

- Two more stateful components with volumes that outlive the release.
- See ADR-0021 for who can reach them.
