# 0004. Shared contracts live in global values

Date: 2026-10-17

## Context

Subcharts cannot read each other's values. A few facts must be identical across them: where the build nodes are, which issuer signs certificates, where CI jobs run.

## Decision

- `global.buildNodes` (selector, tolerations), `global.tls.issuerRef`, `global.jobs.namespace` and `global.jobs.architecture` are the shared contract. Everything else stays in the component's own block.
- Names that depend on the release (Service addresses, Secret names) are not repeated in values: the buildkit chart renders selected fields through `tpl`, and the runner chart renders its configuration through `tpl`, which sees the umbrella's helpers.
- Labels: `app.kubernetes.io/*` plus `ci-accelerator.io/component`, so policies across charts can select each other.

## Consequences

- Name overrides on subcharts whose names are derived elsewhere are refused at render time.
- Helm merges maps: a user's build-node selector extends the default (a switch, `enabled: false`, removes it).
