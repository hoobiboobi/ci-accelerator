# 0046. Cache shared across projects; no per-project namespaces

Date: 2026-10-27

## Context

BuildKit can prefix every cache mount id with a namespace
(`BUILDKIT_CACHE_MOUNT_NS`, a build argument the client sends) and a
salt in the cache key could keep projects' layers apart. Both were
considered for accounting and isolation. From the v0.33.1 source: a
namespace without a slash keeps a step's cache key and only separates the
mount's directory; one with a slash puts the mount's id into the key.
Registry cache export would let builds share layers through a registry.

## Decision

- Keep one cache per daemon shared by every project: identical steps of
  different projects reuse each other's layers and cache mounts.
- No cache mount namespace and no key salt: both make caches colder (a
  package cache per project, duplicate layers). The router passes bytes
  through and could not set them anyway.
- No registry cache export before BuildKit 0.34 (parallel export fixes
  missing in 0.33.1, cache mounts never exported, export on the job's
  critical path).

## Consequences

- One trust domain per daemon, as documented: a project can read what
  another's build left in a shared cache mount.
- How much of a node's cache belongs to which project cannot be read
  from BuildKit's records; the console shows cache by kind only.
