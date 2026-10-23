# 0021. Builds can reach the observability data (accepted for v0.1)

Date: 2026-10-18

Status: ended by [ADR-0039](0039-no-bundled-observability.md): the box keeps no observability data.

## Context

The daemons must reach the storage, and RUN steps share the daemons' network, so untrusted build code can use the storage's whole HTTP API: read every project's traces (which include the buildctl command line), add data, and delete data. NetworkPolicy cannot separate them (same pod IP). buildkit also mounts a trace socket into every RUN step, so builds can always add spans.

## Decision

Accepted for v0.1 and documented (README, Limitations; NOTES). A proxy that admits only the two OTLP ingestion paths without credentials, with NetworkPolicy keeping everything else away from the storage, is planned together with the build console.

## Consequences

- Never pass secrets as build arguments.
- Do not expose the storage outside the cluster.
