# 0015. Everything pinned and verified

Date: 2026-10-18

## Context

Supply-chain hygiene and reproducible checks.

## Decision

- Images are pinned by tag and index digest (buildkit, VictoriaMetrics, VictoriaTraces, and in tests cert-manager and helpers).
- Third-party charts and test tools are downloaded by `make` and checked against pinned sha256 sums. Helm's `Chart.lock` digest does not cover archive content, so the umbrella vendors its dependencies with its own checks.
- Versions are chosen at least two weeks after release, after reading release notes and open issues.

## Consequences

Bumps are deliberate changes with their own checks.
