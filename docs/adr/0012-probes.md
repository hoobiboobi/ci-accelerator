# 0012. Probes with an empty environment and tolerant timings

Date: 2026-10-17

## Context

In production the Kubernetes default probe timeout (1 s) killed busy daemons. With tracing configured, `buildctl` waits for its trace exporter to flush on exit: with a slow or dead trace backend every probe took 10-30 s and liveness restarted every daemon at once. buildkit does not read `OTEL_SDK_DISABLED`.

## Decision

- Probes run `env -i OTEL_TRACES_EXPORTER=none /usr/bin/buildctl ... debug workers` on the local socket.
- A frequent startup probe hands over to readiness (period 10 s) and liveness (timeout 15 s, period 30 s, probe-level grace 30 s); readiness initial delay is 0.
- The end-to-end run keeps a dead trace backend configured the whole time and checks that probes stay fast (with a control that plain buildctl in the same environment is slow).

## Consequences

- Probes cannot detect every hang (a stuck registry push can block builds while the daemon answers).
