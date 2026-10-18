# 0009. The chart always renders the full GC policy set

Date: 2026-10-17

## Context

Reading buildkit v0.33.1's source showed that worker-level `reservedSpace`/`maxUsedSpace`/`minFreeSpace` are used only to generate default policies when no explicit policy exists. One explicit policy (for example, a longer keep time for cache mounts) silently drops the caps. buildkit's default 48 h keep time empties package caches over a weekend.

## Decision

- The chart renders upstream's policy shape itself, with cache mounts in their own policy (336 h by default) and the shared caps injected into every capped policy.
- Render-time guards refuse policies that would delete the whole cache after every build, and Kubernetes size suffixes buildkit cannot parse.
- `minFreeSpace` defaults to 30%: the old 15% sat inside kubelet's eviction zone when the cache shares the node's disk. Dedicated disks use absolute sizes.

## Consequences

- GC behaviour is predictable and tested; an end-to-end check proves a 1 GB cap is enforced.
- Sizes use buildkit's binary units (`10GB` = 10 GiB); documented.
