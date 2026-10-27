# Cache per project: what BuildKit tells us, and a design

Status: proposal (research and a spike, 2026-10-25). Built and removed
again on 2026-10-27, before a release: matching records to builds by
description and creation time is a guess, and a figure on the Nodes page
(with a removal action on it) must not stand on a guess. Nothing of it
ships. The way to exact figures is upstream (below, "Not chosen"); a
cache per project is a router pool (dedicated nodes), not attribution.

## Question

How much of each daemon's cache belongs to which CI project, so the console
can show it next to the disk figures (and an operator can see who fills a
build node)?

## What BuildKit v0.33.1 offers

From the source (moby/buildkit at v0.33.1) and a spike against a daemon of
the box on kind (`buildctl du --format json` before and after each of four
builds of two small projects):

- **`DiskUsage` records** have `id`, `parents`, `description`, `recordType`,
  `size` (the record's own layer, not cumulative), `createdAt`,
  `lastUsedAt`, `usageCount`, `inUse`, `mutable`, `shared`. No build id,
  session, project, blob digest or vertex digest
  (`api/services/control/control.proto`, `cache/manager.go: DiskUsage`).
  Server-side filters: id, parents, description, type, inuse,
  mutable/immutable, shared/private.
- **Descriptions are predictable** (seen in the spike):
  - COPY/ADD: the step name as the build history shows it,
    `[stage-0 3/5] COPY f.txt /f.txt`; `COPY --link` gives two records, the
    copy and a `LINK ...` merge record with two parents and size 0.
  - RUN: `mount / from exec /bin/sh -c <command>`.
  - Base images: `pulled from docker.io/library/alpine:3.22@sha256:...`.
  - Cache mounts (`exec.cachemount`): `cached mount <target> from exec
    <command of the first creator> with id "<namespace>/<target>"`.
  - Build contexts: `local source for context` / `... for dockerfile`.
- **Parents** chain each layer to the previous step down to the base image,
  so a build's layers form a path in the record tree.
- **Cache hits do not touch the chain.** A fully cached rebuild advanced
  `lastUsedAt` only on the result record (the last merge record) and on the
  context records, not on the cached layers below. "Who else uses this
  layer" cannot be read from `lastUsedAt`.
- **Cache-mount counters lag.** A cache mount's `usageCount` changed in the
  snapshot after the *next* build: the reference is released later. A
  snapshot right after a build is off by one build for cache mounts.
- **Cache mounts can carry the project.** The Dockerfile frontend prefixes
  every cache mount id with `BUILDKIT_CACHE_MOUNT_NS`
  (`frontend/dockerfile/dockerfile2llb/convert_runmount.go`). With
  `--opt build-arg:BUILDKIT_CACHE_MOUNT_NS=group/projA` the description reads
  `... with id "group/projA//root/.cache"`; without it, `//root/.cache`,
  one record shared by every project that uses that target.
- **Build contexts are a pool.** buildctl sets no session shared key, so all
  CI jobs reuse the same `local source` records (the spike daemon's were two
  days old and used by every project since).
- **History does not list cache records.** History records carry steps
  (name, cached or not, times) and exporter results, not cache record ids
  (`solver/llbsolver/history.go`). The only cache key to record map is
  `cache.db`, reachable only through the debug server (never enabled) or
  offline.

## Design

1. **Snapshots.** The collector takes `DiskUsage` per daemon at most once a
   minute while builds complete (as GC does) and every 10 minutes when idle,
   and keeps the latest snapshot plus each record's first-seen time.
   Cost, measured (2026-10-25, a separate buildkitd v0.33.1 on kind/arm64,
   records made by builds of 400 RUN steps each): `buildctl du --format
   json` takes about 12.5 ms per 1000 records (0.02 s empty, 0.25 s at
   20 000 records), twice that while five builds run (0.5 s at 21 000).
   `DiskUsage` holds the cache manager's lock while it walks the records in
   memory (`cache/manager.go`), so builds wait for that part; sizes that
   are not known yet are computed afterwards, outside the lock, in
   parallel, and kept, so only the first snapshot after a build walks the
   new layers' files. A 2.4 TB cache of 20-50 MB records (50 000-120 000
   records) would hold the lock for roughly 1-3 s per snapshot: fine once a
   minute, not on every page view.
2. **Owner of a layer = the build that created it.** A new record is matched
   to the builds running on that daemon at its `createdAt` that *executed*
   a step with that name (COPY/ADD by exact step name, RUN by command), and
   the parent chain must continue the same build's previous match. One
   candidate: owned by that build's project. Several projects (concurrent
   builds executing identical steps on the same parent): "ambiguous", never
   guessed. LINK merge records follow their copy record.
   Seen in the concurrency spike (two projects with the same Dockerfile,
   built at the same time): a step both builds share is created once and
   both builds report it as executed (`DONE`), so it is really shared;
   steps whose content differs give two records with the same
   description and the same parent, created 2-22 ms apart, and everything
   after them forms two symmetric chains. Nothing in `DiskUsage` tells the
   two chains apart, only their creation times, and the matcher does not
   depend on timing. Such records go to a row "shared by A and B" with
   their total bytes. Within one project (the jobs of a monorepo) the
   question does not arise: both chains belong to it.
3. **Users of a layer = builds that hit it.** From the stored history: a
   CACHED step with the same name on the same daemon whose parent chain
   matches. This is what shows "shared with project X".
4. **Cache mounts.** Without a namespace: "shared", listing the projects
   whose builds ran steps with that cache mount. With a namespace in the id
   they would be exact per project, but the namespace is a build argument
   the client sends, so any client can name another project. It is left to
   the router (v0.5), which can set it from the authenticated project in
   every request; not through the CI component or a wrapper in the runner
   hook. Notes for then, from the v0.33.1 source (not yet tested): the
   frontend prefixes explicit `id=` values too (convert_runmount.go); a
   namespace with a slash (the project path) drops a default mount's id
   from the step's cache key (exec.go `checkShouldClearCacheOpts`): one
   rebuild when it is switched on, layers still shared between projects;
   one without a slash keeps it and splits the layer cache per project.
   Costs: duplicated package caches, a cold package cache per new project,
   orphaned mounts until GC removes them.
5. **Base images** ("pulled from"): a row of their own, with the projects
   whose Dockerfiles use them; not split.
6. **Build contexts:** one pool row per daemon.
7. **Coverage, always shown.** Every figure says how much of the daemon's
   bytes it explains, e.g. "87% attributed; 9% base images; 4% unclear".

Where it shows: on the Nodes page per daemon (top projects by owned bytes,
plus the pool rows), and on an image's page (its owned bytes per daemon,
and which layers are shared).

Not chosen: a sidecar reading BuildKit's databases (locked by the daemon,
no project in them anyway). Worth proposing upstream: record the history id
that created and last used each cache record and expose it in
`UsageRecord`; that would make attribution exact and timing-free.

## Next steps

1. Done (2026-10-25): the concurrency spike and the `DiskUsage` timing
   (above). Still open: how often different projects run identical step
   names at the same moment on one daemon in a real installation (from the
   console's stored history).
2. The matcher in the collector with fixtures from real snapshots.
3. The UI rows.
4. With the router: the cache-mount namespace per project (above).
