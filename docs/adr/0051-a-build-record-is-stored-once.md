# 0051. A build record is stored once; repairs keep the schema version

Date: 2026-10-28

## Context

A daemon pod replaced on a hostPath (a drain, an eviction, a rollout) keeps
BuildKit's history on that disk, and the new pod sends the old pod's records
again. The console keyed a build by record ref and daemon pod, so 0.3.0 and
0.3.1 stored every one of them a second time under the new pod: counts,
medians and job pages doubled after each rollout (seen on a production
install by the README, 2026-10-28). A record's ref is BuildKit's random id
of the build. The store's schema has a version, and a controller refuses a
newer one, so a migration would make `helm rollback` to 0.3.1 fail.

## Decision

- A ref is stored once. A record already stored belongs to the daemon pod
  that sent it first, also in memory, where reasons compare the daemons
  builds ran on; a unique index keeps it so.
- The copies 0.3.0 and 0.3.1 made are removed on start: what pointed at a
  copy points at the build it copied, and the images' daily figures from
  the first copy's day on are summed up again.
- That runs as a repair on every start, not as a migration: changes of
  data, and indexes the releases since the schema version tolerate, do not
  move the version. 0.3.1 on a repaired store logs "record not stored" for
  a replayed record instead of storing it twice.

## Consequences

- `helm rollback` from 0.3.2 to 0.3.1 keeps working on the same store.
- The e2e upgrade replaces a hostPath daemon pod after the upgrade and
  checks the earlier job still has one build, under the pod that ran it.
