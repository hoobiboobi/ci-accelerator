# Production settings

[install.md](install.md) gets a box running with the defaults: the build
cache in an `emptyDir`, images by tag, one build node. This page changes
each of those for a production install, one setting at a time, with the
values to add to `values.yaml`, the command that applies them and the
check. Every section stands on its own; apply one or all. The values
file at the end has them together.

Apply a change with the same command every time:

```sh
helm upgrade ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.6.0 \
  -n ci-accelerator -f values.yaml --wait --timeout 15m
```

A change under `buildkit:` restarts the daemons, one node at a time; a
daemon waits for its running builds first (up to
`buildkit.terminationGracePeriodSeconds`, 300 s), so pick a quiet moment
and give `--timeout` nodes × that. On the one-machine install, Helm is
the script's ([quickstart.md](quickstart.md#9-change-the-values)).

## A dedicated cache disk

By default each daemon keeps its cache in an `emptyDir` on the node's
root disk: every daemon restart starts with an empty cache, and the cache
competes with everything else on that disk. For production, give each
build node a disk of its own and point the daemon at it.

**1. Prepare the disk on every build node** (as root on the node). XFS:
the cache is millions of small files, and XFS allocates inodes as needed
where ext4 fixes their number at `mkfs` time and can run out of them
(`df -i`) long before it runs out of space; XFS also keeps no 5 % root
reserve that buildkitd's free-space check would count. The production
numbers below come from XFS.

`/dev/nvme1n1` below stands for the empty disk: `lsblk -o
NAME,SIZE,FSTYPE,MOUNTPOINT` lists the disks; take the one with no
filesystem and no mount point (`mkfs` on the wrong one destroys the
node).

```sh
mkfs.xfs -L buildkit /dev/nvme1n1            # ftype=1 (overlayfs needs it) is the default
mkdir -p /var/mnt/nvme && mount -o noatime /dev/nvme1n1 /var/mnt/nvme
mkdir -p /var/mnt/nvme/buildkit              # the directory the daemon uses
```

Add the mount to `/etc/fstab` (or your node image) so it comes back
after a reboot. Check: `findmnt /var/mnt/nvme` prints one line with
`xfs`, and `df -h /var/mnt/nvme` shows the disk's size. With ext4 instead, make more inodes (`mkfs.ext4 -i 8192`)
and drop the reserve (`tune2fs -m 0`).

**2. The values.** The directory must exist on the mounted disk: with
`type: Directory` (the default) a daemon whose disk is not mounted does
not start, instead of filling the OS disk.

```yaml
buildkit:
  storage:
    type: hostPath
    hostPath:
      path: /var/mnt/nvme/buildkit
  config:
    gc:                              # absolute sizes on a dedicated disk; binary units: "2TB" = 2 TiB
      maxUsedSpace: "2TB"            # the cache's budget
      reservedSpace: "100GB"         # never collected below this
      minFreeSpace: "400GB"          # collect when the disk has less free than this
```

Size the three to the disk: on a 3 TB NVMe (about 2.7 TiB) the example
above leaves headroom for what the garbage collector does not count
(running builds, BuildKit's databases, one of which grows without bound:
moby/buildkit#7263). On a 1 TB disk: `maxUsedSpace: "700GB"`,
`reservedSpace: "50GB"`, `minFreeSpace: "150GB"`. More in
[troubleshooting.md](troubleshooting.md#disk-inodes-and-processes).

**3. Apply and check.** The daemons restart once. Then:

```sh
kubectl -n ci-accelerator get pods -l app.kubernetes.io/name=buildkit -o wide
```

Every daemon `2/2 Running`. A daemon in `ContainerCreating` with the
event `MountVolume.SetUp failed ... hostPath type check failed` has no
directory on its node: create it and the pod starts by itself. The
console's Nodes page now shows each node's cache disk with its size and
free space. The cache stays after `helm uninstall`; one release per path
per node.

Moving an existing install from `emptyDir` to `hostPath` is the same
change; the old cache is gone with the restart
([troubleshooting.md](troubleshooting.md#move-from-emptydir-to-hostpath)).

## The daemon's size

The chart's default (500m CPU, 512Mi requested, no limit) only keeps the
daemon out of the kubelet's first evictions. Size it to the build node,
and leave the node headroom for the kubelet, containerd, the router and
the job pods. A hard CPU limit throttles builds, so set none; a memory
limit applies to buildkitd and all RUN steps together, and an OOM kill
takes the daemon with every running build.

```yaml
buildkit:
  resources:
    #   a small node (4-8 CPU, 8-16 GiB):  requests {cpu: "1", memory: 2Gi},  limits {memory: 6Gi}
    #   16-32 CPU, 64 GiB:                 requests {cpu: "8", memory: 16Gi}, limits {memory: 48Gi}
    #   production (48+ CPU, 192+ GiB):    requests {cpu: "48", memory: 128Gi}, limits {memory: 128Gi}
    requests: {cpu: "8", memory: 16Gi}
    limits: {memory: 48Gi}
  config:
    maxParallelism: 28       # concurrent build steps per daemon (the production value on 48 cores)
```

A namespace `LimitRange` can add a memory limit the daemon does not ask
for; check `kubectl -n ci-accelerator get limitrange`. After the change,
the Nodes page of the console draws the daemon's CPU and memory against
the node's.

## Images pinned by digest

The daemons' BuildKit image is pinned by digest by the chart. The box's
own image, `ghcr.io/hoobiboobi/ci-accelerator`, runs the controller, the
web and the router at the release's tag, and the disk probe next to each
daemon at the tag of the release that last changed it. Pin both by their
digests, which every release's notes list under **Digests**
(<https://github.com/hoobiboobi/ci-accelerator/releases/tag/v0.6.0>, the
paragraph "To pin the box's images by digest"):

```yaml
control-plane:
  image:
    digest: "sha256:<from the release notes>"   # ci-accelerator:v0.6.0
buildkit:
  diskProbe:
    image:
      digest: "sha256:8103f17d6512ed305f445fc436dfe19cf9669d57745ae95f2cc98756f39b1204"   # ci-accelerator:v0.3.3, the probe
```

Check: the pods run by digest.

```sh
kubectl -n ci-accelerator get deploy ci-accelerator-controller -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
```

prints `ghcr.io/hoobiboobi/ci-accelerator:v0.6.0@sha256:5071488b...`. A
digest belongs to one release: an upgrade sets the new one from the new
release's notes in the same `helm upgrade`, and the chart refuses a kept
digest of the release before with a message that names the right one
([upgrading.md](upgrading.md#helm-installs)).

## More build nodes, zones, arm64

**More nodes:** label and taint each one as in
[install.md](install.md#1-choose-the-build-node); the daemons and the
routers appear there by themselves, each with its own cache. The router
keeps a project on the node where it built in the last 7 days
(`control-plane.router.warmDays`), so a project's cache warms up once.

**Zones:** with build nodes in several zones
(`topology.kubernetes.io/zone`; check with `kubectl get nodes -L
topology.kubernetes.io/zone`), the router keeps each job in its own zone
by default (`control-plane.router.zones: true`): a project warms up once
per zone, and a zone's jobs never pay for cross-zone traffic. A job in a
zone without a ready daemon goes to another zone
(`control-plane.router.crossZone: true`). Set `zones: false` when the
zones are cheap to cross or every zone has one build node (then every
job stays on its node anyway).

**arm64 build nodes:** the daemons, the runner's job pods and the
runner's helper image follow one architecture, set once:

```yaml
global:
  jobs:
    architecture: arm64
```

A mixed farm (amd64 and arm64 build nodes in one install) is not
supported; two installs, one per architecture, are untested.

## Metrics and traces to your backend

The box keeps no metrics or traces. The daemons push both over OTLP to a
backend you run, such as an OpenTelemetry Collector:

```yaml
buildkit:
  otel:
    metricsEndpoint: http://otel-collector.observability.svc:4318/v1/metrics
    tracesEndpoint: http://otel-collector.observability.svc:4318/v1/traces
    protocol: http/protobuf   # the default; with grpc the endpoints are host:4317 without a path
```

Every metric and span names its daemon (`k8s.pod.name`,
`k8s.node.name`); BuildKit's counters are deltas per push interval
(`metricsIntervalSeconds`, 60), so sum them over time or convert them in
the collector. Every job has its own trace: the runner sets
`TRACEPARENT` before the job's script, with `ci.pipeline.id`,
`ci.job.id` and `ci.project.path` on every span. Check: the collector's
own log or metrics show received points within two minutes of the
daemons' restart; the daemons' NetworkPolicy allows egress to the collector unless its
address falls inside `buildkit.networkPolicy.egress.clusterCIDRs` (empty
by default); with the cluster's CIDRs listed there, add an egress rule
for the collector in `buildkit.networkPolicy.egress.extraRules`.

## The console for a team

The console's Service, `ci-accelerator-web`, is a ClusterIP on port
8080, plain HTTP, and without sign-in it shows every build to whoever
reaches it. For a team: an Ingress or gateway with TLS in front of the
Service, `global.console.url` set to that address, and sign-in through
GitLab, so each person sees the projects GitLab lets them read. The
whole procedure: [console.md](console.md#setting-up-the-console). Until
then, keep it inside the cluster and reach it with `kubectl
port-forward`.

## Private registries and mirrors

Build nodes pull base images from where the Dockerfile says, anonymously
unless the job logs in (the component logs in to GitLab's registry and
Dependency Proxy). Docker Hub's rate limit applies per node address. A
mirror for Docker Hub goes into the daemons' configuration:

```yaml
buildkit:
  config:
    extraConfig: |
      [registry."docker.io"]
        mirrors = ["mirror.example.com"]
```

To run the box from your own registry, mirror `moby/buildkit` (by digest,
`buildkit.image`), `ghcr.io/hoobiboobi/ci-accelerator` at both tags (the
release's and the disk probe's, `control-plane.image` and
`buildkit.diskProbe.image`), the gitlab-runner images
(`gitlab-runner.image`, `gitlab-runner.runners.config` for the helper)
and the component's `buildkit-image` input for the job pods.

## Certificates without cert-manager

cert-manager is the default and the tested path. A cluster without it
needs four certificates from one CA dedicated to the box, which
`scripts/buildkit-certs.sh` makes; the procedure, the Secrets and the
values are in [gitops.md](gitops.md#certificates-without-cert-manager).
Renewal is yours then: the script issues certificates for 365 days,
and the daemons read theirs only at start, so make new ones before
the date and restart the daemons after replacing the Secret.

## NetworkPolicy and what the install needs from the cluster

The box installs NetworkPolicies around every part ([install.md](install.md#what-gets-installed)
lists them). They need a CNI that enforces policies, egress included;
without one everything runs, but the daemons accept connections from
every namespace and job scripts reach cloud instance metadata. An Ingress
controller on the host network needs its node addresses in
`control-plane.networkPolicy.extraFrom` to reach the web. Cluster-scoped
objects, cert-manager's namespace and the installer's rights:
[install-existing-cluster.md](install-existing-cluster.md).

## A production values file

The last two keys need what other pages set up first: the Secret
`gitlab-api-token` ([console.md](console.md#level-2-with-a-gitlab-token),
step 2; the controller does not start without it) and an Ingress that
answers at `global.console.url` ([console.md](console.md#level-3-with-sign-in-through-gitlab-for-a-team)).
Leave both out until then.

```yaml
# values.yaml
gitlab-runner:
  gitlabUrl: https://gitlab.example.com/
  runners:
    secret: gitlab-runner-token

global:
  console:
    url: https://ci-console.example.com      # only once an Ingress answers here (console.md); every job's log links to it

buildkit:
  storage:
    type: hostPath
    hostPath:
      path: /var/mnt/nvme/buildkit
  config:
    gc: { maxUsedSpace: "2TB", reservedSpace: "100GB", minFreeSpace: "400GB" }
    maxParallelism: 28
  resources:
    requests: {cpu: "48", memory: 128Gi}
    limits: {memory: 128Gi}
  diskProbe:
    image:
      digest: "sha256:8103f17d6512ed305f445fc436dfe19cf9669d57745ae95f2cc98756f39b1204"   # ci-accelerator:v0.3.3
  otel:
    metricsEndpoint: http://otel-collector.observability.svc:4318/v1/metrics
    tracesEndpoint: http://otel-collector.observability.svc:4318/v1/traces

control-plane:
  image:
    digest: "sha256:<from the release notes>"   # ci-accelerator:v0.6.0
  gitlab:
    token:
      existingSecret: gitlab-api-token       # the Secret of console.md level 2, created first; or signIn
```

Every other setting and its default: `helm show values
oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.6.0`.
