# 0003. The runner integration is a contract

Date: 2026-10-17

## Context

CI jobs must reach the daemons, authenticate, and run on the node whose daemon has the warm cache. We do not want to own or fork the gitlab-runner chart.

## Decision

The contract between a CI job and the daemons:
1. environment `BUILDKIT_REMOTE_ADDR=tcp://<service>:1234` and `BUILDKIT_TLS_DIR=/certs`;
2. a client certificate Secret mounted read-only at that directory;
3. job pods placed on the build nodes (node selector, toleration, and the node architecture, from which the runner picks its helper image);
4. a small resource tier, since the build runs in the daemon;
5. job pods without a Kubernetes API token, in their own namespace.

`integrations/gitlab-runner/values.yaml` implements it for an existing runner; the umbrella chart implements it with names derived from its release. A check keeps both configurations identical.

## Consequences

- The same contract can be implemented for GitHub Actions Runner Controller later.
- A job's own `variables:` override the injected environment (documented).
- `runners.config` is a single TOML string in the runner chart and is not merged with a user's own.
