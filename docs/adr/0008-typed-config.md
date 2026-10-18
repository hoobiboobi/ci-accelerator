# 0008. A typed buildkitd.toml core with an escape hatch

Date: 2026-10-17

## Context

buildkitd's configuration is large; a few fields decide whether production works (parallelism, GC, history, tracing). Mirroring the whole upstream schema would drift.

## Decision

- Key fields are typed values, validated by the schema and covered by unit tests.
- Everything else goes into `extraConfig`, raw TOML inserted before the chart's tables; `[worker.oci]` keys go through `workerOCI`.
- The rendered TOML is parsed at render time; an invalid result fails the install.

## Consequences

- Users can reach any upstream option without waiting for the chart.
- A broken `extraConfig` is caught before a daemon crashes (a config that parses but is wrong for buildkitd still stops the rollout on one node).
