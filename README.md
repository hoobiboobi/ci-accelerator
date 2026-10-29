# ci-accelerator

A self-hosted fast CI runner for GitLab, on your own Kubernetes cluster.

You give it a GitLab URL and a runner token. You get a runner whose jobs
build container images on persistent BuildKit daemons with a warm, shared
cache, a build router that sends each project's builds to the node where
its cache is, and a build console that tells why a build was slow or
missed the cache. The daemons can also push their metrics and traces over
OTLP to a backend you already run. Hosted services such as Depot,
Namespace or Docker Build Cloud do this for you; this project does it on
hardware you already run.

> **Status:** v0.3.3 is released: signed charts and the ci-accelerator image on
> ghcr.io, installed as in the [Quickstart](#quickstart). Documentation of
> the released version: [ci-accelerator.io](https://ci-accelerator.io/docs/).

## Why

Each CI job usually builds in a fresh container. The layer cache is empty
or has to be pulled, and every job repeats work that the previous one just
did. ci-accelerator keeps the build engine running between jobs instead:

- **The cache stays warm.** BuildKit daemons run on dedicated build nodes
  and keep their cache on local disk across jobs, pipelines and projects.
- **Jobs are thin clients.** A job sends its build context to a daemon and
  waits. The daemon builds and pushes the image.
- **Each project builds where its cache is.** A router sends every job to
  the build node where its project built before, so a project's cache
  warms up once, not once per node. A new project's parallel jobs meet on
  one daemon.
- **Identical work runs once.** BuildKit deduplicates identical steps
  across concurrent builds. Ten parallel jobs of one pipeline that share a
  `yarn install` step run it once.

Numbers from the production setup this project is derived from:

| | Before | With persistent daemons |
|---|---|---|
| Monorepo service image | 16-21 min | ~7 min warm |
| Image export | 130 s | 4 s |
| CPU used by the job pod | — | ~0.03 vCPU |

## What gets installed

`charts/ci-accelerator` (the "box") installs:

```
 GitLab ──────► gitlab-runner ─────────► CI job pod (jobs namespace, on a build node)
                                              │  buildctl, mutual TLS (passed through)
                                              ▼
                     build router (every build node) ◄── where each project built (controller)
                                              │  picks the node where the project is warm
                                              ▼
                     buildkit daemon ── local cache
                                              │
                                              ├── build history ──► controller ──► web: the console
                                              └── OTLP ───────────► your backend: metrics, traces (optional)
```

- **BuildKit daemons:** a DaemonSet on the nodes labelled
  `ci-accelerator.io/node-role=build` (`charts/buildkit`, BuildKit
  v0.34.1, pinned by digest), each with a small disk probe next to it
  that reports the cache disk's size and free space, and the node's and
  the daemon's CPU and memory, for the console.
- **The control plane** (`charts/control-plane`): one image,
  `ghcr.io/hoobiboobi/ci-accelerator`, in three roles:
  - the **build router**, a DaemonSet on every build node behind the
    Service `<release>-router`. Jobs connect to it, and it sends each job
    to the daemon on the node where its project built in the last week;
    a project that has not built in the job's zone yet goes to its daemon
    by a hash every router agrees on, so the parallel jobs of a pipeline
    share one daemon's steps. It passes the jobs' TLS through and holds no
    key ([ADR-0045](docs/adr/0045-build-router.md));
  - the **controller**, one pod (Deployment `<release>-controller`): it
    reads every daemon's build history and the CI job pods into its own
    store (SQLite on a volume), holds the sign-in sessions and every
    GitLab token, and tells the routers where each project built;
  - the **web**, any number of pods (Deployment and Service
    `<release>-web`, two pods by default): the console's pages. It keeps
    nothing and reads the controller's API over HTTPS
    ([ADR-0044](docs/adr/0044-control-plane.md)).
- **The official gitlab-runner chart,** configured so that job pods:
  - run on the build nodes, in their own namespace;
  - get `BUILDKIT_REMOTE_ADDR` (the router's Service) and a client
    certificate (`BUILDKIT_TLS_DIR`);
  - use a small resource tier: the work happens in the daemon.
- **Mutual TLS** between jobs and daemons, issued by cert-manager with a CA
  dedicated to this installation. Without it, any build could drive its own
  daemon.
- **NetworkPolicies:** the daemons accept connections only from the
  routers and the controller, the routers only from the jobs namespace,
  the controller only from the web and the routers, and the web from
  neither the jobs namespace nor the daemons. Builds cannot reach cloud
  instance metadata.
- **Metrics and traces over OTLP (optional):** the daemons push them to
  your own backend (`buildkit.otel.*`). The box stores none of it; build
  times and cache answers are in the console.
- **Runner cache and tiers:** GitLab's `cache:` in any S3-compatible
  bucket, and more runners (tiers) with their own tags, job size,
  placement and cache, each its own pod
  ([docs/runner-cache.md](docs/runner-cache.md)).

Each part is usable on its own:

- **Only the daemons:** `charts/buildkit`
  (`oci://ghcr.io/hoobiboobi/charts/buildkit`).
- **The control plane for daemons installed with `charts/buildkit`:**
  `charts/control-plane` (`oci://ghcr.io/hoobiboobi/charts/control-plane`).
- **An existing gitlab-runner:** the values fragment in
  [integrations/gitlab-runner](integrations/gitlab-runner/README.md).

### The console

The console is on by default. It tells why an image build was slow or not
cached, and whether your change or the build farm caused it. Leaving it
out takes the router with it: set `control-plane.enabled=false` and
`global.router.enabled=false`, and the jobs connect to the daemons'
Service (their own node's daemon). Five sections:

- **Overview:** why images were rebuilt, the reasons ranked by the build
  time they cost, with the build farm's share of the rebuild time and,
  apart, the unclear share.
- **Pipelines:** each pipeline's jobs by stage on a time axis, with how
  long each waited for a runner and ran, searchable by pipeline number,
  commit, title, branch, author or job name within a period (24 hours, 7
  or 30 days, or dates). A pipeline opens into a waterfall of every job,
  and its image builds into stages and steps.
- **Builds:** every image build, filtered by branch, outcome, image or
  your own, and grouped by pipeline if you like.
- **Images:** cache rate and build time per image over 24 hours or 7
  days; one image's page goes back 24 hours, 7 days, 30 days or a year.
- **Nodes:** one table of the daemons, the ones that need attention first
  (not reachable, a disk warning, a cache that could not be read; a filter
  shows only those), with disk, cache, builds, rebuilds for want of cache,
  running builds, projects and the build router's decisions, and each
  node's CPU and memory over the last hour next to the latest reading
  (with a "waiting for CPU" chip when the kernel says some task waited
  for a core); one note per problem, naming its nodes; where the project
  you are looking at built in the last 24 hours; base images held by more
  than one node; and the runners' S3 cache per project, with a warning
  when the bucket has no lifecycle rule. Each daemon has its own page: its
  builds, running builds, the node's cores and memory split into the
  daemon's pod, the rest of the node and what is free (with the last hour
  drawn and the pressure stall information, where the kernel reports it),
  cache by kind and largest cache mounts, its disk (cache, other files,
  free space) with a warning near the GC's keep-free floor or the
  kubelet's eviction thresholds, why the router sent builds there, its
  garbage collection and the projects it built for. The node's and the
  pod's figures come from the disk probe next to each daemon, which reads
  `/proc` and the pod's cgroup every 10 s and keeps an hour; longer
  series belong to your monitoring (OTLP, below).

Every build gets one of five outcomes: from cache, your change, build
farm, unclear or failed. A build's page starts with one sentence, then:

- what changed since the previous build of the image: commit, node,
  Dockerfile lines, build arguments, base image, the files a rebuilt step
  copies (a build argument that reaches a step through `ENV` counts as
  that argument's change);
- its steps, with what each one's processes used (CPU time, time held
  back by a CPU limit, disk read and written, the most memory its cgroup
  held in one of BuildKit's samples, from BuildKit's cgroup readings) and
  how much step time ran in parallel; a Resources tab lists the executed
  steps by CPU time with the cores each used on average, and draws the
  cores the running steps used together over the build against the
  node's cores, saying for how long they wanted more than the node has;
- BuildKit's Dockerfile build checks that failed (rule, line, link);
- for a failed build, whether the step that failed is new or changed;
  exit codes 137 and 143 are read as the signals they are;
- where the job's time went before the build, from the job pod's own
  timestamps: the runner preparing the pod, the pod starting, the runner
  attaching, checkout and script;
- why the build router chose its node. A rebuild on the same node under a
  replaced daemon says so, and whether its cache survived.

**Compare** any two builds of an image side by side
(`/builds/<id>/compare`): time, commit, node, build arguments, the
Dockerfiles' differing lines and every step's state and time. The Compare
button on a build's page (key `x`), "Compare two builds" on the builds
list, "compare two" on an image's page and Ctrl+K lead there.

Every figure is measured, not estimated. Medians and p95 are nearest-rank
percentiles, so always one of the measured times; "usual" needs five
builds of its kind and a p95 twenty rebuilds (a dash until then). The
time a cached build saved is how long the steps it did not run took when
they last ran, and sizes are in binary units (KiB, MiB, GiB). Every
job's log can link to its page in the console (`global.console.url`). Jobs that
build no image stay in GitLab. With sign-in through GitLab
(`control-plane.signIn`), each person sees the projects whose jobs GitLab
lets them read; without it, keep the console inside the cluster (`kubectl
port-forward`). [Setting up the console](#setting-up-the-console) has the
three levels.

## Quickstart

### Try it on one machine

To look at it before a real deployment, one clean Linux machine is enough:
Ubuntu 24.04 or Debian 13, amd64 or arm64, 4 CPUs, 8 GB of memory and 40
GB of free disk, without Docker or Kubernetes. First create a runner in
GitLab and copy its token (`glrt-...`): the script asks for it, or reads
it from a file (`--runner-token-file`), so it never sits on a command line
where other users and the shell history see it. Then run:

```sh
curl -fsSLO https://github.com/hoobiboobi/ci-accelerator/releases/download/v0.3.3/quickstart.sh
curl -fsSL https://github.com/hoobiboobi/ci-accelerator/releases/download/v0.3.3/SHA256SUMS | grep quickstart.sh | sha256sum -c -
sudo bash quickstart.sh --gitlab-url https://gitlab.example.com/
```

`quickstart.sh` installs the following; k3s, Helm, cert-manager and the
chart are pinned and checked against their sha256 (the box's images come
by tag, the daemons' by digest):
- k3s v1.36.5+k3s1, with the machine as its only node and build node;
- Helm v4.3.1 and cert-manager v1.21.3;
- the box, with the build router and the console. The daemon keeps its
  cache in `/var/lib/ci-accelerator/buildkit` (a hostPath, so it survives
  restarts; `--build-dir` puts it elsewhere). Its resources are sized from
  the machine on the first install (half its CPUs and a quarter of its
  memory requested, three quarters of the memory at most) and kept in
  `/var/lib/ci-accelerator/values.yaml`, which later runs leave alone.

It prints a job to add to `.gitlab-ci.yml` and the SSH tunnel to the
console. The console starts in its first level (builds only, no GitLab
access; see [Setting up the console](#setting-up-the-console)):
`--gitlab-token-file` adds job results and runner waits, and sign-in
through GitLab works on this box too. A machine that runs 0.2.x is not
upgraded in place: the script stops and says what to do (see
[Upgrading](#upgrading)). `sudo bash quickstart.sh --uninstall` removes
everything again. It is not meant for production: one node, no high
availability.

### Requirements

- **Kubernetes 1.34 or newer.** Same-node routing (`PreferSameNode`) needs
  kube-proxy 1.34+ or a proxy that implements it.
- **Build nodes:** one or more, labelled and tainted. The daemons, the
  build routers and the job pods run only there. The runner manager and
  the console's controller and web run on the other nodes: keep at least
  one node without the taint (or see
  [troubleshooting](docs/troubleshooting.md#the-runner) for tolerations).
  Nodes in several zones (`topology.kubernetes.io/zone`) keep each job's
  builds in its zone; see [Limitations](#limitations).

  ```sh
  kubectl label node <node> ci-accelerator.io/node-role=build
  kubectl taint node <node> ci-accelerator.io/node-role=build:NoSchedule
  ```

- **cert-manager:**

  ```sh
  helm install cert-manager oci://quay.io/jetstack/charts/cert-manager --version v1.21.3 \
    --namespace cert-manager --create-namespace --set crds.enabled=true
  ```

- **A default StorageClass,** for the controller's volume (or set
  `control-plane.controller.persistence.storageClass` or
  `control-plane.controller.persistence.existingClaim`, or leave the
  control plane out with `control-plane.enabled=false` and
  `global.router.enabled=false`).
- **A CNI that enforces NetworkPolicy** (Calico, Cilium, kindnet, ...).
  Without one, the daemons accept connections from any namespace and builds
  can reach cloud instance metadata; mutual TLS still applies.
- **Permission to create a PriorityClass** (cluster-scoped): the daemons get
  their own, so under node pressure the kubelet evicts CI jobs before them.
  Without that permission set `buildkit.priorityClass.create=false`, or name
  an existing class in `buildkit.priorityClassName`.
- **Permission to create a ClusterRole and its binding** for the control
  plane: the controller checks the ServiceAccount tokens of the web and
  the routers with a TokenReview, its only cluster-wide right.
- **Daemon resources:** the defaults (500m CPU, 512Mi requested) only keep
  the daemons out of BestEffort; size `buildkit.resources` to your build
  nodes (the chart's values list sizes).
- **Helm 3 (3.19+) or Helm 4.**

### 1. A namespace for the box

The daemons are privileged pods, so the namespace must allow them:

```sh
kubectl create namespace ci-accelerator
kubectl label namespace ci-accelerator pod-security.kubernetes.io/enforce=privileged
```

### 2. A runner token

In GitLab, open your project or group → **Settings → CI/CD → Runners → New
runner**. Choose tags, or allow untagged jobs. Put the token (`glrt-...`)
into a Secret. The chart reads both keys, so the empty one must be there
too:

```sh
kubectl -n ci-accelerator create secret generic gitlab-runner-token \
  --from-literal=runner-token=glrt-... --from-literal=runner-registration-token=
```

### 3. Install

```sh
helm install ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.3.3 \
  -n ci-accelerator \
  --set gitlab-runner.gitlabUrl=https://gitlab.example.com/ \
  --set gitlab-runner.runners.secret=gitlab-runner-token \
  --wait --timeout 10m
```

The package contains its subcharts. From a clone instead: `make deps`, then
`helm install ci-accelerator charts/ci-accelerator ...` with the same
values.

- **Defaults:** amd64 build nodes, jobs in namespace `ci-accelerator-jobs`
  (created by the release), the build cache in an `emptyDir` (see below).
- **arm64 build nodes:** add `--set global.jobs.architecture=arm64`.
- **Production:** pin the ci-accelerator images by digest, both listed in
  the release notes: the control plane's with
  `--set control-plane.image.digest=sha256:...`, and the disk probe's next
  to each daemon (it runs the image of the release that last changed it)
  with `--set buildkit.diskProbe.image.digest=sha256:...`. The daemons'
  BuildKit image is pinned by digest already. A digest belongs to one
  release: upgrades set the new one (see [Upgrading](#upgrading)).
- **Without cert-manager:** `buildkit.tls.source=existingSecret` with four
  certificates from one CA that `scripts/buildkit-certs.sh` makes (the
  daemons' with the router's names, the jobs' client, the controller's
  client and the controller API's server certificate); the jobs namespace
  is then yours to create, with the jobs' client Secret in it
  ([docs/gitops.md](docs/gitops.md#certificates-without-cert-manager)).
- **GitOps:** [docs/gitops.md](docs/gitops.md) shows the same install as
  a Flux `HelmRelease` and an Argo CD `Application`.

The notes printed at the end show how to check that the runner registered.

### Where the cache lives

By default the daemons keep their cache in an `emptyDir` on the node's root
disk: fine for a first look, but every rollout starts with an empty cache
and the node's disk is shared with everything else. For production give
each build node a dedicated disk and point the daemons at it:

```yaml
buildkit:
  storage:
    type: hostPath
    hostPath:
      path: /var/mnt/nvme/buildkit   # a directory on the mounted disk, created in advance
  config:
    gc:                              # absolute sizes on a dedicated disk (binary units: 1TB = 1 TiB)
      maxUsedSpace: "2TB"
      reservedSpace: "100GB"
      minFreeSpace: "400GB"
```

- **The directory must exist** on the mounted disk (`type: Directory`, the
  default): if the disk is not mounted, the daemon does not start instead
  of filling the OS disk. One release per path per node; the cache stays
  after `helm uninstall`.
- **XFS.** The cache is millions of small files. ext4 fixes its number of
  inodes at `mkfs` time and can run out of them (`df -i`) long before it
  runs out of space; XFS allocates inodes as needed. ext4 also keeps 5% in
  reserve for root, which buildkitd's free-space check counts and kubelet's
  eviction does not; XFS has no such reserve, so `minFreeSpace` means what
  `df` shows. XFS is what the production numbers above come from:

  ```sh
  mkfs.xfs -L buildkit /dev/nvme1n1            # ftype=1 (overlayfs needs it) is the default
  mkdir -p /var/mnt/nvme && mount -o noatime /dev/nvme1n1 /var/mnt/nvme
  mkdir -p /var/mnt/nvme/buildkit              # the hostPath
  ```

  With ext4, make more inodes (`mkfs.ext4 -i 8192`) and drop the reserve
  (`tune2fs -m 0`).
- **Sizing** and the reasons for headroom: [docs/troubleshooting.md](docs/troubleshooting.md#disk-inodes-and-processes).

### 4. A first build

In a project with a `Dockerfile`, include the GitLab CI component: one job
that builds on the build farm and pushes `$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA`
with the job token. The release notes print this block with the file's
hash:

```yaml
# .gitlab-ci.yml
include:
  - remote: https://raw.githubusercontent.com/hoobiboobi/ci-accelerator/v0.3.3/ci/gitlab/build.yml
    integrity: sha256-<from the release notes>
    inputs:
      runner-tags: [my-runner-tag]   # if the runner only takes tagged jobs
```

Build arguments, secrets, several images, another Dockerfile, the digest
for later jobs and GitLab without access to GitHub:
[ci/gitlab/README.md](ci/gitlab/README.md).

Add `.git` to `.dockerignore`. Every job has a fresh clone, so with `.git`
in the build context, every step after `COPY . .` runs again in every
pipeline. More in [docs/dockerfile-guide.md](docs/dockerfile-guide.md).

Without the component, call buildctl yourself: `buildctl --addr
"$BUILDKIT_REMOTE_ADDR" --tlsdir "$BUILDKIT_TLS_DIR" build ...` in the
`moby/buildkit` image. To push, write the registry credentials to
`$DOCKER_CONFIG/config.json` and run `unset DOCKER_AUTH_CONFIG` first:
buildctl reads that variable too, and a read-only entry in it overrides the
job's credentials.

### Setting up the console

The console is on by default. The control plane serves it: a controller
(Deployment `<release>-controller`, one pod: the store, the sessions,
every GitLab token) and a web (Deployment and Service `<release>-web`, two
pods serving the pages), which reads the controller's API over HTTPS and
proves who it is with a ServiceAccount token; see
[the architecture](docs/architecture.md#the-control-plane-and-the-console).
It works in three levels; each adds to the previous one, and the install
notes say which is on.

| | by itself (default) | with a GitLab token | with sign-in |
|---|---|---|---|
| image builds, steps, cache verdicts, what changed, pipeline timeline | yes | yes | yes |
| job results, how long jobs waited for a runner, "N other jobs" | no | yes | yes |
| "files or node" settled by comparing the two commits, end of a job's log | no | yes, in the background too | yes, while pages are open |
| why the build router chose a build's node | yes | yes | yes |
| who sees what | everyone who reaches it | everyone who reaches it | each person: the projects GitLab lets them read |
| the console reads GitLab with | nothing | one shared token | each person's own token |

**1. By itself.** The console reads the daemons' build history and the CI
job pods, nothing else: every image build with its steps, the verdict
(from cache, your change, build farm, unclear, failed) and the pipeline's
timeline are there; a job's result is "unknown", runner waits are not
shown, and a rebuild whose cache sat on another node stays "unclear". It
has a ClusterIP Service and no sign-in, so whoever reaches it sees
everything: keep it inside the cluster and reach it with
`kubectl port-forward` (the install notes print the command; the
one-machine install prints its SSH tunnel).

**2. With a GitLab token.** One token with `read_api` for all viewers.
The controller reads with it each pipeline's jobs while pages show it,
asks GitLab to compare the commits of a rebuild whose cache was on
another node (in the background, at most 25 requests a minute per
project; GitLab's answers are stored, so they settle the build again
after a restart) and reads the end of a job's log on request. Put it
into a Secret and name it in the values:

```sh
kubectl -n ci-accelerator create secret generic gitlab-api-token --from-literal=token=glpat-...
```

```yaml
global:
  console:
    url: https://ci-console.example.com   # optional: every job's log links to its page
control-plane:
  gitlab:
    token:
      existingSecret: gitlab-api-token
```

On the one-machine install, run the script again with
`--gitlab-token-file ./token` (same GitLab URL and runner token; it only
updates the release).

**3. With sign-in through GitLab.** Replaces the shared token: each person
signs in with their GitLab account, the console reads GitLab with their
own token and shows them the projects whose jobs GitLab lets them read;
`admins` see everything. This is the mode for a console that a team
reaches over https.

1. Decide the address. Serve the console over https at the root of its
   host (no sub-path) and set `global.console.url` to it; plain `http://`
   is accepted only on `localhost`, i.e. through a port-forward or the
   one-machine install's SSH tunnel (`http://localhost:8080`).
2. In GitLab, create an OAuth application (User or Group Settings >
   Applications; Admin > Applications on your own GitLab): confidential,
   scopes `openid` and `read_api`, redirect URI
   `<global.console.url>/auth/callback/<id>`, where `<id>` identifies the
   GitLab instance (the first six hex digits of the sha256 of its URL;
   the install notes print the exact URI). Put the application's ID and
   secret, as GitLab shows them, into a Secret:

   ```sh
   kubectl -n ci-accelerator create secret generic console-gitlab-oauth \
     --from-literal=client-id=... --from-literal=client-secret=gloas-...
   ```

3. Create a session key; it seals each person's GitLab token at rest, and
   changing it signs everyone out (with Flux or Argo CD, see
   [docs/gitops.md](docs/gitops.md#the-session-key)):

   ```sh
   kubectl -n ci-accelerator create secret generic console-session \
     --from-literal=key=$(openssl rand -base64 32)
   ```

4. Set the values and upgrade (drop `control-plane.gitlab.token` if it
   was set: a shared token and sign-in exclude each other):

   ```yaml
   control-plane:
     signIn:
       enabled: true
       sessionKey: { existingSecret: console-session }
       gitlab:
         - existingSecret: console-gitlab-oauth   # client-id, client-secret
           admins: [alice]                        # GitLab usernames who see everything
   ```

   A `gitlab` entry without `url` is the runner's GitLab; several entries
   serve several GitLabs. On the one-machine install the values live in
   `/var/lib/ci-accelerator/values.yaml`; apply them with the Helm the
   script installed:

   ```sh
   sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/lib/ci-accelerator/helm-v4.3.1/helm upgrade ci-accelerator \
     oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.3.3 \
     -n ci-accelerator -f /var/lib/ci-accelerator/values.yaml --wait
   ```

Every key is in [charts/control-plane/values.yaml](charts/control-plane/values.yaml).

### Logs

Every role of the control plane (controller, web, router, disk probe)
writes one JSON object per line with its `role`: when it starts (with the
settings it runs with), is ready, stops and has stopped, and a line per
stored build, job start and end, change of the daemons, sign-in and
sign-out and routed connection. To see where the routers sent jobs (one
router pod per build node, each logs the jobs on its node):

```sh
kubectl -n ci-accelerator logs -l app.kubernetes.io/component=router --prefix --tail=-1 | grep routed
```

With `-l`, kubectl prints only the last 10 lines of each pod unless
`--tail=-1` is given.

`control-plane.log.level=debug` (or one role's
`control-plane.<controller|web|router>.logLevel`) adds every request,
GitLab call, controller API call and closed router connection;
`control-plane.log.format=text` is easier to read by eye. BuildKit's own
log is `buildkit.config.log.level`/`format` and the disk probe's
`buildkit.diskProbe.logLevel`; both are empty by default, and setting
them restarts every daemon once. No log carries a token, a cookie, a query
string, a build argument or step output, and anything shaped like a
GitLab token is masked. Which command reads which pod:
[troubleshooting](docs/troubleshooting.md#reading-the-logs).

### Sending metrics and traces to your backend

The box keeps no metrics or traces. To keep them, point the daemons at your
own OTLP backend, such as an OpenTelemetry Collector. The endpoints are
full URLs, one per signal:

```yaml
buildkit:
  otel:
    metricsEndpoint: http://otel-collector.observability.svc:4318/v1/metrics
    tracesEndpoint: http://otel-collector.observability.svc:4318/v1/traces
    protocol: http/protobuf   # the default; or grpc
```

- **Empty endpoints** (the default) send nothing.
- **Every metric and span names its daemon** with `k8s.pod.name` and
  `k8s.node.name`.
- **BuildKit's counters are deltas** per push interval. Sum them over
  time, or convert them in your collector if your backend expects
  cumulative counters.

Every job on the runner gets its own trace id without changes to
`.gitlab-ci.yml`. The runner sets `TRACEPARENT` before the job's script.
The trace id is derived from the pipeline and job ids, and every span
carries `ci.pipeline.id`, `ci.job.id` and `ci.project.path`. The console
uses the trace id to tie each build to its pipeline and job, so this needs
no trace backend.

## Upgrading

Every release upgrades in place from the previous one, except 0.3.0 from
0.2.x (below). Read its **Upgrade notes** in [CHANGELOG.md](CHANGELOG.md)
first, then:

```sh
helm upgrade ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version <new version> \
  -n ci-accelerator --reset-then-reuse-values --wait --timeout 10m
```

`--reset-then-reuse-values` keeps your values and takes the new version's
defaults for everything else. With the images pinned by digest, add the new
release's digests from its notes (`--set control-plane.image.digest=...`,
and `buildkit.diskProbe.image.digest` when the notes name another probe
image): a kept digest of the release before would run that release under
the new version, so the charts refuse it and say which version they run.
The end-to-end tests upgrade the previous release this way before every
change to the charts or the code is merged
([ADR-0027](docs/adr/0027-upgrades-and-branches.md)).

**From 0.2.x to 0.3.0: install again.** 0.3.0 split the console into the
control plane (chart `control-plane`, image
`ghcr.io/hoobiboobi/ci-accelerator`, a controller, a web and the build
router) and gave every object and value of it a new name, so Helm refuses
to upgrade a 0.2.x release in place and says how to move:

1. Remove the release and wait until its pods are gone (the jobs
   namespace goes with it), then delete the daemons' certificate Secret:
   it outlives the release, and 0.3.0's must name the build router.

   ```sh
   helm uninstall ci-accelerator -n ci-accelerator --wait
   kubectl -n ci-accelerator wait --for=delete pod -l app.kubernetes.io/instance=ci-accelerator --timeout=10m
   kubectl -n ci-accelerator delete secret ci-accelerator-buildkit-tls
   ```

   Without cert-manager, make it again from the same CA with
   `scripts/buildkit-certs.sh --router <release>` (the runbook below has
   the whole command).
2. Move your values from `console:` to `control-plane:`: the controller's
   pod settings and volume under `control-plane.controller`, the web's
   under `control-plane.web`; a runner cache's `consoleSecretName` is now
   `controllerSecretName`. A runner of your own (the values fragment) sets
   `BUILDKIT_REMOTE_ADDR` to the router,
   `tcp://<release>-router.<namespace>.svc:1234`.
3. Install this release (without cert-manager, also the controller API's
   certificate: `control-plane.controller.api.tls.existingSecret`).
4. Point port-forwards, Ingresses and `global.console.url` at the Service
   `<release>-web` (it was `<release>-console`).

The daemons' cache on a hostPath stays. The new console starts with an
empty history; the old one stays on the volume `<release>-console-data`
until you delete it. The one-machine install stops with the same steps;
GitOps tools delete the release and create it again with the new values.
`charts/buildkit` installed on its own upgrades in place; its daemons
restart once. Step by step:
[troubleshooting](docs/troubleshooting.md#upgrading-from-02x-to-030).

**From 0.1.0:** the box no longer installs VictoriaMetrics and
VictoriaTraces. Helm removes them on upgrade, but their volumes stay until
you delete them
([troubleshooting](docs/troubleshooting.md#after-upgrading-from-010)).
Endpoint values reused from 0.1.0 with `--reuse-values` point at the
removed Services: use `--reset-then-reuse-values`, or set
`buildkit.otel.metricsEndpoint` and `tracesEndpoint` to `""` or to your
backend.

## Limitations

These are known, deliberate or not yet solved. Read them before you share
one installation between teams.

**Security model: one installation is one trust domain.** (Reporting a
vulnerability: [SECURITY.md](SECURITY.md).)
- The daemons run as **privileged** pods. Their namespace must allow
  privileged pods.
- **Builds share the cache:** RUN steps can read and write cache mounts
  that other projects' builds left in the same daemon.
- **Builds share the daemon's network.** RUN steps run in the daemon pod's
  network namespace, so they see its localhost and the connections of
  concurrent builds. Their egress is whatever the daemon's NetworkPolicy
  allows. List your cluster CIDRs in
  `buildkit.networkPolicy.egress.clusterCIDRs` to keep builds out of
  in-cluster services. Blocking the API server reliably depends on the CNI:
  kube-proxy sends Service traffic for `kubernetes.default` to the control
  plane's node addresses, and a node's own address is always reachable from
  its pods, so list the control-plane node addresses too.
- **The disk probe's numbers are readable by builds.** The box runs a small
  container next to each daemon that reports, on port 9180 for the
  controller, the cache disk's size and free space and the last hour of the
  node's CPU, memory and pressure and of the daemon pod's CPU and memory
  (every 10 s). Builds share the pod's network and can read those numbers
  too, so a build can see when the node was busy, including with other
  projects' builds. The probe reads the host's cgroup filesystem through a
  read-only mount; the daemon's own share needs cgroup v2 nodes.
- **Trace data is untrusted.** Any build (a RUN step) can write spans
  through BuildKit, so traces in your backend are input from builds. The
  buildctl command line, build-arg values included, is recorded in traces:
  **never pass secrets as build args**; use `--secret`.
- **Client certificates:** any certificate from the installation's CA is
  accepted. By default cert-manager lets namespace editors request
  certificates from any ClusterIssuer; on a shared cluster restrict that,
  e.g. with cert-manager approver-policy.
- **The router places builds; it does not authorize them.** It passes the
  bytes through and never reads a build, so the daemons' mutual TLS is
  still what lets a job in, and any job may build anything on the daemon
  it is sent to. Per-project cache mount namespaces or authorization
  would need the router to read the build requests
  ([ADR-0046](docs/adr/0046-cache-reuse.md)).
- **Job pods need Pod Security `baseline`.** The runner's job pods do not
  meet `restricted`.

**Cache and routing.**
- **The cache is per daemon.** The router sends a project's jobs to the
  node where it built in the last `control-plane.router.warmDays` (7) days,
  so each project warms one node. When that daemon is not ready, the job
  goes to another one: it succeeds, with a cold cache there. A project
  that has not built in the zone yet goes to a node by hash. The job's
  build context travels to the chosen node, which may not be its own.
- **Zones:** with `control-plane.router.zones` (the default) a job stays
  in its zone (`topology.kubernetes.io/zone` of the daemons' nodes), so a
  project warms up once per zone; when its zone has no ready daemon,
  `crossZone` (on by default) sends it to another zone, or refuses it.
  Clusters that put one zone label on every node, or zones close enough
  that crossing them costs nothing, set `zones=false`.
- **The router knows a job by its pod's address.** With a CNI that hides
  it (SNAT inside the cluster) every job goes to its own node's daemon, as
  without the router, after the router waited up to 3 s for a pod with
  that address to appear (a job's pod can connect before the router has
  seen it). While the controller is away, the routers keep routing on what
  it last told them.
- **emptyDir mode:** the cache lives only as long as the pod, so every
  rollout empties it, and the router no longer counts builds of a replaced
  daemon as warmth. Use `hostPath` on a dedicated disk in production.
- **What uninstall leaves behind:**
  - with hostPath: the cache on the nodes;
  - the controller's volume `<release>-controller-data` (the console's history), kept on purpose;
  - the certificate Secrets: the CA key in cert-manager's namespace, and
    the daemons' and the controller's certificates in the box namespace
    (`<release>-buildkit-tls` among them).

**Operations.**
- **Certificate renewal needs a restart.** buildkitd reads its certificate
  only at start: after a renewal (once a year by default) the daemons must
  restart. With Stakater Reloader in the cluster that happens by itself.
- **Probes miss some hangs.** They check that the daemon answers, not that
  builds make progress. Stuck registry pushes can hang all builds on one
  daemon without failing a probe (moby/buildkit#7275).
- **Upstream BuildKit issues that matter here:**
  - a cancelled build can fail another build that shares its steps
    (moby/buildkit#7268);
  - heap retained after builds (moby/buildkit#7181);
  - metadata that grows outside the GC budget (moby/buildkit#7263).
- **Kernel risk on Talos 6.18:** a very large cache with high parallelism
  has led to node hangs (dying cgroups). This is under investigation, not
  solved.

**The console.**
- **Without sign-in it is open.** Every page, `/search-index`, `/events`
  and `/api/v1` answer whoever reaches the Service `<release>-web`. The
  Service is ClusterIP only: use `kubectl port-forward`, or an Ingress with its own
  authentication. "Inside the cluster" is not a trust boundary here: the
  box's own CI jobs and builds run untrusted code in it. The box therefore
  ships a NetworkPolicy (`control-plane.networkPolicy`, on by default)
  that keeps the jobs namespace and the daemons (whose network every RUN
  step shares) away from the web, and admits only the web and the routers
  to the controller; other namespaces, an Ingress controller among them,
  still reach the web. It needs a CNI that enforces NetworkPolicy.
- **A configured token is shared.** Without sign-in, anyone who reaches
  the console can read the logs of the jobs it shows (masked, a part at a
  time) through the installation's GitLab token.
- **It stores build data.** Step output is kept 7 days, masked on a best
  effort basis: known token formats, passwords in URLs, bearer tokens and
  the values of build arguments with secret-like names. Dockerfiles are
  kept too. The API returns stored build arguments, with secret-named ones
  masked.
- **Job pods carry the commit author's name and email** as annotations,
  and the console stores them.
- **One controller.** SQLite on a ReadWriteOnce volume (never NFS), with
  the `Recreate` strategy: while the controller restarts (an upgrade that
  changes it), the pages answer that it is not reachable (502) and the
  routers route on what they last knew. There is no built-in backup.
- **No in-place upgrade from 0.2.x.** 0.3.0 is installed again, and its
  console starts with an empty history ([Upgrading](#upgrading)).
- **Without sign-in and without a token** the console shows no runner wait
  times and cannot compare commits, so "files or node" stays "unclear".
- **Images are told apart by Dockerfile target.** Builds without a target
  (the CI component's default) are named by the repository they push to,
  and each is compared with the previous build that pushed the same
  repository; but the Images page and the daily figures put all of a
  project's builds without a target in one row. Give each image its own
  target, or one project per image, to see them apart there.
- **A failed build's Dockerfile** comes from BuildKit's stored error: only
  the failed step's lines are marked (BuildKit records no provenance for a
  failed build), and a canceled build has none.

**Scope.**
- **GitLab only.** GitHub Actions support (through Actions Runner
  Controller) is planned.
- **The CI component is for GitLab only**; GitHub Actions is planned with
  the GitHub support above.

## Testing

Every check is a `make` target. The same targets are meant for CI.

```sh
make quick                    # before every push: lint, unittest and go-test
make lint unittest validate   # no cluster needed: helm lint, helm-unittest, kubeconform (k8s 1.34 and 1.37)
make go-test ui-test          # Go tests; the console's pages in headless Chrome
make kind-up                  # local kind cluster (examples/kind)
make e2e                      # end-to-end on kind
```

- **The e2e** installs releases of its own and covers:
  - cache hits and step deduplication;
  - routing: the Service's same-node routing, and the build router (every
    build node has one, a job's build page says why its node was chosen,
    a job cannot reach a daemon around the router, a router rollout lets
    a running build finish);
  - graceful shutdown and crash recovery;
  - GC limits;
  - mutual TLS, including a RUN step that tries to drive its own daemon;
  - the NetworkPolicy;
  - OTLP export;
  - the whole box, with the console;
  - an in-place upgrade from the last published release, and a rollback.

  On GitHub it runs in four parallel groups, each on its own kind cluster.
  A pull request runs only what its change touches: documentation alone
  runs nothing, Go code the Go checks and the two groups that run the
  control plane, a chart every group
  ([ADR-0049](docs/adr/0049-ci-runs-what-a-change-touches.md)).
- **The console's pages** are checked in headless Chrome: on test data by
  `make ui-test`, and against the real console in the e2e's box scenario.
- **The quickstart** is checked on a clean Ubuntu machine:
  `test/quickstart/run.sh` runs `quickstart.sh`, two CI jobs and the
  console, then uninstalls (`.github/workflows/quickstart.yml`).
- **CI jobs in the e2e** come from a real gitlab-runner talking to a
  stand-in for the GitLab API. The box has also been tested by hand against
  gitlab.com.
- **Mutation testing:** the checks are broken on purpose (the code they
  guard is changed or removed) to make sure each one can actually fail.
- **Helm versions:** the checks pass on Helm 3 and Helm 4.

## Repository

| Path | What |
|---|---|
| `CHANGELOG.md` | Changes and upgrade notes of every release |
| `CONTRIBUTING.md` | How to report issues and propose changes |
| `SECURITY.md` | How to report a vulnerability, and what is scanned |
| `docs/releasing.md` | How a release is cut and verified |
| `charts/ci-accelerator` | The box (umbrella chart) |
| `charts/buildkit` | The daemons, usable on their own |
| `integrations/gitlab-runner` | Values fragment for an existing gitlab-runner |
| `ci/gitlab` | GitLab CI component: a job that builds and pushes on the build farm |
| `charts/control-plane` | The control plane (controller, web and build router; the console), usable and published on its own (`oci://ghcr.io/hoobiboobi/charts/control-plane`) |
| `cmd/ci-accelerator`, `internal/` | Go: the binary and image `ghcr.io/hoobiboobi/ci-accelerator`, with the roles `controller`, `web`, `router`, `disk-probe` and `all` (build history to builds and reasons, store, collector, pages, routing) |
| `scripts/quickstart.sh` | One clean Linux machine: k3s, cert-manager and the box (attached to every release) |
| `scripts/buildkit-certs.sh` | CA and certificates for `charts/buildkit` without cert-manager (`tls.source=existingSecret`) |
| `docs/architecture.md` | How the parts fit together: build path, cache, certificates, telemetry, rollouts |
| `docs/troubleshooting.md` | Symptoms and fixes, runbooks |
| `docs/dockerfile-guide.md` | Writing Dockerfiles that stay cached |
| `docs/runner-cache.md` | The runners' S3 cache and runner tiers |
| `docs/gitops.md` | Installing with Flux or Argo CD |
| `docs/design/console.md` | The console's first design draft (historical) |
| `docs/adr` | Architecture decision records: why the charts are the way they are |
| `examples/kind` | Local cluster for development |
| `artifacthub/` | Artifact Hub metadata of the buildkit and ci-accelerator charts |
| `test/` | End-to-end checks and helpers |

## License

[Apache License 2.0](LICENSE)
