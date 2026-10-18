# 0011. Same-node routing with fallback

Date: 2026-10-17

## Context

The cache is per daemon, so a job should talk to the daemon on its own node. The production setup used `internalTrafficPolicy: Local`: a node without a ready daemon made its jobs fail.

## Decision

The Service uses `trafficDistribution: PreferSameNode` by default: the node's own daemon, or another node's daemon when it is not ready. `Local` (strict) and `Cluster` are available. Verified on kind 1.37.

## Consequences

- A fallback build succeeds with a cold cache.
- Requires Kubernetes 1.34+ and a kube-proxy (or replacement) that implements node hints; older proxies silently balance cluster-wide.
- For a second or two after endpoints change, requests may go elsewhere.
