# Troubleshooting

Symptoms first, then runbooks. Commands assume the release `ci-accelerator`
in namespace `ci-accelerator` with jobs in `ci-accelerator-jobs`, so the
objects are `ci-accelerator-buildkit` (the daemons),
`ci-accelerator-router` (the build router), `ci-accelerator-controller`
and `ci-accelerator-web` (the console); adjust the names. How the parts fit
together is in [architecture.md](architecture.md).

Talking to a daemon from inside its pod needs an empty environment, as the
probes do, or tracing variables can make `buildctl` hang on exit:

```sh
kubectl -n ci-accelerator exec <daemon pod> -c buildkitd -- env -i OTEL_TRACES_EXPORTER=none \
  /usr/bin/buildctl --addr unix:///run/buildkit/buildkitd.sock <command>
```

## The install fails

The chart stops on purpose when the install could not work. The message
names the fix; the common ones:

| Message starts with | Fix |
|---|---|
| `namespace ... enforces Pod Security level` | Label the box namespace `pod-security.kubernetes.io/enforce=privileged` |
| `no node matches the build-node selector` | Label and taint the build nodes, or `global.buildNodes.enabled=false` |
| `tls.source=certManager, but the cluster has no cert-manager` | Install cert-manager (command in the message); for `helm template`, pass `--api-versions cert-manager.io/v1` |
| `gitlab-runner.gitlabUrl is required` / `gitlab-runner.runners.secret is required` | Set them |
| `Secret .../gitlab-runner-token ... has no runner-registration-token key` | See [The runner](#the-runner) |
| `runner-token does not look like a token` | The Secret holds a placeholder or a pasted string with spaces; re-create it |
| `Secret ... (tls.existingSecret) has no ca.crt` | `kubectl create secret tls` writes no `ca.crt`; use `create secret generic` with all three files |
| `control-plane.enabled with buildkit.tls.source=existingSecret` | Without cert-manager the controller needs two Secrets from the daemons' CA: its client certificate in `control-plane.tls.existingSecret` (`scripts/buildkit-certs.sh ... --no-server --client controller`) and its API's server certificate in `control-plane.controller.api.tls.existingSecret` (`scripts/buildkit-certs.sh ... --controller <release>`) |
| `the build router is part of the control plane` | With `control-plane.enabled=false`, also set `global.router.enabled=false`: the jobs then connect to the daemons' Service |
| `Namespace ... (global.jobs.namespace) exists and was not created by this release` | Helm cannot adopt a namespace it did not create: `jobsNamespace.create=false` to use yours (label it `baseline`), or delete it |
| `Namespace ... (global.jobs.namespace) is still being deleted` | A reinstall right after uninstall: wait for the namespace to go |
| `jobsNamespace.create=false would make Helm delete Namespace ...` | The release created the jobs namespace; Helm deletes what leaves its manifest. `kubectl annotate namespace <jobs> helm.sh/resource-policy=keep`, then upgrade (or keep `create=true`) |
| `gitlab-runner.runners.config does not render to valid TOML` | Your own `runners.config` (or a value it uses) breaks the TOML; the message names the line |
| `global.buildNodes.selector has kubernetes.io/arch=...` | Set `global.jobs.architecture` to the same architecture |
| `image.tag is ..., but image.digest is still the digest of ...` | Set `image.digest` to the new tag's digest, or `""` |
| `image.digest (control-plane.image.digest in the box) is the digest of the v... image, but this chart runs v...` | The digest pinned for the release before, kept by the upgrade: set the new release's digest from its notes, or remove it |
| `diskProbe.image.digest (buildkit.diskProbe.image.digest in the box) is the digest of the v... image, but the disk probe runs v...` | Set the digest the message names (the release notes list it too), or remove it |
| `priorityclasses.scheduling.k8s.io is forbidden` | The installer may not create cluster-scoped objects: `buildkit.priorityClass.create=false`, or name an existing class in `buildkit.priorityClassName` (and the same class, or `""`, in `control-plane.router.priorityClassName`) |
| `clusterroles.rbac.authorization.k8s.io is forbidden` | The controller checks its callers' ServiceAccount tokens with a TokenReview, which needs a ClusterRole and its binding: install with the right to create them |
| `ci-accelerator 0.3.0 cannot upgrade a 0.2.x release in place`, or `release ... has the console of ci-accelerator 0.2.x` | 0.3.0 renamed every object and value of the console: uninstall, wait for the pods, install again ([below](#upgrading-from-02x-to-030)) |
| `values under console: are the 0.2.x console's` | Move them under `control-plane:` (pod settings and volume under `control-plane.controller`, the web's under `control-plane.web`) |

Checks that read the live cluster (Pod Security, build nodes, Secrets)
need read access to Namespaces, Nodes and Secrets. Installers without it
switch them off: `buildkit.preflight.podSecurity`,
`buildkit.preflight.buildNodes` and `buildkit.preflight.tlsSecret` (the
daemons; without the `buildkit.` prefix in the standalone chart),
`preflight.runnerSecret` (the box's runner token Secret).

## The daemons do not start

```sh
kubectl -n ci-accelerator get ds ci-accelerator-buildkit
kubectl -n ci-accelerator get pods -l app.kubernetes.io/name=buildkit -o wide
kubectl -n ci-accelerator describe pod <daemon pod> | tail -20
```

| Symptom | Cause |
|---|---|
| `DESIRED 0` | No node matches the selector (build nodes not labelled) |
| `Pending` | Taint without the matching toleration, or no resources left on the node |
| `ContainerCreating`, event about the TLS Secret | The certificate is not issued yet: `kubectl -n ci-accelerator get certificate`, then `kubectl describe certificate` |
| `CrashLoopBackOff`, log `failed to load config` | A configuration buildkitd rejects (often `config.extraConfig`). The rollout stops on the first node; `helm rollback` |
| `CrashLoopBackOff` with hostPath, lock error | Another daemon holds the state directory: a second release on the same path, or a surge pod (the chart refuses `maxSurge` with hostPath) |
| `disk-probe` in `ImagePullBackOff`, daemon pod not started | The probe runs the `ghcr.io/hoobiboobi/ci-accelerator` image at its own pinned tag (`buildkit.diskProbe.image`, default tag `v0.3.3`), and a pod starts only with all its images: mirror that tag too, or `buildkit.diskProbe.enabled=false` |
| No pods at all after a successful `helm install` | The namespace enforces `baseline`/`restricted` without a label the chart could see (some distributions default to `baseline`); see the DaemonSet's events |

The daemons run BuildKit v0.34.1. To check what they actually run, the
console's Nodes page names the BuildKit version (a column appears when the
daemons differ), or:

```sh
kubectl -n ci-accelerator get ds ci-accelerator-buildkit \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
```

An earlier chart's default digest kept by `helm upgrade --reuse-values`
counts as the current one; a tag and digest of your own stay as you set
them. To pin an earlier BuildKit, set its tag and its digest together
(`buildkit.image.tag` and `buildkit.image.digest` in the box): the chart
refuses another tag with the default digest.

## The runner

The runner chart mounts **both** keys of its token Secret:

```sh
kubectl -n ci-accelerator create secret generic gitlab-runner-token \
  --from-literal=runner-token=glrt-... --from-literal=runner-registration-token=
kubectl -n ci-accelerator get secret gitlab-runner-token \
  -o go-template='{{range $k, $v := .data}}{{$k}} len={{len $v}}{{"\n"}}{{end}}'
```

| Symptom | Cause | Fix |
|---|---|---|
| Runner pod in `ContainerCreating`, event `FailedMount ... non-existent secret key: runner-registration-token` | The empty key is missing | `kubectl -n ci-accelerator patch secret gitlab-runner-token --type merge -p '{"data":{"runner-registration-token":""}}'` |
| Log `Verifying runner... is not valid` | Wrong or placeholder token, or the runner was deleted in GitLab | Fix the Secret, then `kubectl -n ci-accelerator rollout restart deploy/ci-accelerator-gitlab-runner` (the token is read at start) |
| The runner is online, jobs stay pending or run elsewhere | Tags: a runner with tags that does not run untagged jobs only takes jobs with matching `tags:`. On gitlab.com untagged jobs go to the shared runners, which have no `BUILDKIT_*` variables | Add `tags:` to the job, or allow untagged jobs on the runner |
| Runner pod `Pending` (`Unschedulable`), `helm install --wait` times out on the runner Deployment; the same for the control plane's pods | Every node is a tainted build node: the runner manager, the controller and the web do not tolerate the build-node taint (the router does: it runs on the build nodes) | Keep one node without the taint, or add tolerations: `gitlab-runner.tolerations`, `control-plane.controller.tolerations` and `control-plane.web.tolerations` |
| Job pod `Pending` | It cannot go to a build node: missing toleration or node selector in a hand-made runner configuration | Use the configuration from the umbrella or `integrations/gitlab-runner` |
| Job fails pulling `gitlab-runner-helper:x86_64-...` with `no match for platform` | arm64 build nodes, but the runner assumed amd64 | `global.jobs.architecture=arm64` (or `kubernetes.io/arch` in the fragment's node selector) |
| Jobs rejected by Pod Security | The jobs namespace enforces `restricted` | Use `baseline` |

## A job cannot reach the daemons

In the box, jobs connect to the build router,
`tcp://ci-accelerator-router.ci-accelerator.svc:1234`, which passes the
connection through to a daemon; TLS stays between the job and the daemon.
With `global.router.enabled=false` they connect to the daemons' Service,
`tcp://ci-accelerator-buildkit.ci-accelerator.svc:1234`. A runner of your
own (`integrations/gitlab-runner/values.yaml`) must use the address that
matches.

Run the connection check from the job (or from a debug pod in the jobs
namespace with the client Secret mounted):

```sh
buildctl --addr "$BUILDKIT_REMOTE_ADDR" --tlsdir "$BUILDKIT_TLS_DIR" debug workers -v
```

| Error | Cause |
|---|---|
| `x509: certificate signed by unknown authority` | The client's `ca.crt` is from another CA than the daemons' certificate (a Secret from another installation, or a CA that was replaced) |
| `certificate is valid for ..., not <host>` | The address is not among the certificate's names (custom DNS names, IPs, NodePort clients): `buildkit.tls.certManager.extraDnsNames` / `ipAddresses`, or `--tlsservername`. Not valid for `<release>-router...`: the daemons still serve a certificate without the router's names. With cert-manager, the Secret was issued again after the daemons started (a Secret left over from 0.2.x): restart the daemons. With `buildkit.tls.source=existingSecret`, make it again with `scripts/buildkit-certs.sh --name <release>-buildkit --namespace <namespace> --router <release>` (the same `--out` directory, so the CA stays), replace the Secret and restart the daemons |
| `tls: certificate required`, or `broken pipe` / `connection reset by peer` right after connecting | No client certificate, or one from another CA: the client Secret is not mounted or comes from the wrong namespace. TLS 1.3 often reports a missing certificate only as a reset |
| `certificate has expired` | The daemons still serve an old certificate after a renewal: restart them (`kubectl -n ci-accelerator rollout restart ds/ci-accelerator-buildkit`) |
| `i/o timeout`, `context deadline exceeded` | Network, not TLS: the NetworkPolicy does not admit the job's namespace (the routers' `control-plane.router.clientNamespaces`, default the jobs namespace; without the router `buildkit.tls.client.namespaces` / `buildkit.networkPolicy.ingress.namespaces`), or routing is `Local` and the job's node has no ready router or daemon, or the job pod is not on a build node. With the router on, the daemons admit only the routers and the controller: a job that connects to a daemon around the router times out by design |
| The connection closes at once, without a TLS error | The router could not hand it to a daemon. Its log says why: `connection refused: no daemon` (no daemon is ready, or none in the job's zone with `control-plane.router.crossZone=false`) or `daemon not reached` (the daemons' NetworkPolicy does not admit pods labelled `ci-accelerator.io/component: router`: give the buildkit chart `global.router.enabled=true` and `global.router.service` (the router's Service name), as the box does) |
| `Unavailable ... connection refused` | Nothing is listening: no router (or, without the router, no daemon) is running, or a wrong address |

The router's pods and their readiness: a router is ready only while it
knows a ready daemon.

```sh
kubectl -n ci-accelerator get pods -l app.kubernetes.io/component=router -o wide
kubectl -n ci-accelerator logs <router pod on the job's node> | grep -v '"msg":"routed"'
```

## Builds are slow or not cached

**Where did the router send it?** The build page's details say "Routed by
the build router" and why:

- *the job's own node, whose cache holds the project's layers*;
- *the node whose cache holds the project's most recently used layers*
  (another node of the job's zone);
- *the project's node by hash: no cache held its layers*;
- *in another zone: no daemon of the job's zone was ready*.

No such line: the router did not know the job and sent it to its own
node's daemon (see "The router does not know the job" below), the router
is off, or the controller was not reachable when the router reported its
decision. The Nodes page shows where the project the pages show built in
the last 24 hours ("Where *project* built"), and each daemon's page counts
the router's decisions by reason.

**The router sent a project's job to a cold node.** The router follows
facts the controller stored, per node:

- A project is warm on a node for `control-plane.router.warmDays` (default
  7 days) after its last build there, and for an hour after the router
  sent one of its jobs there. Older builds do not count: the job goes to
  the project's node by hash.
- Builds of a daemon pod that has been replaced since count only when its
  cache survives a restart (`buildkit.storage.type=hostPath`). With
  `emptyDir`, every rollout empties every cache, and the router starts
  over by hash.
- A build counts only when the console attributed it to a project (the job
  pod's annotations from the box's runner configuration or
  `integrations/gitlab-runner/values.yaml`).
- The router follows the project, not each image: a project's jobs go to
  the node where it built most recently, and an image whose previous
  build ran on another node is rebuilt there once. The build page's
  verdict says so ("Why here").
- `control-plane.router.overloadBuilds` (off by default) skips a warm
  daemon that runs that many builds when another one is free.
- While the controller does not answer, the routers keep routing on the
  last view they had (log `controller not reached: routing on the last
  view`); decisions made then are not shown on the pages (`decisions
  dropped while the controller was away`).

**The router does not know the job.** Its log then says `routed a
connection of no known CI job to its node's daemon`, and the job goes to
the daemon on its own node, as without the router (by hash when its node
has no ready daemon). The router knows a job
by the source address of its connection, which must be the address of a
running job pod in the jobs namespace (`control-plane.jobs.namespace`)
whose annotation `job.runner.gitlab.com/url` starts with one of the
control plane's GitLab URLs (`control-plane.gitlab.urls`, or in the box the
runner's `gitlabUrl`, exactly as written). It is not known when:

- the connection comes from another pod (a debug pod, a build started
  outside CI), or from a pod with `hostNetwork`;
- the job pod lacks the label `ci-accelerator.io/component=ci-job`, which
  the box's runner configuration (or `integrations/gitlab-runner/values.yaml`)
  puts on job pods;
- a proxy, mesh sidecar or egress gateway changes the source address on
  the way to the router;
- the job pod is not in the router's view within 3 seconds of connecting
  (the router waits that long for a new pod's address);
- your own runner's `gitlabUrl` differs from the control plane's URL
  (a trailing slash, another host name for the same GitLab).

**Zones.** With `control-plane.router.zones=true` (the default) a job stays
in its node's zone (`topology.kubernetes.io/zone` of the daemons' nodes),
and a project warms up once per zone. Where every node carries its own
zone label, every job stays on its own node: set
`control-plane.router.zones=false` when crossing zones costs nothing.
`control-plane.router.crossZone=false` refuses a job whose zone has no
ready daemon instead of sending it to another zone. A job on a node that
never had a daemon (a runner tier on any node) has no known zone: it goes
to the project's warm daemon or its daemon by hash in any zone, and is
never refused for crossing zones.

Then the build itself:

1. **Which daemon built it?** The build page names the build node; the
   job's `debug workers -v` prints `worker.hostname` of the daemon that
   connection was routed to (the router decides each connection on its
   own, so a later one can go elsewhere); metrics and traces sent to your backend carry
   `k8s.pod.name`. The cache is per daemon: a build on another daemon than
   the image's last one starts cold.
2. **Was the daemon replaced?** With `emptyDir`, every new pod (rollout,
   eviction, deletion) starts with an empty cache; a container restart inside
   the same pod (the RESTARTS column) keeps it. The build page says when a
   rebuild ran under a replaced daemon on the same node, and whether its
   cache persisted:
   ```sh
   kubectl -n ci-accelerator get pods -l app.kubernetes.io/name=buildkit
   ```
3. **Does the build context change every time?** `.git` in the context is
   the usual culprit, since every job has a fresh clone. So are generated
   files. See [dockerfile-guide.md](dockerfile-guide.md).
4. **Do the build arguments change every time?** An `ARG` fed with
   `CI_JOB_ID` or a timestamp invalidates every step after it. The build
   page reports a build argument that reaches a step through `ENV` as that
   argument's change.
5. **Did GC remove it?**
   - Cache mounts expire after `config.gc.cacheMounts.keepDuration` (14
     days) unused.
   - Everything else goes when the cache is above `config.gc.maxUsedSpace`
     or free disk is below `minFreeSpace`.
   - Check the usage (the daemon's page in the console shows the cache by
     kind, too):
     ```sh
     kubectl -n ci-accelerator exec <daemon pod> -c buildkitd -- env -i OTEL_TRACES_EXPORTER=none \
       /usr/bin/buildctl --addr unix:///run/buildkit/buildkitd.sock du | tail -3
     ```

To see what differs between a slow build and a fast one of the same image,
open the build and press Compare (key `x`): time, commit, node, build
arguments, the Dockerfiles' differing lines and every step side by side.

**The job is slow before its build starts.** The build page's details
("Before the build") split the time from the job's start to the build,
from the job's pod as Kubernetes records it (whole seconds):

- *the runner preparing the job's pod*: the runner creating the pod;
- *the job's pod starting*: the pod waiting to be scheduled and its images
  pulled. Long here: the build nodes are full (pods pending: `kubectl
  get pods -n <jobs namespace>` and the scheduler's events), or a job image
  is pulled again (pin images by digest and keep them on the nodes);
- *the runner attaching to the pod, the checkout and the job's script up
  to the build*: the runner notices the running pod by polling (several
  seconds), then clones the repository (`GIT_DEPTH`) and runs the script
  before `buildctl`.

The pipeline's timeline draws the same parts on each job's bar.

## Builds hang or fail

Open BuildKit issues that still apply to v0.34.1:

- **Every build on one daemon hangs, but the daemon still answers its
  probes.** A registry push that got stuck can hold BuildKit's per-registry
  slots and block every build on that daemon (moby/buildkit#7275). Find the
  daemon (the build page, or `debug workers -v` from the job) and delete
  its pod; the DaemonSet replaces it.
- **A build fails with `failed to get state for index`.** Another build
  that shared its steps was cancelled (moby/buildkit#7268). Retry the job.

Other causes:

- **A job waits for a long time after a build node died.** `buildctl` has
  no keepalive: when a node disappears without closing its connections
  (power loss, network partition), the job waits for TCP to give up, which
  can take from 15 minutes to two hours. Bound the build in the job (the
  CI component in `ci/gitlab/` does):
  ```sh
  timeout -s TERM -k 30 3000 buildctl ...
  ```
  `buildctl --timeout` only bounds connecting. Through the router, a
  daemon whose node vanished is usually noticed sooner: the router keeps
  TCP keepalive on both of its connections (15 seconds idle, then three
  probes 15 seconds apart) and closes the job's connection when the
  daemon's side is gone. Keep the timeout anyway.
- **A step failed with exit code 137 or 143.** The build page reads them:
  137 is SIGKILL (often the out-of-memory killer, in the daemon's memory
  limit), 143 is SIGTERM (the job was cancelled or timed out, or the daemon
  was shutting down). It also says whether the failed step is new or
  changed in this build.
- **A registry fails certificate verification after the upgrade to
  0.3.0.** BuildKit v0.34.1 is built with Go 1.26.10, which no
  longer accepts the revoked eMudhra intermediate certificates: a registry
  whose certificate chain uses one of them now fails verification, for
  base images and pushes alike. The registry has to serve a chain without
  them.
- **Builds stop when a router restarts.** A stopping router keeps
  carrying its open connections for `control-plane.router.drainSeconds`
  (300) and then closes them (log `drain time over: closing open
  connections`); builds longer than that fail on a rollout of the routers.
  The routers roll one node at a time, when their own settings
  (`control-plane.router.*`, the log settings, `control-plane.gitlab.urls`)
  or the image change; a daemon rollout does not restart them. A new
  `gitlab-runner.gitlabUrl` in the box does not roll them: the routers
  take it in place and the controller starts again by itself, within
  about two minutes of the upgrade.

Fixed upstream: the daemon panic when one of several builds sharing a step
was cancelled (moby/buildkit#7253) is fixed in BuildKit v0.34.0. If
daemons still restart that way, check that they run v0.34.1
([above](#the-daemons-do-not-start)).

## Disk, inodes and processes

**Sizing a dedicated disk.** BuildKit sizes are binary. On a 3 TB NVMe
(about 2.7 TiB):

```yaml
storage:
  type: hostPath
  hostPath:
    path: /var/mnt/nvme/buildkit   # a directory on the mounted disk, created in advance
config:
  gc:
    maxUsedSpace: "2TB"            # 2 TiB
    reservedSpace: "100GB"
    minFreeSpace: "400GB"
```

The filesystem matters too: XFS (dynamic inodes, no root reserve) is the
tested choice; the README's [Where the cache lives](../README.md#where-the-cache-lives)
says why and how to prepare the disk.

Keep headroom beyond `maxUsedSpace`. Two reasons:
- GC runs only after builds, while running builds and BuildKit's databases
  grow outside the GC budget. One of those databases grows without bound
  (moby/buildkit#7263, still open in v0.34.1).
- On a node's root disk, `minFreeSpace` must stay above kubelet's eviction
  thresholds (10-15%); the chart's default is 30%. An evicted daemon gets
  about a second of grace and loses its running builds.

**Inodes.** Package caches (`node_modules`) create millions of small
files. On ext4, check `df -i` on the cache disk; XFS allocates inodes
dynamically. The console warns when under 5 % of inodes are free.

**Processes.** RUN steps run inside the daemon's pod, so their processes
count against the node's pod PID limit (kubelet `podPidsLimit`). With high
`config.maxParallelism` and builds that start many processes, raise the
limit on build nodes.

## Cluster integration

- **Cluster Autoscaler.** It does not count DaemonSet pods when it decides
  a node is idle, so it may remove a build node and its cache. Annotate
  build nodes with `cluster-autoscaler.kubernetes.io/scale-down-disabled=true`,
  or keep build pools out of autoscaling.
- **Resources.** The chart's default requests (500m CPU, 512Mi) only keep
  the daemons out of BestEffort, and the install notes say so while they
  are in place. Size `buildkit.resources` to the build nodes; the chart's
  values list sizes (production started from requests of 48 CPU / 128 GiB
  and a memory limit of 128 GiB). A LimitRange can add a default memory
  limit, and an OOM kill takes the daemon down with every running build.
- **Priority.** The chart gives the daemons their own PriorityClass
  (`<namespace>-<release>-buildkit`, value 1000000, never preempting): under
  node pressure the kubelet evicts CI jobs before them. In the box the
  routers run with the same class. Creating it needs permission for
  cluster-scoped objects; otherwise set `buildkit.priorityClass.create=false`,
  or name your own class in `buildkit.priorityClassName`, and set
  `control-plane.router.priorityClassName` to that class or `""`.
- **kube-proxy replacements.** Same-node routing to the router (and,
  without the router, to the daemons) needs kube-proxy 1.34+ or a
  replacement that implements `PreferSameNode`. Others balance across
  nodes silently. With the router on, the router still picks the daemon by
  the job's own node and project, so this costs a hop to another node, not
  the cache; without it, caches stay cold and builds still work.

## The console

- **Pages answer "The console's controller is not reachable" (502).** The
  web's pods (`deploy/ci-accelerator-web`) serve the pages; the controller
  (`deploy/ci-accelerator-controller`) holds the data. During a controller
  restart this lasts a few seconds. If it stays: `kubectl logs
  deploy/ci-accelerator-controller` for the controller (`controller API
  refused a call` there means it refused a caller's token, see below),
  `kubectl logs deploy/ci-accelerator-web` for the web's side (a
  NetworkPolicy that blocks the web shows there as failed calls to the
  controller).
- **The web's pods are not ready, "The console is starting".** A web pod
  waits for the controller's first answer (its setup) before it serves.
  Check that the Service `ci-accelerator-controller` has an endpoint, that
  the Secret `ci-accelerator-controller-tls` exists (cert-manager issues it;
  without cert-manager `control-plane.controller.api.tls.existingSecret`),
  and that the controller may create TokenReviews (the ClusterRole
  `ci-accelerator-ci-accelerator-controller-tokenreview`, that is
  `<namespace>-<release>-controller-tokenreview`).
- **The controller refuses the web or a router (401/403 in its log).** Their
  tokens are projected for the audience `ci-accelerator-controller`, and
  their ServiceAccounts must be `<release>-web` and `<release>-router` in
  the release's namespace; the controller checks both with Kubernetes
  (TokenReview). The answers are kept for 5 minutes, refusals for 10
  seconds.
- **A running job's page shows no build in progress.** The console knows a
  running build's job only from the build option
  `ci-accelerator.io/traceparent`, which the CI component passes since
  0.3.5 (it holds the job's `TRACEPARENT`, set by the box's runner). A
  component of an earlier release, or a `buildctl build` of your own,
  needs `--opt "ci-accelerator.io/traceparent=$TRACEPARENT"`; without it
  the build shows once it completes. "follows too many running builds"
  means 32 running builds have open pages: it shows when it is done. Its
  progress is read through the daemons' TLS port, like the build history:
  "could not be read" with a TLS or connection error points to the same
  places as missing builds (below).
- **No pipelines on the pages.** The console lists builds of CI jobs it can
  attribute: the job pods must carry the runner's annotations (the box's
  runner configuration or `integrations/gitlab-runner/values.yaml`), and
  `control-plane.jobs.namespace` must be the jobs namespace. Builds started
  outside CI are stored but not listed.
- **The console does not answer through an Ingress (or a tunnel).** The
  box's NetworkPolicy for the web admits pods of other namespaces, not
  traffic from node addresses: an Ingress controller on the host network
  (`hostNetwork: true`) comes from a node. Add the node addresses to
  `control-plane.networkPolicy.extraFrom` (`ipBlock`), or set
  `control-plane.networkPolicy.enabled=false` if the CNI enforces no
  policies anyway. `kubectl port-forward` is not affected; it goes to
  `svc/ci-accelerator-web`.
- **The Nodes page.** One table of the daemons, the ones that need
  attention first; "Needs attention" (`/nodes?show=attention`) shows only
  those, and one note per problem names its nodes. Each daemon has its
  own page (`/nodes/<daemon>`): builds, cache by kind, disk, the router's
  decisions, garbage collection, the projects it built for. The page
  of a daemon replaced by a rollout answers "not found" and links to the
  daemon now on its node first.
- **"A daemon is not reachable".** The controller reads each daemon's
  history over mutual TLS: check its client certificate
  (`ci-accelerator-controller-client`) and that the daemons' NetworkPolicy
  admits the controller's pod (label `ci-accelerator.io/component:
  controller`). Daemons that are gone after a rollout disappear from the
  page by themselves.
- **"A daemon's cache could not be read".** The controller lists each
  daemon's cache once a minute; the row keeps the builds it stored, and the
  figures come back with the next reading.
- **The disk column says "no answer" ("The disk probe did not answer" on
  the daemon's page).** The controller asks the disk probe next to each
  daemon on the port the daemon names (9180 by default): check the daemon
  pod's `disk-probe` container and that the daemons' NetworkPolicy has the
  `disk` port (it has, when `buildkit.diskProbe.enabled`). "No disk probe
  runs next to this daemon": the probe is off (`buildkit.diskProbe.enabled`,
  on in the box).
- **Disk warnings on the Nodes page.** "Less free than the garbage
  collection keeps": BuildKit deletes cache after each build until
  `minFreeSpace` is free again, so builds get colder; give the cache more
  room or lower `maxUsedSpace`. "Under 15 % / 10 % available": if the cache
  shares the node's disk (emptyDir, or hostPath on the root disk), the
  kubelet starts evicting pods at its thresholds; the daemons go last by
  their priority, but CI jobs fail. Under 10 % the Nodes item in the
  sidebar shows a warning. "Under 5 % of inodes free": the disk fills up by
  file count before bytes.
- **No runner or checkout time, no jobs of other runners.** Those come from
  GitLab's API. Sign in through GitLab (`control-plane.signIn`), or
  without sign-in give the controller a token that can read the projects'
  jobs (`control-plane.gitlab.token.existingSecret`). GitLab is called:
  - while the pipelines list and a pipeline's page are in use, for the
    pipelines' jobs (a finished pipeline once);
  - for "files or node": the controller settles every project in the
    background once a minute, with the installation's token at most 25
    comparisons per project, and pages of a project ask with the reader's
    token; a build's page settles its own build;
  - for the end of a job's log, when you ask for it.

  It waits while GitLab rate-limits. The Nodes page shows the GitLab API's
  last state.
- **"Files or node — unclear".** The step copies files, and the job ran on
  a node that had not built the image last. The console settles it with
  GitLab's comparison of the two builds' commits, which it stores, so a
  controller restart settles it again within a minute. It stays unclear
  when:
  - the console has no GitLab token and no sign-in, or (with sign-in) no
    one who may read the project has opened its pages since the build;
  - a commit is not known (the job pod's annotations were missing);
  - GitLab's comparison failed or was cut short (too large or too slow).

  The build router, which keeps a project's jobs on the node that built it,
  makes the case rare; the build page says how the router placed the job.
- **Figures show a dash.** "Usual" times need five builds of their kind,
  and a p95 needs twenty rebuilds; until then the console shows a dash
  rather than a guess.
- **Signed in, but the console says "No project you can read builds
  here".** GitLab does not let that person read the project's jobs
  (Reporter or more, or public pipelines). GitLab's answer is cached, see
  below.
- **Jobs do not print the console's link.** Set `global.console.url`, or
  with your own runner add `CI_ACCELERATOR_CONSOLE_URL` to the runner's
  `environment` and the hook lines from
  `integrations/gitlab-runner/values.yaml`.
- **GitLab says "The redirect URI included is not valid".** The OAuth
  application must list the console's callback exactly:
  `<console address>/auth/callback/<id>`, where the id is the first six hex
  digits of the SHA-256 of the GitLab URL as the runner has it (trailing
  slash included). The install notes and the controller's log
  (`kubectl logs deploy/ci-accelerator-controller | grep redirect_uri`)
  print it.
- **Signing in does not ask for consent again after signing out.** GitLab
  skips its consent screen while the console holds any token of yours (a
  session in another browser). Signing out revokes only that session's
  token; revoking the application in GitLab (Preferences > Applications)
  withdraws all of them.
- **Signed in, but every page sends you back to sign in.** The session
  cookie is Secure: the console's address must be https, or
  `http://localhost` with a port-forward in a browser that keeps Secure
  cookies on localhost (Chrome and Firefox do).
- **Someone removed from a project still sees it.** GitLab's answer on a
  project is kept for 10 minutes (a refusal for 90 seconds).

## Reading the logs

Every role of the `ci-accelerator` binary (controller, web, router, disk
probe) writes one JSON object per line on stderr, each with its `role`.
BuildKit writes its own log, as text unless `buildkit.config.log.format`
says otherwise.

| Pod | Command |
|---|---|
| controller | `kubectl -n ci-accelerator logs deploy/ci-accelerator-controller` |
| web | `kubectl -n ci-accelerator logs deploy/ci-accelerator-web` (one of its pods; `-l app.kubernetes.io/component=web --prefix --tail=-1` for all) |
| build router | `kubectl -n ci-accelerator logs -l app.kubernetes.io/component=router --prefix --tail=-1` (one pod per build node; a job's connections go to the router on its node; with `-l`, kubectl prints only the last 10 lines of each pod unless `--tail=-1` is given) |
| disk probe | `kubectl -n ci-accelerator logs <daemon pod> -c disk-probe` |
| BuildKit | `kubectl -n ci-accelerator logs <daemon pod> -c buildkitd` |

At the default level (`info`) a role says when it starts (with the settings
it runs with), when it is ready, stops and has stopped; the controller adds
a line for every stored build (`build stored`: daemon, steps built and
cached, the reason) and one when it gets its pipeline and job
(`build attributed`: BuildKit sends a record a few seconds before the
client's trace is in it), every job start and end, every change of the
daemons and every sign-in and sign-out; the router one per routed
connection (`routed`: job, project, daemon, node, reason; or `routed a
connection of no known CI job to its node's daemon`). Repeated warnings (a
daemon or GitLab down) are written once a minute with how many were held
back (`suppressed`).

To find something out, raise the level for a while:
`--set control-plane.log.level=debug` (or one role's
`control-plane.<controller|web|router>.logLevel`) adds a line per request,
GitLab call, controller API call and closed router connection;
`control-plane.log.format=text` is easier to read by eye. Changing either
restarts the control plane's pods (the routers one node at a time, each
draining its builds), not the daemons. BuildKit's own log is
`buildkit.config.log.level`/`format` and the disk probe's level
`buildkit.diskProbe.logLevel`; both are empty by default, and setting them
restarts every daemon once. BuildKit at `debug` writes a stack for every
failed call, so turn it back afterwards. Every role also takes
`-log-level` and `-log-format` on its command line.

No log carries a token, a cookie, a query string (an OAuth callback's code
and state), a build argument, step output or a commit title: the roles log
ids, paths and counts, and mask anything that looks like a GitLab token.

## Helm

- **Upgrades:** use `helm upgrade --reset-then-reuse-values`. With
  `--reuse-values`, keys added by a newer chart are missing from the old
  values.
- **`helm template`:**
  - pass `--api-versions cert-manager.io/v1`;
  - on Helm 3 older than 3.19, also `--kube-version 1.34.0` or newer: the
    charts require Kubernetes 1.34.
- **`updateStrategy.type=OnDelete`:** Helm 4's `--wait` waits for pods that
  are never replaced; use `--wait=legacy` or no `--wait`.

## Runbooks

### Empty one daemon's cache

Online, without a restart:

```sh
kubectl -n ci-accelerator exec <daemon pod> -c buildkitd -- env -i OTEL_TRACES_EXPORTER=none \
  /usr/bin/buildctl --addr unix:///run/buildkit/buildkitd.sock prune --all
```

The router still counts the node as warm for the projects that built
there (for `control-plane.router.warmDays`), so their next builds there
start cold once.

### Recover a corrupted state directory

After a crash (OOM kill, node failure) buildkitd repairs some of its state
by itself.

- **Repaired automatically.** If `cache.db` or `history.db` cannot be
  opened, the daemon logs `resetting to empty`, keeps a `*.bak` copy and
  starts with that part empty.
- **Needs a wipe.** If it fails to open `metadata_v2.db` or
  `containerdmeta.db`, it crash-loops. Wipe the node's state directory:

1. Take the daemon off the node: remove the build label, and the
   DaemonSet stops its pod gracefully (the router's pod on that node goes
   too, and the node's jobs cannot be placed there meanwhile).
   ```sh
   kubectl label node <node> ci-accelerator.io/node-role-
   ```
2. Wipe the directory. The debug pod needs a privileged namespace, so run
   it in the box namespace:
   ```sh
   kubectl -n ci-accelerator debug node/<node> -it --image=busybox:1.37 \
     -- sh -c 'rm -rf /host/var/mnt/nvme/buildkit/*'
   ```
3. Put the label back, and a fresh daemon starts with an empty cache:
   ```sh
   kubectl label node <node> ci-accelerator.io/node-role=build
   ```

### Move from emptyDir to hostPath

1. Create the directory on every build node, on the dedicated disk.
2. Set `buildkit.storage.type=hostPath` and `buildkit.storage.hostPath.path`.
3. Upgrade. Every daemon restarts with an empty cache, one node at a time.
   From then on the router keeps counting a node's builds after its daemon
   is replaced.

### Upgrading from 0.2.x to 0.3.0

0.3.0 split the console into the control plane: chart `control-plane`
(was `console`), image `ghcr.io/hoobiboobi/ci-accelerator` (was
`ci-accelerator-console`), a controller and a web with new names for every
object (`<release>-controller`, `<release>-web`) and value, and the build
router in front of the daemons. Helm refuses to upgrade a 0.2.x release in
place, so install it again:

1. Remove the release and wait for its pods:
   ```sh
   helm uninstall ci-accelerator -n ci-accelerator --wait
   kubectl -n ci-accelerator wait --for=delete pod -l app.kubernetes.io/instance=ci-accelerator --timeout=10m
   ```
   The jobs namespace goes with the release (running jobs fail); the
   daemons' cache on a hostPath stays. With cert-manager, then delete the
   daemons' certificate Secret: it outlives the release, and 0.3.0's names
   the build router (the daemons read their certificate only at start):
   ```sh
   kubectl -n ci-accelerator delete secret ci-accelerator-buildkit-tls
   ```
   Without cert-manager (`buildkit.tls.source=existingSecret`), make the
   daemons' certificate again from the same CA (the same `--out`
   directory, so the clients' certificates stay valid) and replace the
   Secret named in `buildkit.tls.existingSecret` with it:
   ```sh
   scripts/buildkit-certs.sh --name ci-accelerator-buildkit --namespace ci-accelerator \
     --out <your certs directory> --router ci-accelerator
   ```
2. In your values, move everything under `console:` to `control-plane:`.
   The controller's pod settings and volume go under
   `control-plane.controller` (`resources`, `nodeSelector`, `tolerations`,
   `affinity`, `podLabels`, `podAnnotations`, `persistence`), the web's
   under `control-plane.web` (`port`, from `service.port`; the web no
   longer follows the controller's placement). Drop
   `daemons.diskProbePort`: the daemons tell the controller their probe's
   port. An image mirror now mirrors `ghcr.io/hoobiboobi/ci-accelerator`
   (`control-plane.image` and `buildkit.diskProbe.image`). A runner
   cache's `consoleSecretName` is now `controllerSecretName`. A runner of
   your own sets `BUILDKIT_REMOTE_ADDR` to the router,
   `tcp://ci-accelerator-router.ci-accelerator.svc:1234`.
3. Install 0.3.0 with those values. Without cert-manager, issue the
   controller's API certificate from the same CA and name its Secret in
   `control-plane.controller.api.tls.existingSecret` (the script prints the
   `kubectl create secret` command):
   ```sh
   scripts/buildkit-certs.sh --name ci-accelerator-buildkit --namespace ci-accelerator \
     --out <your certs directory> --controller ci-accelerator
   ```
4. The new console starts with an empty history. The old one stays on
   `<release>-console-data`; delete it when you no longer need it, with
   the old client certificate Secret:
   ```sh
   kubectl -n ci-accelerator delete pvc ci-accelerator-console-data
   kubectl -n ci-accelerator delete secret ci-accelerator-console-client --ignore-not-found
   ```
   Port-forwards, an Ingress or a `global.console.url` tunnel point at the
   Service `<release>-web` now (it was `<release>-console`).

On the one-machine install, `quickstart.sh` stops with these steps: run
the `helm uninstall` and `kubectl delete secret` it prints, rename
`console:` to `control-plane:` in `/var/lib/ci-accelerator/values.yaml` (or
delete the file to start over) and run the script again. GitOps: delete
the Application or HelmRelease and create it again with the new values.

### After upgrading from 0.1.0

0.1.0 installed VictoriaMetrics and VictoriaTraces for metrics and traces.
The upgrade removes them, but their volumes stay. Find them and delete
them:

```sh
kubectl -n ci-accelerator get pvc | grep -e victoria-metrics-single -e vt-single
kubectl -n ci-accelerator delete pvc <name> <name>
```

If the daemons still try to send to the removed Services, the upgrade
reused the old endpoint values: upgrade again with
`--reset-then-reuse-values`, or set `buildkit.otel.metricsEndpoint` and
`tracesEndpoint` to `""` or to your own backend.

### Uninstall completely

`helm uninstall` removes the release, including the jobs namespace and
every job pod in it. It leaves:

- **with hostPath, the cache on each build node:** remove it as in step 2
  of [Recover a corrupted state directory](#recover-a-corrupted-state-directory);
- **the controller's volume,** if the control plane was enabled (it is
  kept on purpose, with the console's history; a 0.2.x install also left
  `ci-accelerator-console-data`):
  ```sh
  kubectl -n ci-accelerator delete pvc ci-accelerator-controller-data
  ```
- **the certificate Secrets** (cert-manager keeps them when their
  Certificates are deleted):
  - the CA key in cert-manager's namespace (or in
    `buildkit.tls.certManager.clusterResourceNamespace`), named
    `<box namespace>-<release>-buildkit-ca`; there is none with your own
    `global.tls.issuerRef`. Delete it by name: another box with the same
    release name in another namespace carries the same labels, and
    without its CA key cert-manager makes it a new CA that its existing
    certificates do not chain to.
    ```sh
    kubectl -n cert-manager delete secret ci-accelerator-ci-accelerator-buildkit-ca
    ```
  - the daemons' certificate and the controller's certificates in the
    box namespace:
    ```sh
    kubectl -n ci-accelerator delete secret ci-accelerator-buildkit-tls ci-accelerator-controller-client ci-accelerator-controller-tls
    ```
- **the token Secrets and the box namespace you created.**

Before reinstalling on the same hostPath, wait until the old daemon pods
are gone: a new daemon cannot take the locked state directory.

## Local cluster (kind)

- **Every worker in the example is a tainted build node.** Pods that are
  not build pods need a toleration for the control-plane node: the runner,
  the controller, the web and cert-manager.
- **Every worker has its own zone label** (`zone-a`, `zone-b`, `zone-c`).
  With the box's router and zones on, every job stays on its own node; set
  `control-plane.router.zones=false` to see the router move jobs between
  nodes.
- **A sleeping laptop pauses the Docker VM.** Long "hangs" in tests and a
  cert-manager restart with `clockHealth failed ... out of sync` follow a
  host sleep. Check `pmset -g log | grep -E ' Sleep | Wake '` before
  debugging software.
