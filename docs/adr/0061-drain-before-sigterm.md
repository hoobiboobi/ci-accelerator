# 0061. A daemon drains before SIGTERM; the collector reads it to the end

Date: 2026-11-13

## Context

[ADR-0014](0014-termination.md) ends a daemon with SIGTERM: buildkitd
stops listening and lets its running builds finish, up to the pod's
grace period. The lab's kill check (2026-11-11) showed what that costs
with the default `emptyDir` storage: a daemon deleted in the middle of a
five-minute build finished it and the CI job passed, but the console
never stored that build. Two causes: the controller stopped reading a
daemon's history as soon as its address left the headless Service,
minutes before its builds ended; and buildkitd exits right after its
last build, before the record's blobs could be read. With `hostPath`
the next pod serves the old history and the build arrives late; with
`emptyDir` it is gone. So every rollout of a default install lost the
builds that were still running.

## Decision

- The daemon's `preStop` hook sleeps `preStopSleepSeconds` (as before,
  while the endpoint is withdrawn), then waits while `buildctl debug
  histories` lists a running build, then 15 s more so the controller
  reads the last records; all within `terminationGracePeriodSeconds`.
  buildkitd serves everything meanwhile (no new job reaches it: its
  endpoint is gone), and SIGTERM then finds nothing to drain.
- The controller keeps reading a daemon whose address left the Service
  until the daemon closes the stream, at most 35 minutes, never redials
  it and forgets it then; an address used again is a new daemon.

## Consequences

- Builds that finish while a daemon stops are stored, with any storage.
- A rollout is not slower than before: the grace period already waited
  for the running builds; the 15 s come on top per node.
- Unit tests cover the leaving stream and a reused address; the e2e
  graceful scenario covers the drain. Measured on the lab stand: the
  build that was lost before is stored after the change.
