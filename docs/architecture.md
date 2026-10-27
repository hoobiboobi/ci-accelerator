# Architecture

How the box works: where things run, how a build travels from a CI job to
a daemon and back, where the cache lives, and what happens during
rollouts. The decisions behind it are in [adr/](adr/README.md).

## Components and namespaces

[![The box namespace holds the gitlab-runner manager, the build router's Service, the daemons' headless Service, the daemon DaemonSet, the control plane (controller, web and router) and the daemons' NetworkPolicy. The runner creates CI job pods in the jobs namespace, which reach the daemons through the router over mutual TLS. The release's CA in cert-manager's namespace signs the daemons' server certificate and the jobs' client certificate.](images/components-and-namespaces.svg)](images/components-and-namespaces.svg)

| Part | Where | Notes |
|---|---|---|
| buildkitd | DaemonSet on build nodes (`ci-accelerator.io/node-role=build`) | Privileged, rootful; one per node; its own PriorityClass |
| Disk probe | A container in each daemon pod (box) | The ci-accelerator image; reports the cache disk's size and free space (statfs) |
| gitlab-runner manager | Box namespace, any non-build node | Polls GitLab, creates job pods |
| CI job pods | Jobs namespace, build nodes | Thin clients: `buildctl` and the build context |
| Build router (control plane) | DaemonSet on build nodes | Passes each job's connection to the daemon where its project is warm; no key, no state |
| Controller (control plane, optional) | Box namespace, any non-build node | One pod: reads the daemons' build history and the job pods, holds the store and the sessions |
| Web (control plane, optional) | Box namespace, any non-build node | Two pods by default: serve the console's pages, reading the controller's API |
| CA, server and client certificates | cert-manager | The CA lives in cert-manager's namespace |

The jobs namespace is separate because job pods run untrusted code and the
runner needs broad rights there; the daemons' keys must not be in reach
([ADR-0010](adr/0010-namespaces.md)).

**Priority and resources.** The kubelet evicts pods under memory or disk
pressure that use more than they requested, in order of priority. The
daemons therefore have their own PriorityClass (`<namespace>-<release>-buildkit`,
value 1000000, `preemptionPolicy: Never`: above CI jobs, and never evicting
a running job to get scheduled) and default requests of 500m CPU and 512Mi
([ADR-0043](adr/0043-daemon-priority-and-disk-probe.md)).

## The path of a build

1. **The runner creates a job pod** in the jobs namespace, on a build node.
   It sets `BUILDKIT_REMOTE_ADDR=tcp://<release>-router.<namespace>.svc:1234`
   and mounts the client certificate at `BUILDKIT_TLS_DIR`.
2. **`buildctl build` connects to the router** on the job's own node
   (`trafficDistribution: PreferSameNode`; another node's router when it
   is not ready). The router knows the job by its pod's address and picks
   the daemon ([ADR-0045](adr/0045-build-router.md)):
   - the job's own node, if its cache holds layers of the project used in
     the last week;
   - else the node of its zone whose cache holds the project's most
     recently used layers;
   - else the project's node by a rendezvous hash over the zone's ready
     daemons: every router agrees, so a new project's parallel jobs meet
     on one daemon and BuildKit runs their common steps once;
   - else another zone's daemon (or none, with `crossZone: false`).

   Zones are the daemons' nodes' `topology.kubernetes.io/zone`; with
   `control-plane.router.zones: false` every daemon counts as one zone
   (zones close enough that crossing them costs nothing).

   Warmth is the project's builds on the node in the last week (builds of
   a daemon replaced since count only when its cache survives restarts:
   hostPath, which the daemon states in a worker label) and the routers'
   decisions of the last hour. A connection from a pod that is no CI job
   goes to its own node's daemon. The routers learn where projects built from the controller
   (and keep routing on their last view while it is away); the build page
   says why its node was chosen.
3. **Mutual TLS, end to end.** The router passes the bytes through. The
   client checks the daemon's certificate, whose names cover the daemons'
   Services and the router's. The daemon requires a client certificate
   from the installation's CA.
4. **The build runs in the daemon:**
   - `buildctl` streams the build context and Dockerfile from the job pod;
   - the daemon resolves and pulls base images;
   - it runs `RUN` steps in runc sandboxes;
   - it exports the result.

   Registry credentials stay in the job: the daemon asks `buildctl` for
   them over the session when it pulls or pushes.
5. **The job only waits.** It uses almost no CPU; that is why job pods get
   a small resource tier.

`RUN` steps run in the daemon pod's network namespace (BuildKit's host
network mode). They see the daemon's localhost and leave the pod with the
pod's IP, so the daemon's NetworkPolicy also limits what builds can reach
([ADR-0019](adr/0019-network-policy.md)).

### Which daemon served a job

Each daemon names itself in two ways:

- **From the job:**

  ```sh
  buildctl --addr "$BUILDKIT_REMOTE_ADDR" --tlsdir "$BUILDKIT_TLS_DIR" debug workers -v | grep hostname
  ```

  prints `org.mobyproject.buildkit.worker.hostname: <daemon pod>`.
- **In metrics and traces** (when they are sent to a backend): the labels
  `k8s.pod.name` and `k8s.node.name`.

## The cache

- **Per daemon.** Each daemon has its own cache; nothing is shared between
  nodes. The router keeps a project on the node where it built last, so its
  cache warms up once; a job sent elsewhere (that daemon not ready, another
  zone) builds with the other daemon's cache. Warmth is kept by node: a
  daemon's restart on hostPath keeps its projects.
- **Shared between projects.** Identical steps of different projects reuse
  each other's layers, and cache mounts are shared
  ([ADR-0046](adr/0046-cache-reuse.md)).
- **Whose it is: not known.** BuildKit's disk usage lists records without
  a build or project, so the console shows each node's cache by kind
  (base images, build layers, cache mounts, contexts), not by project.
- **Persistence:**
  - **`hostPath`** (production, on a dedicated disk): the cache survives
    restarts, rollouts and uninstall.
  - **`emptyDir`** (the default, for trials and CI): the cache dies with the
    pod, and every rollout empties it.
- **Deduplication.** Identical steps of concurrent builds on the same daemon
  run once, so parallel jobs of one pipeline share their common steps.
- **Garbage collection** runs after builds (at most once a minute) and at
  start, not on a timer. The chart always renders the full policy set
  ([ADR-0009](adr/0009-gc-policies.md)):

  | Policy | What it removes |
  |---|---|
  | Build contexts and git checkouts | Records unused for 48 h |
  | Cache mounts (`RUN --mount=type=cache`) | Records unused for 14 days (`config.gc.cacheMounts.keepDuration`) |
  | Everything else | Records unused for 60 days; and while the cache is above `maxUsedSpace` or free disk is below `minFreeSpace`, the least recently used records, down to the caps but never below `reservedSpace` |

  - A size cap on a filtered policy is compared with the size of the whole
    cache, not with the filtered records.
  - Sizes use BuildKit's binary units: `10GB` means 10 GiB.

## Identity and certificates

- **The CA.** With cert-manager, the release creates a CA of its own unless
  you name an issuer. That CA is a self-signed issuer, a CA certificate
  that keeps its key on renewal, and a ClusterIssuer.
- **Leaf certificates.** The daemons' certificate and one client
  certificate per jobs namespace last a year and are renewed after two
  thirds.
- **Renewals need a restart.** buildkitd reads its certificate only at
  start. The DaemonSet carries a Stakater Reloader annotation, so with
  Reloader in the cluster a renewed Secret triggers a rolling restart.
  Without it, restart the daemons before the old certificate expires (you
  have about four months after a renewal).
- **What the daemon accepts.** It accepts any client certificate from its
  CA and checks nothing else about the client. The CA must therefore be
  dedicated to the installation.

## Telemetry

- **Pushed, not scraped.** The daemons push metrics and traces over OTLP
  (http/protobuf by default) to your own backend, when
  `buildkit.otel.metricsEndpoint` and `tracesEndpoint` are set. Nothing is
  stored in the box, and nothing is sent by default. The daemon's debug
  HTTP server (`/metrics` next to pprof and cache endpoints) is never
  enabled.
- **Metrics.** BuildKit currently exports three:
  - `buildkit.builds` (by status);
  - `buildkit.builds.steps` (by kind: completed, cached, total, warnings);
  - `buildkit.build.duration`.

  The counters are deltas per push interval: whatever stores them must sum
  them over time, or convert them to cumulative counters first. Cache size
  and GC activity are not exported by BuildKit yet.
- **Traces.** The runner configuration has a `pre_build_script` that runs in
  every job right before its script and exports:
  - `TRACEPARENT`, one trace per job:
    - trace-id = `01` (scheme version), 6 hex characters derived from the
      GitLab URL, the pipeline id and the job id (12 hex characters each);
    - parent span-id = the job id in hex.

    Ids that are not numbers from 1 to 2^48-1 set nothing. A `TRACEPARENT`
    the job sets itself is kept.
  - `OTEL_RESOURCE_ATTRIBUTES` with the pipeline id, job id and project path.

  `buildctl` reads them and forwards its spans through the daemon, with no
  exporter in the job and no change to `.gitlab-ci.yml`. A job's trace is
  found from its ids; a pipeline's jobs by the `ci.pipeline.id` attribute.
  One trace per job also keeps BuildKit's build history clean: it attaches
  every span of a build's trace id to the build's record, so a trace shared
  by concurrent jobs would end up in each of their records
  ([ADR-0028](adr/0028-one-trace-per-job.md)). The console uses the trace
  id to tie each build record to its pipeline and job; that needs no trace
  storage. The job itself is not a span yet: in a trace viewer the builds
  hang under a missing parent. Job pods also carry annotations under
  `ci-accelerator.io/` with job data from GitLab's CI variables: pipeline
  id, source and creation time, project path, job stage and start time,
  commit title and author, merge request.
  The runner adds the job id, name, URL, ref, commit and project id itself.
  An unset variable gives an empty value; a commit title reaches the pod as
  text, never expanded.

## The console

The console is served by the control plane (chart `control-plane`,
`control-plane.enabled`, on by default), which runs in the box namespace as
roles of one image, `ci-accelerator`: the controller, the web and the build
router. Besides routing, it only reads; the one change it makes is removing
a project's cache from a node when an admin asks.

- **The controller** (Deployment `<release>-controller`, one pod,
  `Recreate`) owns everything with state: the store, the reading of the
  daemons and the job pods, the sessions of people signed in and every
  GitLab token.
- **The web** (Deployment `<release>-web`, two pods by default) serves the
  console's pages and JSON API on the Service `<release>-web`. It keeps
  nothing: it reads the controller's API over HTTPS (Service
  `<release>-controller`, port 8443), so it scales and rolls without a
  pause, and a controller restart shows as an error page for a moment, not
  a lost session. The writes the pages cause (GitLab's answers, settled
  reasons, sessions) are done by the controller.
- **Between them.** The controller's API certificate comes from the daemons'
  CA with server authentication only: the daemons accept client
  certificates, so it can never drive them. The web proves who it is with a
  ServiceAccount token that Kubernetes projects into its pods for the
  controller's audience; the controller checks it with a TokenReview (its
  only cluster-wide right). The routers prove themselves the same way and
  may only read their view and report decisions. Its NetworkPolicy admits
  the web's and the routers' pods alone: CI jobs and RUN steps cannot
  reach the API.

What the controller does:

- **Build history from the daemons.** It finds the daemons through the
  headless Service and reads each one's build history over the daemons'
  TLS port (1234), with a client certificate of its own from the daemons'
  CA. The daemons' NetworkPolicy has an ingress rule that admits pods
  labelled `ci-accelerator.io/component: controller` (never the web's).
- **Disk space from the disk probes.** BuildKit reports what the cache
  holds, not how much room its disk has left. Once a minute, with each
  daemon's inspection, the controller asks the probe next to it (plain HTTP on
  the daemon pod's address, on the port the daemon states in a worker
  label, 9180 by default) for the size, free
  space and inodes of the cache's filesystem. The daemons' NetworkPolicy
  opens that port to the same peers as the TLS port. The Nodes page shows
  it against BuildKit's keep-free floor and the kubelet's eviction
  thresholds.
- **Job data from the job pods.** A Role in the jobs namespace lets it get,
  list and watch pods. The pods' annotations tie each build to its
  pipeline, job, project and commit; the trace id ties the build record to
  the job.
- **GitLab's API, outbound and on demand.** With a token or with sign-in,
  it reads a pipeline's jobs while someone looks at pipelines, compares two
  commits to settle "files or node", and reads the end of a job's log when
  someone asks for it. It waits when GitLab rate-limits. GitLab never needs
  to reach the console: there are no webhooks, and with sign-in the browser
  carries the redirects.
- **Its store** is SQLite on a ReadWriteOnce volume (the controller: one
  replica, `Recreate`). Builds are kept in full for 30 days (`retention.days`);
  each image's daily figures (builds by outcome, build times) for a year;
  step output for 7 days. The volume is kept when the release is removed.

## Rollouts and restarts

**What restarts the daemons.** The pod template changes, so every daemon
restarts, when:
- the rendered `buildkitd.toml` changes (the template carries its checksum);
- the image changes;
- environment values change (for example `extraEnv`, OTLP endpoints);
- the TLS Secret name changes;
- the PriorityClass, the resources or the disk probe (on, off, its image)
  change.

Upgrading the chart without changing any of these does not restart
anything ([ADR-0013](adr/0013-stable-pod-template.md)).

**How a rollout goes:**
- one node at a time (`maxUnavailable: 1`, never a surge with hostPath,
  since the state directory is locked);
- each new daemon must stay ready for 10 s before the next node.

**How a daemon stops:**
1. `preStop` keeps it serving for 5 s while the endpoint is withdrawn;
2. on SIGTERM it stops accepting connections and lets running builds
   finish, up to `terminationGracePeriodSeconds` (300 s by default).

New jobs on that node fall back to another daemon in the meantime.

**Duration.** A rollout takes up to *nodes × (longest build + daemon
startup)*. Pass a matching `--timeout` to Helm. For production builds of
10-20 minutes, set the grace period to 1200-1800 s.

**A bad configuration.** If it parses but crashes buildkitd, it stops the
rollout on the first node; the other daemons keep serving. Roll back with
`helm rollback`.

## Install-time checks

The chart refuses installs that would end with nothing working. Checks that
need the live cluster are skipped when rendering offline (Argo CD, Flux,
`helm template`).

| Check | Fails when |
|---|---|
| Pod Security | The box namespace enforces a level that forbids privileged pods |
| Build nodes | No node carries every label of the build-node selector |
| cert-manager | Its API is missing, with the command that installs it |
| TLS Secret (existing-Secret mode) | It exists but lacks `tls.crt`, `tls.key` or `ca.crt` |
| Runner token Secret | It exists but lacks a key, or the token is not a token |
| Values | GC policies that would delete the whole cache, unsafe storage combinations, invalid TOML, names the subcharts must agree on |
