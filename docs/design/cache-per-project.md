# Cache per project: what BuildKit tells us, and a design

Status: proposal (research and a spike, 2026-10-25). Nothing here is built.

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
   and keeps the latest snapshot plus each record's first-seen time. The
   cost of `DiskUsage` on a large cache (it walks every record under the
   cache manager's lock, moby/buildkit#7175) must be measured on a
   production-size cache before the interval is fixed.
2. **Owner of a layer = the build that created it.** A new record is matched
   to the builds running on that daemon at its `createdAt` that *executed*
   a step with that name (COPY/ADD by exact step name, RUN by command), and
   the parent chain must continue the same build's previous match. One
   candidate: owned by that build's project. Several projects (concurrent
   builds executing identical steps on the same parent): "ambiguous", never
   guessed. LINK merge records follow their copy record.
3. **Users of a layer = builds that hit it.** From the stored history: a
   CACHED step with the same name on the same daemon whose parent chain
   matches. This is what shows "shared with project X".
4. **Cache mounts.** With a namespace in the id: exact per project. The CI
   component and the runner hook can set `BUILDKIT_CACHE_MOUNT_NS` to the
   project path, as an opt-in input (it also isolates package caches between
   projects; existing mounts go cold once). Without it: "shared", listing
   the projects whose builds ran steps with that cache mount.
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

1. A spike with concurrent builds of two projects sharing a base and a step
   name, to count the ambiguous cases, and `DiskUsage` timing on a cache of
   hundreds of GB.
2. The matcher in the collector with fixtures from real snapshots.
3. The `BUILDKIT_CACHE_MOUNT_NS` input in the component and the hook
   (opt-in, upgrade note).
4. The UI rows.
