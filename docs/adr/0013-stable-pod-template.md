# 0013. A pod template that chart releases do not change

Date: 2026-10-18

## Context

Any change to a DaemonSet's pod template restarts every daemon, and with emptyDir empties every cache.

## Decision

The pod template carries only selector, part-of and component labels (plus user labels); chart version labels stay on object metadata. User labels cannot override the chart's own. The end-to-end run checks that a chart-version-only upgrade keeps the same pods.

## Consequences

Upgrading the chart without changing the configuration does not restart daemons.
