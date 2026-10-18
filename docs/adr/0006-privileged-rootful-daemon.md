# 0006. Rootful, privileged daemons by default

Date: 2026-10-17

## Context

The production numbers come from the root `moby/buildkit` image running privileged. The rootless variant needs unconfined seccomp/AppArmor, user namespaces and, on some kernels, a slower snapshotter.

## Decision

The daemon runs as a privileged rootful pod. Rootless is an opt-in to be added only together with end-to-end coverage. The install refuses a namespace whose Pod Security level forbids privileged pods.

## Consequences

- The daemons' namespace must be labelled `pod-security.kubernetes.io/enforce=privileged`.
- One installation is one trust domain (README, Limitations).
