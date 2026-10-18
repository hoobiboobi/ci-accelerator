# 0010. One box namespace and a separate jobs namespace

Date: 2026-10-17

## Context

CI jobs run untrusted code. The gitlab-runner chart's default Role covers everything in the job namespace: sharing it with the daemons would hand the runner their keys and DaemonSet. Pod Security, quotas and NetworkPolicy are per namespace.

## Decision

- The box installs into the release namespace (Pod Security `privileged`, for the daemons).
- CI job pods run in a separate namespace with Pod Security `baseline`, created by the release (switchable off).
- Client certificates are issued across namespaces, so cert-manager needs a ClusterIssuer; the CA key never goes into the jobs namespace.

## Consequences

- Uninstalling the release deletes the jobs namespace with everything in it.
- `restricted` does not work for the runner's job pods.
