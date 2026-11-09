# Installing on Kubernetes

The commands are in the [README's quickstart](../README.md#quickstart); this
page has what they need, what the box installs, the install options and
where the build cache should live.

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
  v0.34.2, pinned by digest), each with a small disk probe next to it
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
    key ([ADR-0045](adr/0045-build-router.md));
  - the **controller**, one pod (Deployment `<release>-controller`): it
    reads every daemon's build history and the CI job pods into its own
    store (SQLite on a volume), holds the sign-in sessions and every
    GitLab token, and tells the routers where each project built;
  - the **web**, any number of pods (Deployment and Service
    `<release>-web`, two pods by default): the console's pages. It keeps
    nothing and reads the controller's API over HTTPS
    ([ADR-0044](adr/0044-control-plane.md)).
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
  neither the jobs namespace nor the daemons. Neither builds nor job
  scripts reach cloud instance metadata (the jobs namespace has a policy of
  its own: nothing connects into a job pod, and everything but the
  metadata addresses stays reachable from it), and nothing reaches a
  runner tier's pod.
- **Metrics and traces over OTLP (optional):** the daemons push them to
  your own backend (`buildkit.otel.*`). The box stores none of it; build
  times and cache answers are in the console.
- **Runner cache and tiers:** GitLab's `cache:` in any S3-compatible
  bucket, and more runners (tiers) with their own tags, job size,
  placement and cache, each its own pod
  ([runner-cache.md](runner-cache.md)).

Each part is usable on its own:

- **Only the daemons:** `charts/buildkit`
  (`oci://ghcr.io/hoobiboobi/charts/buildkit`).
- **The control plane for daemons installed with `charts/buildkit`:**
  `charts/control-plane` (`oci://ghcr.io/hoobiboobi/charts/control-plane`).
- **An existing gitlab-runner:** the values fragment in
  [integrations/gitlab-runner](../integrations/gitlab-runner/README.md).

## Requirements

- **Kubernetes 1.34 or newer.** Same-node routing (`PreferSameNode`) needs
  kube-proxy 1.34+ or a proxy that implements it.
- **Build nodes:** one or more, labelled and tainted. The daemons, the
  build routers and the job pods run only there. The runner manager and
  the console's controller and web run on the other nodes: keep at least
  one node without the taint (or see
  [troubleshooting](troubleshooting.md#the-runner) for tolerations).
  Nodes in several zones (`topology.kubernetes.io/zone`) keep each job's
  builds in its zone; see [Limitations](limitations.md).

  ```sh
  kubectl label node <node> ci-accelerator.io/node-role=build
  kubectl taint node <node> ci-accelerator.io/node-role=build:NoSchedule
  ```

- **cert-manager,** installed as the [quickstart](../README.md#on-your-cluster)
  shows (or without it: see "Without cert-manager" below).
- **A default StorageClass,** for the controller's volume (or set
  `control-plane.controller.persistence.storageClass` or
  `control-plane.controller.persistence.existingClaim`, or leave the
  control plane out with `control-plane.enabled=false` and
  `global.router.enabled=false`).
- **A CNI that enforces NetworkPolicy** (Calico, Cilium, kindnet, ...).
  Without one, the daemons accept connections from any namespace and builds
  and job scripts can reach cloud instance metadata; mutual TLS still
  applies.
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

## Install options

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
  release: upgrades set the new one (see
  [upgrading.md](upgrading.md#helm-installs)).
- **Without cert-manager:** `buildkit.tls.source=existingSecret` with four
  certificates from one CA that `scripts/buildkit-certs.sh` makes (the
  daemons' with the router's names, the jobs' client, the controller's
  client and the controller API's server certificate); the jobs namespace
  is then yours to create, with the jobs' client Secret in it
  ([gitops.md](gitops.md#certificates-without-cert-manager)).
- **GitOps:** [gitops.md](gitops.md) shows the same install as
  a Flux `HelmRelease` and an Argo CD `Application`.

The notes printed at the end show how to check that the runner registered.

## Where the cache lives

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
- **Sizing** and the reasons for headroom: [troubleshooting.md](troubleshooting.md#disk-inodes-and-processes).

## Without the CI component

Without the component, call buildctl yourself: `buildctl --addr
"$BUILDKIT_REMOTE_ADDR" --tlsdir "$BUILDKIT_TLS_DIR" build ...` in the
`moby/buildkit` image. To push, write the registry credentials to
`$DOCKER_CONFIG/config.json` and run `unset DOCKER_AUTH_CONFIG` first:
buildctl reads that variable too, and a read-only entry in it overrides the
job's credentials.
