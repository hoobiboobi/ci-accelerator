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
(Deployment `ci-accelerator-controller`, one pod: the store, the
sessions, every GitLab token) and a web (Deployment and Service
`ci-accelerator-web`, two pods serving the pages), which reads the
controller's API over HTTPS and proves who it is with a ServiceAccount
token ([architecture.md](architecture.md#the-control-plane-and-the-console)).
The names assume the install of [install.md](install.md): release and
namespace `ci-accelerator` (`helm list -A` shows yours). The install
notes come back any time with `helm get notes ci-accelerator -n
ci-accelerator`; on the one-machine install, with the script's Helm
([quickstart.md](quickstart.md#3-run-it)).

The console works in three levels; each adds to the previous one.

| | by itself (default) | with a GitLab token | with sign-in |
|---|---|---|---|
| image builds, steps, cache verdicts, what changed, pipeline timeline | yes | yes | yes |
| job results, how long jobs waited for a runner, "N other jobs" | no | yes | yes |
| "files or node" settled by comparing the two commits, end of a job's log | no | yes, in the background too | yes, while pages are open |
| why the build router chose a build's node | yes | yes | yes |
| who sees what | everyone who reaches it | everyone who reaches it | each person: the projects GitLab lets them read |
| the console reads GitLab with | nothing | one shared token | each person's own token |

Which level runs now: `helm get values ci-accelerator -n ci-accelerator
-o yaml` shows `control-plane.gitlab.token` (level 2) or
`control-plane.signIn` (level 3), or neither.

Every change below is applied the same way, with the version you run
(`helm list -n ci-accelerator`, the CHART column) and your values file:

```sh
helm upgrade ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.5.2 \
  -n ci-accelerator -f values.yaml --wait --timeout 15m
```

The control plane's pods restart; the daemons do not. On the one-machine
install the values are `/var/lib/ci-accelerator/values.yaml` and the
command is in [quickstart.md](quickstart.md#9-change-the-values).

### Level 1: by itself

The console reads the daemons' build history and the CI job pods,
nothing else: every image build with its steps, the verdict (from cache,
your change, build farm, unclear, failed) and the pipeline's timeline
are there; a job's result is "unknown", runner waits are not shown, and
a rebuild whose cache sat on another node stays "unclear". It has a
ClusterIP Service and no sign-in, so whoever reaches it sees everything:
keep it inside the cluster and reach it with

```sh
kubectl -n ci-accelerator port-forward svc/ci-accelerator-web 8080
```

at <http://localhost:8080/> (the one-machine install: the SSH tunnel of
[quickstart.md](quickstart.md#5-open-the-console)).

### Level 2: with a GitLab token

One token with the scope `read_api` for all viewers. The controller reads
with it each pipeline's jobs while pages show it, asks GitLab to compare
the commits of a rebuild whose cache was on another node (in the
background, at most 25 requests a minute per project; GitLab's answers
are stored, so they settle the build again after a restart) and reads
the end of a job's log on request. When the commits show no change
in what the rebuilt step copies, but the step reads the whole build
context and `.dockerignore` does not keep `.git` out, the verdict names
the job's `.git`: every job's checkout writes its own (see
[dockerfile-guide.md](dockerfile-guide.md)).

1. **Create the token in GitLab.** For a team, a group access token:
   the group > **Settings > Access tokens** > **Add new token**, role
   **Reporter** (enough to read jobs), scope `read_api`, an expiry date.
   For one project, a project access token the same way under the
   project's Settings. For a trial, a personal access token: your avatar
   > **Edit profile** > **Access** > **Personal access tokens**, scope
   `read_api` (it is tied to your account and its memberships). GitLab
   shows the token (`glpat-...`) once. Note the expiry date: when it
   passes, the console silently drops back to level 1.
2. **Put it into a Secret.** The key must be named `token`:

   ```sh
   kubectl -n ci-accelerator create secret generic gitlab-api-token --from-literal=token=glpat-...
   ```

3. **Add to `values.yaml` and apply** (the command above). If the file
   already has a `control-plane:` key, put `gitlab:` under it; a second
   top-level `control-plane:` key silently replaces the first:

   ```yaml
   control-plane:
     gitlab:
       token:
         existingSecret: gitlab-api-token
   ```

4. **Check.** Open a finished pipeline in the console: each job now has
   its result and "waited for a runner" time. The controller reads the
   Secret at start, so after changing the token later, re-create the
   Secret and `kubectl -n ci-accelerator rollout restart
   deploy/ci-accelerator-controller`. A refused token (wrong, expired,
   without `read_api`) shows in the controller's log as `gitlab api`
   warnings with GitLab's 401 or 403: `kubectl -n ci-accelerator logs
   deploy/ci-accelerator-controller | grep 'gitlab api'`.

On the one-machine install, run the script again with
`--gitlab-token-file` instead ([quickstart.md](quickstart.md#7-add-a-gitlab-token)).

### Level 3: with sign-in through GitLab, for a team

Sign-in replaces the shared token: each person signs in with their
GitLab account, the console reads GitLab with their own token and shows
them the projects whose jobs GitLab lets them read; `admins` see
everything. Visibility is per project, not per runner: a person sees the
builds of every project where they may read jobs (Reporter or more,
directly or through a group; a project with public pipelines is visible
to everyone signed in). A group runner and a project runner make no
difference. GitLab's answer about a person's projects is kept for 10
minutes.

The order matters: the console is open to whoever reaches it until
sign-in is on, so the Ingress comes last.

**1. Decide the address.** The console is served over https at the root
of its host (no sub-path): `https://ci-console.example.com`. Plain
`http://` is accepted only on `localhost`, through a port-forward or the
one-machine install's tunnel. Set it in `values.yaml`; from then on
every job's log prints a link to its page there. (Without sign-in, the
web then answers only on that name, on `localhost` and on in-cluster
names; with sign-in on, the sign-in completes only at that address.
`control-plane.signIn.url` overrides it only when the sign-in address
must differ from the one in the job logs, which is rare.)

```yaml
global:
  console:
    url: https://ci-console.example.com
```

**2. Create the OAuth application in GitLab.** For a team, a group
application: the group > **Settings > Applications** (needs the Owner
role); on your own GitLab, **Admin > Applications**. A user application
(your avatar > **Edit profile** > **Access** > **Applications** > **Add
new application**) works for a trial, but stops with the account. Fill
in:

- **Name:** anything, for example `ci-accelerator console`.
- **Redirect URI:** `https://ci-console.example.com/auth/callback/<id>`,
  where `<id>` is six characters derived from the GitLab URL exactly as
  `gitlab-runner.gitlabUrl` has it, trailing slash included. The install
  notes print the whole URI (`helm get notes ci-accelerator -n
  ci-accelerator | grep auth/callback`, with `global.console.url` set);
  or compute it:

  ```sh
  printf %s https://gitlab.com/ | sha256sum | cut -c1-6     # shasum -a 256 on macOS
  ```

  For `https://gitlab.com/` it is `993196`.
- **Confidential:** ticked.
- **Scopes:** `openid` and `read_api`, nothing else.

Select **Save application**. GitLab shows the **Application ID** and
the **Secret** (`gloas-...`): copy both now, the Secret is shown once
(**Renew secret** makes a new one later).

**3. The two Secrets.** The keys must be named as shown; create them
before the upgrade, because the controller mounts them and waits
(`FailedMount`) until they exist:

```sh
kubectl -n ci-accelerator create secret generic console-gitlab-oauth \
  --from-literal=client-id=<the Application ID> --from-literal=client-secret=gloas-...
kubectl -n ci-accelerator create secret generic console-session \
  --from-literal=key=$(openssl rand -base64 32)
```

The session key seals each person's GitLab token at rest; changing it
later signs everyone out (with Flux or Argo CD, see
[gitops.md](gitops.md#the-session-key)). The controller reads both at
start: after changing one, `kubectl -n ci-accelerator rollout restart
deploy/ci-accelerator-controller`.

**4. The values.** Your GitLab user id for `admins` is the number on
your profile page (`https://gitlab.com/<username>`, "User ID" under the
name), not the username: GitLab gives a renamed or deleted account's
username to the next account that takes it. Admins see every project,
builds the console could not attribute to a project, and may call the
JSON API.

```yaml
global:
  console:
    url: https://ci-console.example.com
control-plane:
  signIn:
    enabled: true
    sessionKey: { existingSecret: console-session }
    gitlab:
      - existingSecret: console-gitlab-oauth   # client-id, client-secret
        admins: ["id:1234567"]                 # GitLab user ids who see everything
```

A `gitlab` entry without `url` is the runner's GitLab; several entries
serve several GitLabs. A shared token and sign-in exclude each other:
remove `control-plane.gitlab.token` from `values.yaml` (and, if the
install set it with `--set`, add `--set
control-plane.gitlab.token.existingSecret=` to the upgrade, or the chart
stops with `gitlab.token and sign-in exclude each other`). Apply with
the command above. The Secret `gitlab-api-token` is no longer read:
delete it and revoke the token in GitLab. The stored builds stay.

**5. Check before the Ingress.** The controller's log names the redirect
URI it expects, and every sign-in:

```sh
kubectl -n ci-accelerator logs deploy/ci-accelerator-controller | grep -E 'redirect|signed in'
```

Compare the URI with the application's, character by character. Through
a port-forward the sign-in page shows but the sign-in itself completes at
`global.console.url`, so the full check comes after the Ingress.

**6. Expose the web.** The chart creates no Ingress. The console is the
ClusterIP Service `ci-accelerator-web`, port 8080, plain HTTP: put your
Ingress or gateway with TLS in front of it, for example

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: ci-accelerator-console
  namespace: ci-accelerator
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt    # or a TLS Secret of your own
spec:
  ingressClassName: nginx
  tls:
    - hosts: [ci-console.example.com]
      secretName: ci-console-tls
  rules:
    - host: ci-console.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: ci-accelerator-web
                port:
                  number: 8080
```

The box's NetworkPolicy admits connections to the web from every other
namespace except the jobs namespace. An Ingress controller on the host
network (ingress-nginx with `hostNetwork: true`, k3s's Traefik on some
setups) connects from the node addresses, which the policy refuses: the
browser hangs and the Ingress controller's log (not ci-accelerator's)
says `upstream timed out`. Add the
addresses (`kubectl get nodes -o wide`, INTERNAL-IP):

```yaml
control-plane:
  networkPolicy:
    extraFrom:
      - ipBlock: { cidr: 10.0.0.0/24 }
```

**7. Check.** In a private browser window, open
`https://ci-console.example.com/`: a sign-in page with one button per
GitLab; GitLab asks you to authorize the application (`openid` and
`read_api`, read-only); then the Pipelines page of your projects. A
person without a readable project sees an empty list. Sign out is in
the sidebar; several people can be signed in at once. If GitLab answers
"The redirect URI included is not valid", the application's URI and
the one in the controller's log differ (the trailing slash of the
GitLab URL is the usual cause); if sign-in ends with an error after
Authorize, the `client-secret` does not match the application's:
re-create the Secret with both values and restart the controller. More
in [troubleshooting.md](troubleshooting.md#the-console).

Every key is in [charts/control-plane/values.yaml](../charts/control-plane/values.yaml).

## Logs

Every role of the control plane (controller, web, router, disk probe)
writes one JSON object per line with its `role`: when it starts (with the
settings it runs with), is ready, stops and has stopped, and a line per
stored build, job start and end, change of the daemons, sign-in and
sign-out and routed connection.

```sh
kubectl -n ci-accelerator logs deploy/ci-accelerator-controller                                   # the store, GitLab, sign-in
kubectl -n ci-accelerator logs -l app.kubernetes.io/component=web --prefix --tail=-1                # the pages
kubectl -n ci-accelerator logs -l app.kubernetes.io/component=router --prefix --tail=-1 | grep routed   # where jobs went
```

With `-l`, kubectl prints only the last 10 lines of each pod unless
`--tail=-1` is given. Messages to grep for: `build stored`, `build
attributed`, `routed`, `signed in`, `signed out`, `gitlab api` (a
refused or failed GitLab call). `jq -r '[.time,.level,.msg]|@tsv'`
makes the lines readable; or set `control-plane.log.format: text`.

`control-plane.log.level: debug` (or one role's
`control-plane.<controller|web|router>.logLevel`) adds every request,
GitLab call, controller API call and closed router connection; apply it
like any value (the control plane's pods restart, the routers draining
their builds first) and set it back to `info` the same way (a `--set`
survives `--reset-then-reuse-values`: set it to `info`, not just drop
it). BuildKit's own log is
`buildkit.config.log.level`/`format` and the disk probe's
`buildkit.diskProbe.logLevel`; both are empty by default, and setting
them restarts every daemon once. No log carries a token, a cookie, a
query string, a build argument or step output, and anything shaped like
a GitLab token is masked. Which command reads which pod:
[troubleshooting.md](troubleshooting.md#reading-the-logs).

## Metrics and traces

The box keeps no metrics or traces; the daemons push them over OTLP to a
backend you run: [production.md](production.md#metrics-and-traces-to-your-backend).
