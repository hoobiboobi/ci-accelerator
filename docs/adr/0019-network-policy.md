# 0019. A NetworkPolicy for daemons and builds; no per-step network

Date: 2026-10-18

## Context

RUN steps share the daemon's network namespace, so the daemon's egress is also every build's egress: cloud metadata (node credentials), the API server, every in-cluster service. buildkit can give each RUN step its own bridge network, at a cost (namespace setup per step, MTU and iptables-backend risks) that production has not measured.

## Decision

- The chart renders a NetworkPolicy (on in the umbrella): clients only from the jobs namespace; egress to DNS and the internet, never to cloud metadata, and not to cluster CIDRs once the user lists them.
- RUN steps stay in host network mode. NetworkPolicy applies to them because their traffic leaves with the pod's IP. Bridge mode remains available through `workerOCI`.

## Consequences

- Builds still see the daemon's localhost and each other's connections (documented).
- Blocking the API server reliably depends on the CNI.
