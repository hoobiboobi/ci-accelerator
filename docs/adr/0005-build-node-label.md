# 0005. Build nodes are marked with our own label and taint

Date: 2026-10-17

## Context

Daemons and CI job pods must run on dedicated build nodes, and nothing else should. kubelet refuses to self-assign `node-role.kubernetes.io/*` labels (found on kind).

## Decision

Build nodes carry the label `ci-accelerator.io/node-role=build` and a `NoSchedule` taint with the same key and value. The defaults of every chart match them. The install fails when no node carries the label.

## Consequences

- Users label and taint nodes before installing.
- The label domain is ours, independent of other conventions.
