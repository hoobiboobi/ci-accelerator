# 0014. Draining builds on shutdown

Date: 2026-10-18

## Context

It was unclear whether buildkitd aborts running builds on SIGTERM. Measured on kind: it stops accepting new connections and lets running builds finish.

## Decision

- `terminationGracePeriodSeconds` (300 by default) is documented as "the longest build that survives a rollout"; production should use 1200-1800.
- A 5 s `preStop` sleep keeps serving while the endpoint is withdrawn (cheap insurance; the race could not be reproduced on a small cluster).
- Liveness and startup probes have their own 30 s grace, so a wedged daemon is not waited on for the full grace period.

## Consequences

A rollout takes up to nodes x (longest build + startup); pass `--timeout` to Helm accordingly.
