# 0001. An umbrella chart over standalone subcharts

Date: 2026-10-17

## Context

Users want either the whole thing (a fast GitLab runner) or one part of it (only the BuildKit daemons, or only a runner configuration for a runner they already operate). Harbor and GitLab ship the same way: one installable product made of charts that also stand alone.

## Decision

- `charts/ci-accelerator` is the product: one `helm install` with a few required values.
- `charts/buildkit` (and later components) are self-contained charts, installable and published on their own.
- Third-party charts (gitlab-runner, VictoriaMetrics, VictoriaTraces) are dependencies with a `condition`, never vendored copies or forks. Infrastructure we do not own (cert-manager, Postgres, oauth2-proxy) is referenced, not installed.
- Alternatives rejected: Timoni/CUE (no ecosystem; third-party charts would need rewriting), Kustomize (does not compose third-party charts), an operator (a later layer, not a replacement).

## Consequences

- Each subchart needs its own schema, tests and docs.
- Values shared between subcharts need an explicit contract (ADR-0004).
- Users with their own runner use `integrations/gitlab-runner` instead of the umbrella's dependency.
