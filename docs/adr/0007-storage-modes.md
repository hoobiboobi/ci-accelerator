# 0007. Storage modes and their guard rails

Date: 2026-10-17

## Context

buildkitd takes an exclusive lock on its state directory, so replicas cannot share a volume. Production keeps the cache on a local NVMe disk; CI and kind need something disposable. A cache on the node's root disk competes with kubelet's eviction thresholds.

## Decision

- `DaemonSet` + `hostPath` for production (a pre-created directory on a dedicated disk, `type: Directory`, so a missing mount fails the pod instead of filling the OS disk).
- `DaemonSet` + `emptyDir` for kind and CI (the default).
- Not `Deployment` + one PVC.
- With hostPath, `maxSurge` must be 0 (a surge pod would never get the lock); with an emptyDir size limit, GC caps must be absolute and below 80% of it.

## Consequences

- emptyDir: every rollout starts with an empty cache.
- hostPath: the cache survives pod deletion and `helm uninstall`; cleanup is manual.
