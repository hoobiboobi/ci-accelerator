# 0030. Console store: SQLite, a content-addressed step graph, daily output files

Date: 2026-10-20

## Context

The console keeps every build for 90 days and every step's output for 7 days ([ADR-0029](0029-console-gitlab-and-build-data.md)). Its queries are relational:
- a pipeline's jobs, builds and steps;
- one step's history across builds;
- reasons counted per project;
- the previous build of a target.

At 1000 builds per hour the store receives 0.3 builds per second.

Options considered:
- **Embedded key-value stores** (bbolt, Badger, Pebble): queries and indexes would have to be written by hand. bbolt files do not shrink without compaction; BuildKit's own metadata database shows that (moby/buildkit#7263).
- **MongoDB:** a separate server, and SSPL, which many companies do not allow.
- **ClickHouse:** a heavy separate server. GitLab's own CI analytics need one, and that keeps them away from most self-managed installations.
- **DuckDB:** an analytics engine whose Go driver needs cgo.
- **Redis:** keeps data in memory and has no queries.
- **The bundled VictoriaMetrics and VictoriaTraces:** builds can still read and delete their data ([ADR-0021](0021-observability-access.md)), which rules them out for step output that may hold secrets.

Two facts shape the design:
- **Old builds must stay comparable.** A reason compares a build with the previous one, which may be weeks old, but raw provenance (20–40 KB per build, with build-argument values in it) must not be kept.
- **Step output compresses poorly block by block.** On 21 real builds, zstd gave ×3.9 for single steps. With the project's earlier output as a dictionary it gave ×16.2.

## Decision

- **SQLite by default**, through `database/sql` with portable SQL so that Postgres can be added for larger installations.
  - Driver: `modernc.org/sqlite`, pure Go, no cgo.
  - Pragmas: WAL, `synchronous = NORMAL`, foreign keys, `auto_vacuum = INCREMENTAL`.
- **One writer.** The collector and the web server run as one pod with one replica, updated with `Recreate`, on a `ReadWriteOnce` block volume. NFS and other shared file systems are refused in the docs.
- **A content-addressed step graph.** `step_defs` keeps each step identity once: the normalised operation with secret values replaced by keyed hashes, and its inputs' identities. A reason reads the previous build back from it. Dockerfiles are kept once per content.
- **Step output** goes into one SQLite file per UTC day, as a key → value table:
  - key: build and step;
  - value: zstd-compressed output, with per-project dictionaries that live in the same file.
  - Retention deletes whole files.
- **Linking in memory.** "Received from another build" is resolved over the last two hours of builds per daemon. After a restart the window is read again from the daemons' history. Vertex digests are not stored.
- **Migrations** are forward-only and recorded in `PRAGMA user_version`. A newer schema is refused.

## Consequences

- **No database server** to run. The data is derived from builds, so losing it loses history, not builds. `VACUUM INTO` takes a copy.
- **No high availability** and no separate web replicas with SQLite; that is what the Postgres option is for.
- **Output cannot be searched with SQL**, because it is compressed. Searching output is not in v0.2.
- **A weak secret passed as a build argument** could be guessed from its keyed hash by whoever holds the database file and its key. BuildKit secrets are the way to pass secrets.
- **Records missed for more than two hours.** If the collector is down longer than that, a received step may lose the link to the build that executed it. It still shows as received.
