# Console (v0.2): design draft

Status: **draft for review**. Nothing here is implemented. Decisions are
proposals until this document is merged; open questions are listed at the
end.

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

### Pipelines of a project

One row per pipeline:
- pipeline number, branch, short commit, start time;
- a link to the pipeline in GitLab;
- the pipeline's build time;
- the time saved by the cache.

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
with their duration only. Jobs that ran on other runners are not visible.
The page says so: "Jobs run by this installation".

### A job

A job may run several builds: an image and a test target, or two images.
For each build the page shows:
- the target and the Dockerfile;
- the image it pushed, if any;
- a step timeline.

Each step is labelled:
- **cached**;
- **built**, with its duration;
- **received from `<job>`**, with the wait.

For a rebuilt step it shows the reason, and it links the first rebuilt step
to the line of the Dockerfile.

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
| Job pods in the jobs namespace (watch) | Job id, name, ref, commit, URL, project id and name, pipeline id (our annotation), start and end | Pods are deleted when the job ends: missed if the collector is down during the job |
| The build's trace id | GitLab instance, pipeline and job of every build ([ADR-0028](../adr/0028-one-trace-per-job.md)) | Always present in a record when the runner hook ran |
| OTLP backend (VictoriaTraces), optional | The client's spans with `ci.*` attributes, also when the history missed them | Only when the bundled storage or a compatible backend is on |

The GitLab API is not used. That needs no token and no reachability of
GitLab from the cluster. It also means no commit authors, and no jobs from
other runners.

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
                       oauth2-proxy or basic auth (user-provided)
```

- **One Go binary** with two roles: collector and web. Deployed by a new
  chart `charts/console`, standalone like `charts/buildkit`. The umbrella
  enables it through a switch that is off until the feature is complete.
- **The collector** gets its own client certificate from the installation's
  CA. It discovers daemons through the headless Service and reconnects when
  pods change.
- **No raw blobs kept.** Derived records are kept: builds, steps, edges
  "received from". The trace blob, the largest part, is read only for its
  trace id.
- **The history must outlive collector downtime.** With the console
  enabled, the chart raises the daemons' `[history]` limits (proposal:
  2000 records / 7 days). This changes `buildkitd.toml` and restarts the
  daemons once. It goes into the upgrade notes ([ADR-0027](../adr/0027-upgrades-and-branches.md)).
- **Auth:** none built in. The chart supports an oauth2-proxy sidecar or
  basic auth from a Secret. Read-only, no multi-user model.
- **Observability access** ([ADR-0021](../adr/0021-observability-access.md)):
  the console needs query access to the bundled storage. The vmauth front
  that limits builds to ingestion lands together with it.

## Data model (first version)

| Table | Key | Main fields |
|---|---|---|
| `projects` | GitLab instance + project id | name, namespace |
| `pipelines` | instance + pipeline id | project, ref, sha, first and last job times |
| `jobs` | instance + job id | pipeline, name, ref, sha, url, start, end, status |
| `builds` | daemon + record ref | job (nullable: unattributed), target, Dockerfile, start, end, error, pushed image, counts |
| `steps` | build + identity | name, Dockerfile line, state (cached / built / received), start, end, cpu, memory |
| `received_from` | waiting step → executing build | wait duration |

Schema migrations are forward-only and versioned. A collector that finds a
newer schema refuses to start (ADR-0027).

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

- Jobs on other runners. The GitLab API stays optional, later.
- Which files of the context changed (needs the CI component).
- Users, roles, per-project access control.
- GitHub Actions (ARC): the data model keeps a job source field.
- Moving builds to the daemon that holds their cache (routing work, v0.4).

## Open questions

1. **Retention:** how long should the console keep derived data? Proposal:
   90 days, configurable.
2. **History limits:** is raising them acceptable given BuildKit's
   metadata growth (moby/buildkit#7263)? Measure before deciding.
3. **Failed and cancelled builds:** should the console also keep a short
   log excerpt for them? The record holds the error and the step's logs.
4. **Several GitLab instances** behind one installation: the trace id
   carries an instance prefix, but project names could collide in the UI.
5. **Language of the UI:** English only, or translations from the start?

## Evidence

- **Spike of 2026-10-19 on BuildKit v0.33.1.** It covered:
  - what a history record contains;
  - the event stream;
  - unstable vertex digests;
  - normalisation through provenance;
  - deduplication visible as foreign vertices;
  - the 3-second window for client spans.
- **Real runs on gitlab.com** (three pipelines, three jobs each, images
  pushed to the GitLab registry):
  - every record was attributed to its job by trace id;
  - repeated work was found across daemons.
