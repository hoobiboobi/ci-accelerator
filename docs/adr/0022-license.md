# 0022. Apache License 2.0

Date: 2026-10-18

## Context

The project builds on BuildKit, Kubernetes, Helm and VictoriaMetrics, all Apache-2.0. Companies should be able to self-host it without legal review.

## Decision

Apache-2.0. Each chart package carries a copy of the license text (the packaged chart is a redistribution); a check keeps the copies identical.

## Consequences

A hosted clone needs no permission; accepted.
