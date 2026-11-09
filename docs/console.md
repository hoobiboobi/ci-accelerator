# The console

The console is on by default. It tells why an image build was slow or not
cached, and whether your change or the build farm caused it. Leaving it
out takes the router with it: set `control-plane.enabled=false` and
`global.router.enabled=false`, and the jobs connect to the daemons'
Service (their own node's daemon). Five sections:

- **Pipelines** (the home page): each pipeline's jobs by stage on a time
  axis, with how long each waited for a runner and ran, searchable by
  pipeline number, commit, title, branch, author or job name within a
  period (24 hours, 7 or 30 days, or dates). A pipeline opens into a
  waterfall of every job, and its image builds into stages and steps.
- **Insights:** why images were rebuilt, the reasons ranked by the build
  time they cost, with the build farm's share of the rebuild time and,
  apart, the unclear share.
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

- what changed since the previous build of the image (failed and
  canceled builds do not count: BuildKit records no provenance for
  them): commit, node, Dockerfile lines, build arguments, base image, the
  files a rebuilt step copies (a build argument that reaches a step
  through `ENV` counts as that argument's change);
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

**While a build runs**, its job's page follows it: what runs now and,
against the last build of the image that ran the same step, how long the
rest likely takes; the node's CPU and memory, the builds on its daemon and
the CPU pressure; every step on a timeline that grows, each opening to its
last 8 lines (the running one opens by itself, a finished one folds). The
pipeline's timeline opens a running job into its stages so far. The job
names itself to the box through the CI component (the build option
`ci-accelerator.io/traceparent`); a build without it shows once it is
done.

**Compare** any two builds of an image side by side
(`/builds/<id>/compare`): time, commit, node, build arguments, the
Dockerfiles' differing lines and every step's state and time. The Compare
button on a build's page (key `x`), "Compare two builds" on the builds
list, "compare two" on an image's page and Ctrl+K lead there.

**Speed-ups**: a step that runs a package manager (apt, apk, dnf, pip,
uv, npm, Yarn, pnpm, Go, Cargo, Maven, Gradle and about twenty more)
without a cache mount, or with one that cannot work, gets a hint with a
ready fragment for the Dockerfile and how long that step took in the
image's builds of the last 30 days. A cache mount cannot work when a flag,
a setting or a cleanup switches the cache off, when it sits on another
path than the one the manager uses, when it covers what the image needs,
when apt, apk, dnf or zypper share it without `sharing=locked`, or when a
non-root step cannot write to it. The hints come from the operation
BuildKit recorded (command, environment, user, mounts), not from the
Dockerfile's text; where the path cannot be known from it, the hint names
the manager's own command that prints it. They show as a card above the
steps, on the Dockerfile tab, on the step panel's Speed-up tab and as
`hints` in `GET /api/v1/builds`;
[the Dockerfile guide](dockerfile-guide.md#3-use-cache-mounts-for-package-managers) explains the
mounts.

Every figure is measured, not estimated. Medians and p95 are nearest-rank
percentiles, so always one of the measured times; "usual" needs five
builds of its kind and a p95 twenty rebuilds (a dash until then). The
time a cached build saved is how long the steps it did not run took when
they last ran, and sizes are in binary units (KiB, MiB, GiB). Every
job's log can link to its page in the console (`global.console.url`). Jobs that
build no image stay in GitLab. With sign-in through GitLab
(`control-plane.signIn`), each person sees the projects whose jobs GitLab
lets them read; without it, keep the console inside the cluster (`kubectl
port-forward`). [Setting up the console](console.md#setting-up-the-console) has the
three levels.

## Setting up the console

The console is on by default. The control plane serves it: a controller
(Deployment `<release>-controller`, one pod: the store, the sessions,
every GitLab token) and a web (Deployment and Service `<release>-web`, two
pods serving the pages), which reads the controller's API over HTTPS and
proves who it is with a ServiceAccount token; see
[the architecture](architecture.md#the-control-plane-and-the-console).
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
`--gitlab-token-file ./token` and the same GitLab URL and runner token.
Like every second run, it keeps `values.yaml`, and it also brings the
script's cert-manager and Helm
([what a second run changes](upgrading.md#upgrade-with-the-new-releases-script)).

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
   [gitops.md](gitops.md#the-session-key)):

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
           admins: ["id:1234567"]                 # GitLab user ids who see everything
   ```

   A `gitlab` entry without `url` is the runner's GitLab; several entries
   serve several GitLabs. Admins are named by user id (`id:<n>`, shown on
   the person's GitLab profile page); usernames are refused, since GitLab
   gives a renamed or deleted account's username to the next account that
   takes it. On the one-machine install
   the values live in `/var/lib/ci-accelerator/values.yaml`; apply them
   with the command in the [README's quickstart](../README.md#try-it-on-one-machine)
   (it uses the Helm the script installed).

   An install made by an older script may have another Helm under
   `/usr/local/lib/ci-accelerator/` and another version: the script prints
   the command for its own at the end
   ([details](upgrading.md#apply-your-own-changes-or-upgrade-by-hand)).

Every key is in [charts/control-plane/values.yaml](../charts/control-plane/values.yaml).

## Logs

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
[troubleshooting](troubleshooting.md#reading-the-logs).

## Sending metrics and traces to your backend

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
