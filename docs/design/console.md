# Console (v0.2): design draft

> The pages described here were replaced by the builds-only console of
> [ADR-0037](../adr/0037-console-builds-only.md). The data model, store and
> reasons below still apply.

Status: **draft, reviewed**, then implemented, and later replaced as the
note above says. The text below is the draft as it was. The answers
from the review are listed at the end; sign-in, the GitLab API and the
kept build data are decided in
[ADR-0029](../adr/0029-console-gitlab-and-build-data.md).

## Goal

A developer opens the console, finds their pipeline and sees what happened
to its builds, in their own terms:

- which jobs came from cache, fully or in part;
- which steps were rebuilt, and why;
- which jobs got a result from another job and waited for it, instead of
  building it themselves;
- how much time the cache saved.

Build nodes, daemons and runner instances do not appear in this view.
They matter to whoever runs the installation. A second view serves them
(see [The operations view](#the-operations-view)).

## Who uses it

| Audience | Question | View |
|---|---|---|
| Developer | "What happened to my pipeline, and why was this job slow?" | Pipelines, pipeline, job (default) |
| DevOps / platform team | "Is the farm doing its job? Where is work repeated, where is cache lost?" | Operations |

## The developer view

### How a developer gets there

The runner's `pre_build_script`, which already runs in every job, prints one
line when the console is enabled:

```
Build details: https://<console>/jobs/<job id>
```

A developer clicks it from the GitLab job log. The console also has a
project list for browsing.

### Pipelines

One row per pipeline, newest first: pipeline number, branch, project,
commit title, how many builds ran and how many rebuilt something, start
time. Pipelines whose jobs built no image are listed too, with each job's
status as GitLab tells it; a "With builds" filter hides their jobs
([ADR-0033](../adr/0033-console-jobs-without-builds.md)). A filter per
project comes with sign-in, which decides what a user may see.

### A pipeline

One row per job of the pipeline that ran on this installation's runner.
Each row has the job name, its duration and one status:

| Status | Meaning | Shown with |
|---|---|---|
| **From cache** | Every build step came from the cache | Time saved |
| **Partly from cache** | Some steps were rebuilt | The first rebuilt step and the reason (see [Reasons](#reasons-for-a-rebuild)) |
| **Got results from another job** | Some steps were being built by another job at the same time; this job waited and took the result | Which job, which step, how long it waited |
| **Built** | Nothing came from cache: a first build, or the cache was not reachable | The reason when one is known |
| **Failed** | The build failed | The failing step, its command and exit code, the last log lines |

Jobs that run no build (tests outside BuildKit, deploy scripts) are listed
with GitLab's status (passed, failed, canceled, running, waiting for a
runner or earlier jobs, manual, skipped) and their time; without a token
the console does not know how they ended and says so. A pipeline with no
build at all has its own summary instead of build figures: jobs by status,
time waiting for runners, the slowest job. Jobs that ran on other runners
appear when GitLab is read.

Two more tabs show the pipeline as a whole:
- **Timeline:** every job on one time axis, with the time it waited for a
  runner (GitLab's `queued_duration`), a build's steps coloured by result;
  the job that ended last (it sets the pipeline's time) is outlined, and
  none is while GitLab says the pipeline goes on;
- **Dockerfile:** the Dockerfile with one column per job, so a line shows
  which jobs built it, took it from cache or waited for another job.

### A job

A job may run several builds: an image and a test target, or two images.
For each build the page shows:
- the cause first: one sentence on why it was slow or rebuilt, and what
  helps;
- where the time went: runner and checkout, sending the context, pulling
  the base image, waiting for another job, building, exporting and pushing;
- the target and the image it pushed, if any;
- three tabs:
  - **Steps:** a table grouped by stage, runs of cached stages folded,
    with when each step ran;
  - **Dockerfile:** the Dockerfile this build used, as recorded by
    BuildKit. Each line shows its step's result and duration; stages the
    target does not use are dimmed. Hovering a line explains what
    happened to it. Clicking opens the step: the command, its output and
    its history over earlier builds;
  - **Timeline:** the job on a time axis: runner and checkout, sending the
    context, each stage and step, export and push.
- beside them, the build's facts: node, the node of the target's last
  build, job and BuildKit time, the target's usual time, context size, job
  id and trace id;
- below, the previous build of the target step by step, and the details
  for operators.

The time before and after the build (runner, checkout, script) comes from
GitLab's API when the console has a token, from the job pod otherwise
(ADR-0032).

A job that built no image has a simpler page: what GitLab says it did,
where its time went (waiting for a runner, running), its facts (stage,
runner, node, times), its runs in the project's earlier pipelines, and the
end of its log, read from GitLab when the reader asks (secrets masked).

Each step is labelled:
- **cached**;
- **built**, with its duration;
- **received from `<job>`**, with the wait.

For a rebuilt step it shows the reason, and it links the first rebuilt step
to the line of the Dockerfile.

### Project insights and build nodes

**Project insights** answer "is it getting worse": the median pipeline
time, how many steps were reused, the build time the cache saved, how
many rebuilds were the node's fault, why builds were rebuilt (count and
time), each pipeline's time, and every step of a target across its last
builds.

**Build nodes** are the operations view: per daemon its node, builds
served, rebuilds because of the node, builds running now (and how long the
oldest runs), its cache by kind (base images, build layers, cache mounts,
contexts) as BuildKit counts it, its cache mounts and when they expire,
base images held by several nodes, and the cache policies. Free disk
space and cache per project are not reported by BuildKit (planned: a disk
probe in the daemon pod).

### Reasons for a rebuild

The console compares a build with the previous successful build of the same
project, Dockerfile and target. The first step that was rebuilt gets one of
these reasons:

| Reason | How it is known | Example text |
|---|---|---|
| The step changed | Its normalised definition differs from the previous build's | "Line 8 changed: `RUN echo build 1001` → `RUN echo build 1002`" |
| A build argument changed | The definitions differ only in a build argument's value | "Build argument `VERSION` changed (1001 → 1002)" |
| The base image changed | The resolved base image digest differs | "`python:3.13-slim` now points to a new image" |
| Files it reads changed | Same definition, rebuilt anyway, and it reads the build context | "`COPY src/` read changed files" |
| Not in the cache | Same definition, no file input: evicted, or built elsewhere | "Not found in the cache" |
| The cache is on another node | Same definition; the previous build ran on another node; and the step reads no files, or another job of the same pipeline (same commit) found this step in the cache on its node | "Not your change: the cache for `COPY common/` is on the node this target used last time" |
| Files or node | Same definition, previous build on another node, and no job of the pipeline settles it | "Either `common/` changed, or the cache is on another node; the records cannot tell which" |

When another job of the same pipeline ran on its own previous node and had
to rebuild the same step, the files changed: the reason is "files it reads
changed" for every job of the pipeline.

The console cannot say *which* files changed: BuildKit records no content
checksums. A later CI component could report a manifest of the context.
Without that, the page names the `COPY`/`ADD` step and its paths.

### Waiting for another job

When two builds need the same step at the same time, BuildKit runs it once
and the second build waits. A record shows this exactly:
- the waiting build's progress contains the other build's vertex for that
  step, and that vertex is not in its own `digestMapping`;
- the other build's record does have it in its `digestMapping`;
- that record's trace id names the job.

The wait lasts from the moment the waiting build reached the step until the
other build finished it. Both moments are in the records.

**Privacy.** If the other build belongs to a different project, the page
says "received from another build" and does not name the project or the
job. Within one project it names the job and the pipeline.

### Time saved

For a cached or received step: the duration of its last execution with the
same normalised definition on record. The page shows this as an estimate
("saved about 3 min").

## The operations view

For whoever runs the installation, behind its own entry in the menu:

- builds per daemon and node over time;
- **repeated work**: steps executed in several jobs of one pipeline on
  different daemons, with the time spent;
- **cache locality**: how often a project's build lands on a daemon that
  holds its previous build;
- history health: records per daemon, oldest record, records missed while
  the collector was down;
- daemon restarts.

The sketch from 2026-10-19 (three gitlab.com pipelines, three jobs each)
showed why this view matters. Every pipeline's jobs landed on three
different daemons, so the shared dependency stage ran three times per
pipeline: 47 s of repeated work in three small pipelines.

## Data sources

| Source | What it gives | Reliability |
|---|---|---|
| BuildKit build history, per daemon (`ListenBuildHistory`, content API) | Every build: steps, cached flags, timings, errors, base images, pushed image; provenance with the build definition, Dockerfile lines and resource usage; the build's trace | Events for every build. Kept only up to `[history]` limits (48 h / 50 records by default): the collector must keep up. The trace is attached up to 3 s after the build |
| Job pods in the jobs namespace (watch) | Job id, name, ref, commit, URL, project id and name, pipeline id (our annotation), start, and "running now"; the end is when the pod's deletion is asked | A pod never knows how its job ended, does not exist while the job waits, and outlives it: never a source of status ([ADR-0033](../adr/0033-console-jobs-without-builds.md)) |
| GitLab's API, with a token | Every job of a pipeline: status, queue time, start, end, runner; a job's log when asked | Read while a page shows the pipeline: a running one every 20 s at most, a finished one again only when a job of it ran since |
| The build's trace id | GitLab instance, pipeline and job of every build ([ADR-0028](../adr/0028-one-trace-per-job.md)) | Always present in a record when the runner hook ran |
| OTLP backend (VictoriaTraces), optional | The client's spans with `ci.*` attributes, also when the history missed them | Only when the bundled storage or a compatible backend is on |

Job pods carry, as annotations, the CI variables a page needs: job name and
stage, commit title and author, ref, merge request, pipeline source, job
start and pipeline creation. They need no token.

The GitLab API is used only while a page is open (with sign-in, as the
signed-in user; for now with one token): one request per pipeline for its
jobs' status and times, again every 20 s while it runs and a page shows it.
A finished pipeline is read again only when a job of it ran after GitLab's
last answer (a manual job, a retry); nothing polls GitLab otherwise. A page
renders without it when GitLab is unreachable or rate-limited. The console
is never reachable from GitLab: no webhooks.

A build whose record has no trace id of our scheme (a job that set its own
`TRACEPARENT`, or a client other than a runner job) is kept as
"unattributed". If its trace holds several `ci.job.id` values, it is shown
as ambiguous; it is never assigned by guessing.

## Step identity

Steps of different builds are compared by a **normalised identity**, not by
BuildKit's vertex digests:

- A vertex digest changes in every build for each step that depends on the
  build context, even when the step is cached. The local source carries
  per-session data.
- The provenance records each step's LLB operation without that data;
  `digestMapping` joins progress vertices to these steps.
- Identity = hash of the operation and of its inputs' identities. Local
  sources are reduced to their name: the list of paths a target needs
  differs between targets of one Dockerfile, but it is not what a step
  reads.
- The `exporting to image` vertex is not an LLB step; it belongs to the
  build itself.

These rules come from the
[spike of 2026-10-19](#evidence) and the gitlab.com runs; each needs a
fixture in the collector's tests.

## Architecture

```
 build daemons (DaemonSet)          jobs namespace
   history API :1234 (mTLS)            job pods (annotations)
          │                                  │ watch (get/list/watch pods)
          ▼                                  ▼
   ┌──────────────────── collector (Go, one replica) ──────────────────┐
   │ one ListenBuildHistory stream per daemon (headless Service)       │
   │ backfill on start, upsert by record ref and generation            │
   │ fetch logs + provenance (+ trace) blobs once, keep derived data   │
   └──────────────────────────────┬─────────────────────────────────────┘
                                  ▼
                       store: SQLite (PVC) or Postgres
                                  ▼
                       console: read-only HTTP UI + /api/v1
                                  ▼
                       sign-in through GitLab, in the console (ADR-0034)
```

- **One Go binary** with two roles: collector and web. Deployed by a new
  chart `charts/console`, standalone like `charts/buildkit`. The umbrella
  enables it through a switch that is off until the feature is complete.
- **The collector** gets its own client certificate from the installation's
  CA. It discovers daemons through the headless Service and reconnects when
  pods change.
- **No raw blobs kept.** Derived records are kept: builds, steps, edges
  "received from", the Dockerfile text with each step's lines, and each
  step's output (capped per step, known secret formats masked). The trace
  blob, the largest part, is read only for its trace id.
- **Retention:** derived data 90 days by default (30 days since
  ADR-0037), step output 7 days
  (both configurable).
- **The history must outlive collector downtime.** With the console
  enabled, the chart raises the daemons' `[history]` limits (proposal:
  2000 records / 7 days). This changes `buildkitd.toml` and restarts the
  daemons once. It goes into the upgrade notes ([ADR-0027](../adr/0027-upgrades-and-branches.md)).
- **Sign-in through GitLab.** A user sees a project's builds only when
  GitLab lets them read that project's jobs. This is checked as that user
  and cached for a few minutes. Administrators are GitLab usernames named
  in the configuration ([ADR-0034](../adr/0034-console-sign-in.md)). The
  console is read-only and has no users or roles of its own.
- **Disk usage** of each daemon's cache directory comes from a small sidecar
  in the daemon pod; BuildKit's API does not report it.
- **Web stack:** Go `html/template` and `go:embed`, server-rendered SVG, a
  small script with server-sent events for live updates. No single-page
  framework. Every page and the step panel work without the script; the
  script adds the hints, the panel and the keyboard (arrows, Enter, Esc).
  Everything shown comes from builds and CI jobs, so it is escaped, and
  the content security policy allows only the console's own script and
  stylesheet: no inline code, no other origins. Static files are linked
  by a content version and cached for a year.
- **Observability access** ([ADR-0021](../adr/0021-observability-access.md)):
  the console needs query access to the bundled storage. The vmauth front
  that limits builds to ingestion lands together with it.

## Store

SQLite by default, Postgres as an option for larger installations
([ADR-0030](../adr/0030-console-store.md)). The queries are relational:
pipeline → jobs → builds → steps, the history of one step across builds,
reasons counted per project, the previous build of a target. The write
rate is small (1000 builds per hour is 0.3 per second).

**One process writes.** The collector and the web server are one binary
in one pod with one replica, updated with `Recreate`, so two processes
never open the files at once. The volume must be block storage
(`ReadWriteOnce`), never NFS or another shared file system: SQLite's
locking and its WAL shared memory do not work there.

### Main database

Times are Unix milliseconds (UTC); step times are offsets from the build's
start. Hashes are the first 8 bytes of a SHA-256, kept as a 64-bit integer.

| Table | Key | Fields | Why |
|---|---|---|---|
| `meta` | name | schema facts; the key used to mask secrets | |
| `daemons` | id | pod name, node | Interned: every build refers to one |
| `projects` | instance, project id | path | From job pod annotations |
| `pipelines` | instance, pipeline id | project, ref, commit, source, created, commit title and author, merge request | From job pod annotations |
| `jobs` | instance, job id | pipeline, name, stage, started, node of the pod | From job pod annotations |
| `dockerfiles` | content hash | file name, text, last seen | Stored once, shared by every build that used it |
| `step_defs` | step identity | the step's operation (normalised, secrets masked), its inputs' identities, its name, last seen | Stored once per identity, so any earlier build can be compared with a new one without its provenance |
| `builds` | id (ingest order); unique daemon + record ref | generation, attribution, instance, pipeline, job, project, target, Dockerfile, build arguments (secret values masked), start, end, error, context size, images, reason (kind, step, old, new, argument, paths, settling build), time saved, output segment | |
| `steps` | build, index | name (only without an identity), identity, state, start, duration, Dockerfile lines, received from, error, output length | |

Indexes follow the pages:
- `builds (instance, job)`, `builds (instance, pipeline)`;
- `builds (instance, project, target, start)` for the previous build;
- `builds (completed)` for retention;
- `steps (identity, build)` for a step's history and the time it saves.

**Identities make old builds comparable.** A step identity is a hash of the
step's operation and of its inputs' identities, so `step_defs` is a
content-addressed copy of the build graphs, like BuildKit's own. A reason
needs the previous build's operations: they are read back from
`step_defs`, and the raw provenance is never kept.

**Secrets.** Before a step's operation is hashed or stored, the value of
every environment variable whose name says it is a secret becomes a keyed
hash (HMAC with a per-installation key in `meta`). A changed secret still
changes the identity, so "build argument `API_TOKEN` changed" is still
found, but no value is stored. A weak secret could be guessed by whoever
holds the database file; the docs keep saying not to pass secrets as build
arguments.

**What is not stored:** the trace (only its id is used), BuildKit's vertex
digests, raw provenance. Linking received steps to the build that executed
them happens in memory, over the builds of the last two hours per daemon:
after a restart the collector reads that window again from the daemons'
history.

### Step output: compressed blocks in daily files

Step output is most of the data and the only part kept for 7 days. It is a
key → value store:

- **Key:** build id and step index, packed into one integer (the table's
  row id).
- **Value:** the output, masked and clipped (64 KiB per step), compressed
  with zstd.
- **One SQLite file per UTC day.** Retention deletes whole files: no
  `DELETE`, no fragmentation, no `VACUUM` for the largest data. A build
  row names its file.
- **Dictionaries.** Output compressed step by step is small and compresses
  poorly on its own; with a dictionary of the same project's recent output
  it compresses four times better (measured below). Each daily file holds
  its own dictionaries, built from the project's latest output when the
  project first writes that day, and immutable afterwards. A project
  without earlier output writes without one until it has some. Every block
  names its dictionary. A file never depends on another file, so deleting
  an old day never breaks a newer one.

Measured on the output of 21 real gitlab.com builds (26 steps with output,
85 KB):

| Compression | Size | Ratio |
|---|---|---|
| Each step alone, deflate | 20.5 KB | ×4.2 |
| Each step alone, zstd | 21.9 KB | ×3.9 |
| Each build alone, zstd | 21.1 KB | ×4.0 |
| Each step, zstd with the project's earlier output as dictionary | 5.3 KB | ×16.2 |
| Each build, zstd with that dictionary | 5.0 KB | ×17.0 |

Steps stay separate blocks: a page reads one step, and whole builds gain
little. Compressed output cannot be searched with SQL; searching output is
not in v0.2.

### Retention and migrations

- **Daily:**
  - delete builds older than 90 days (30 since ADR-0037) in small batches
    (steps go with them);
  - delete Dockerfiles and step definitions not seen within that time;
  - return free pages to the file system (`auto_vacuum = INCREMENTAL`);
  - delete output files older than 7 days.
- **Schema version** in `PRAGMA user_version`. Migrations are forward-only.
  A collector that finds a newer schema refuses to start
  ([ADR-0027](../adr/0027-upgrades-and-branches.md)).
- **Backups** are optional: everything is derived from builds. A copy can be
  taken with `VACUUM INTO`.

## Scale

- **Per build:** 20–40 KB of provenance and 5–20 KB of logs are read (seen
  in the spike and on gitlab.com); a few hundred bytes per step are stored.
  The trace is read for its id.
- **Throughput:** 1000 builds/hour is 0.3 builds/s. One collector handles
  that; SQLite is enough for one installation, Postgres for larger ones or
  HA.
- **One trace per job** keeps every record's trace bounded by its own job
  ([ADR-0028](../adr/0028-one-trace-per-job.md)).

## Delivery and compatibility

- **Pull requests.** It arrives in several pull requests behind
  `console.enabled: false`:
  1. collector + store;
  2. pipeline and job pages;
  3. operations view;
  4. log link and history limits.

  Enabling it by default is the v0.2.0 change.
- **New keys.** New values keys are read with defaults. The e2e `upgrade`
  scenario checks every step against the last release.
- **API.** `/api/v1` from the first version.

## Out of scope for v0.2

- Which files of the context changed (needs the CI component).
- Users and roles of the console's own; permissions come from GitLab.
- Cache size per project: BuildKit's cache records do not say which build
  created them.
- GitLab webhooks.
- Translations: the UI is in English.
- GitHub Actions (ARC): the data model keeps a job source field.
- Moving builds to the daemon that holds their cache (routing work, v0.4).

## Decisions from the review

1. **Retention:** derived data is kept 90 days by default (configurable;
   30 days since ADR-0037).
   Log excerpts of failed builds are kept 7 days.
2. **History limits:** raising the daemons' `[history]` limits when the
   console is enabled is accepted (proposal: 2000 records / 7 days). The
   metadata growth (moby/buildkit#7263) is measured before the default is
   fixed.
3. **Failed builds:** the console keeps the error, the failing step and an
   excerpt of its log lines, for 7 days.
4. **Several GitLab instances** behind one installation (for example the
   umbrella's runner on gitlab.com and an existing runner of a self-managed
   GitLab attached to the same daemons): every key carries the GitLab
   instance, taken from the trace id's prefix, so equal project, pipeline
   or job numbers of two instances never merge. The UI names the instance
   only when there is more than one.
5. **Language:** the UI is in English only.
6. **GitLab, kept data and views** ([ADR-0029](../adr/0029-console-gitlab-and-build-data.md)):
   sign-in and permissions through GitLab, the API only while a page is
   open, job data from pod annotations, the Dockerfile and step output kept
   per build, the build and pipeline tabs above, and "not your change" only
   with proof from the same pipeline.
7. **Store** ([ADR-0030](../adr/0030-console-store.md)): SQLite with one
   writing pod, a content-addressed step graph instead of raw provenance,
   and step output in daily files compressed with per-project
   dictionaries.

## Evidence

- **Spike of 2026-10-19 on BuildKit v0.33.1.** It covered:
  - what a history record contains;
  - the event stream;
  - unstable vertex digests;
  - normalisation through provenance;
  - deduplication visible as foreign vertices;
  - the 3-second window for client spans.
- **Real runs on gitlab.com** (six pipelines, three or four jobs each,
  images pushed to the GitLab registry):
  - every record was attributed to its job by trace id;
  - repeated work was found across daemons;
  - every record's provenance held the full Dockerfile and each step's
    lines, and its progress held each step's output;
  - of 8 builds whose previous build ran on another node, 1 was proven a
    pure node effect by another job of its pipeline, 3 had changed files,
    4 could not be told apart.
- **A comparison of three UI designs** on those builds, by task time for
  eight tasks of developers and platform teams. It led to the views above.
