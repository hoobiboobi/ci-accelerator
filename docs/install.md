# Install on your cluster

This page installs ci-accelerator on a Kubernetes cluster from the
beginning to a registered runner and an open console, one step at a time,
with what each step should print. It takes about twenty minutes. Other
starting points:

- a cluster that already runs cert-manager, a GitLab runner or admission
  policies: [install-existing-cluster.md](install-existing-cluster.md),
  which builds on this page;
- one Linux machine to try it on, without a cluster:
  [quickstart.md](quickstart.md);
- Flux or Argo CD: [gitops.md](gitops.md);
- the first CI job after the install: [first-build.md](first-build.md).

What you get: BuildKit daemons on the nodes you choose as build nodes, a
build router in front of them, a GitLab runner whose jobs build on those
daemons, and the console that explains every build
([what gets installed](#what-gets-installed), at the end of this page).

## Before you start

You need these. Each line has the check and what it must show.

| You need | Check | It must show |
|---|---|---|
| Kubernetes 1.34 or newer | `kubectl version` | `Server Version: v1.34.x` or newer |
| Helm 4 (Helm 3.19 still works; Helm 3's security support ended on 2026-11-11) | `helm version` | `v4.x`, or `v3.19.x` (works, no longer receives security fixes) |
| Cluster-admin rights on the cluster | `kubectl auth can-i create clusterroles` | `yes` (a warning that the resource is not namespaced is fine); the install creates a PriorityClass, a ClusterRole and a ClusterIssuer |
| A default StorageClass | `kubectl get storageclass` | one line with `(default)` after its name |
| Two nodes that take ordinary pods | `kubectl get nodes -o wide`, `kubectl describe node <node> \| grep Taints` | one node to give to builds, which nothing else will use, and one more without a `NoSchedule` taint for the runner, the controller and the web (a control-plane node with its default taint does not count). One machine in total: [quickstart.md](quickstart.md) |
| A CNI that enforces NetworkPolicy, ingress and egress | the cluster's documentation | Calico, Cilium, kindnet and most managed clusters do; plain flannel does not |
| The Maintainer role on a GitLab project | the project's Settings menu | it has a CI/CD entry |
| Outbound access from the nodes | | to `ghcr.io`, `quay.io`, `registry.gitlab.com`, `docker.io` and your GitLab |

Without a default StorageClass, the console's controller cannot get its
volume and the install waits until it times out: pick a class from
`kubectl get storageclass` whose provisioner gives block volumes (a
cloud disk, local-path, Longhorn; not NFS or another shared file
system, which the controller's SQLite store cannot use) and keep its
name for step 6. Without a CNI
that enforces NetworkPolicy everything works, but the daemons accept
connections from every namespace and builds can reach cloud instance
metadata; see [limitations.md](limitations.md).

All commands below assume the namespace `ci-accelerator` and the release
name `ci-accelerator`. Keep them: every object name, every later command
and the install notes use them.

## 1. Choose the build node

The daemons, the build router and the CI job pods run only on nodes
labelled as build nodes. Everything else (the runner, the controller, the
web) runs on the other nodes, so at least one node must stay without the
label.

List the nodes and pick one:

```sh
kubectl get nodes -o wide -L kubernetes.io/arch
```

Take the worker with the most CPU and memory. Its root disk holds the
build cache until you give it a dedicated disk
([production.md](production.md#a-dedicated-cache-disk)). The node must be
amd64 unless you set `global.jobs.architecture: arm64` in step 6: the
runner's job pods and the daemons follow the same architecture. Then
label and taint it:

```sh
kubectl label node <node> ci-accelerator.io/node-role=build
kubectl taint node <node> ci-accelerator.io/node-role=build:NoSchedule
```

The taint keeps new pods off the node; pods already running there stay
until they restart. Check:

```sh
kubectl get nodes -l ci-accelerator.io/node-role=build
```

One line: your node. More build nodes later: the same two commands per
node, and the daemons appear there by themselves.

## 2. Install cert-manager

cert-manager issues the certificates for the mutual TLS between the CI
jobs and the daemons. Skip this step if the cluster has it already
(`kubectl get crd certificates.cert-manager.io` finds the CRD). It must
be v1.16 or newer: `kubectl get deploy -A -l
app.kubernetes.io/name=cert-manager -o
jsonpath='{.items[*].spec.template.spec.containers[0].image}'` shows the
image tag; older, or anything else about an existing cert-manager:
[install-existing-cluster.md](install-existing-cluster.md#cert-manager-is-already-installed).

```sh
helm install cert-manager oci://quay.io/jetstack/charts/cert-manager --version v1.21.3 \
  --namespace cert-manager --create-namespace --set crds.enabled=true --wait
```

The command returns after about half a minute, when cert-manager's own
start-up check has seen its webhook answer (`--wait` also waits for the
three pods). Check:

```sh
kubectl -n cert-manager get pods
```

Three pods `1/1 Running`: `cert-manager`, `cert-manager-cainjector` and
`cert-manager-webhook`. cert-manager v1.21.3 is the version the project
tests with; v1.16 or newer works.

## 3. Create the namespace

The daemons are privileged pods, so their namespace must allow them.
Create it and label it:

```sh
kubectl create namespace ci-accelerator
kubectl label namespace ci-accelerator pod-security.kubernetes.io/enforce=privileged
```

Check:

```sh
kubectl get namespace ci-accelerator --show-labels
```

The labels contain `pod-security.kubernetes.io/enforce=privileged`. If an
admission policy of your cluster removes the label or refuses it, see
[install-existing-cluster.md](install-existing-cluster.md#pod-security-is-managed-by-a-policy).

Do not create a second namespace for the jobs: the install creates
`ci-accelerator-jobs` itself, and refuses to adopt one that exists.

## 4. Create the runner in GitLab

The runner registers itself from a token that GitLab shows once when you
create the runner. Create it in the project you will build first (a
project runner serves only that project; a group runner serves every
project of the group, with the same steps under Group > Build > Runners).

1. Open the project in GitLab. In the left sidebar select **Settings >
   CI/CD**, then expand **Runners**.
2. Select **Create project runner** (on older GitLab versions: **New
   project runner**).
3. In **Tags**, enter `ci-accelerator`. Leave **Run untagged jobs**
   unchecked: this runner should take only the build jobs that name the
   tag ([first-build.md](first-build.md) puts it in `runner-tags:
   [ci-accelerator]`), not every untagged job of the project; jobs
   without the tag keep going where they go today (on gitlab.com, the
   shared runners).
4. Leave **Protected** and **Lock to current projects** as they are
   (unchecked). A description is optional.
5. Select **Create runner**.
6. GitLab now shows a page with a token that starts with `glrt-`. Copy it
   somewhere safe: GitLab shows it only this once. Ignore the
   `gitlab-runner register` commands on that page: the install registers
   the runner for you.

Until the install runs, the runner's row under Runners says **Never
contacted**. That is expected.

## 5. Store the token in a Secret

```sh
kubectl -n ci-accelerator create secret generic gitlab-runner-token \
  --from-literal=runner-token=glrt-... \
  --from-literal=runner-registration-token=
```

Replace `glrt-...` with the token from step 4. The second key is
deliberately empty: the runner chart mounts both keys, and a Secret
without the empty one keeps the runner pod from starting. Check:

```sh
kubectl -n ci-accelerator get secret gitlab-runner-token \
  -o go-template='{{range $k, $v := .data}}{{$k}} len={{len $v}}{{"\n"}}{{end}}'
```

```
runner-registration-token len=0
runner-token len=104
```

(The length is of the stored, base64-encoded value: around 100 for a
`glrt-` token.)

The token is now in your shell history. To avoid that, write it to a
file and use `--from-file=runner-token=<file>` instead of the first
`--from-literal`.

## 6. Write the values file

Put the settings in a file, `values.yaml`, and keep it: every later
change and every upgrade starts from it.

```yaml
# values.yaml
gitlab-runner:
  # Your GitLab's address, with the trailing slash. Write it the same way
  # everywhere: the runner, the router and the console compare it
  # character by character.
  gitlabUrl: https://gitlab.com/
  runners:
    # The Secret of step 5.
    secret: gitlab-runner-token

buildkit:
  # The daemon's size. The chart's default (500m CPU, 512Mi) only keeps it
  # out of the kubelet's first evictions; size it to the build node and
  # leave the node headroom. Sizes that worked:
  #   a small node (4-8 CPU, 8-16 GiB):  requests {cpu: "1", memory: 2Gi},  limits {memory: 6Gi}
  #   16-32 CPU, 64 GiB:                 requests {cpu: "8", memory: 16Gi}, limits {memory: 48Gi}
  #   production (48+ CPU, 192+ GiB):    requests {cpu: "48", memory: 128Gi}, limits {memory: 128Gi}
  # The requests must fit the node, or the daemon stays Pending.
  resources:
    requests: {cpu: "1", memory: 2Gi}
    limits: {memory: 6Gi}

# Only without a default StorageClass (see "Before you start"):
# control-plane:
#   controller:
#     persistence:
#       storageClass: <a ReadWriteOnce class>

# Only with arm64 build nodes:
# global:
#   jobs:
#     architecture: arm64
```

The build cache goes to an `emptyDir` on the node's root disk by default,
and is lost when the daemon's pod is replaced (a rollout, an eviction). That is fine for a start; a
dedicated disk comes later without a reinstall
([production.md](production.md#a-dedicated-cache-disk)). Every other
setting and its default: `helm show values
oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.6.0`.

## 7. Install

```sh
helm install ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.6.0 \
  -n ci-accelerator -f values.yaml --wait --timeout 10m
```

The chart checks the cluster before it creates anything (the namespace's
Pod Security label, a build node, cert-manager, the Secret's keys) and
stops with a message that names the fix when a check fails. Then the
nodes pull the images (about 250 MB on the build node) and cert-manager
issues the certificates. With `--wait` the command returns when every pod
is ready, after one to three minutes, and prints:

```
NAME: ci-accelerator
LAST DEPLOYED: ...
NAMESPACE: ci-accelerator
STATUS: deployed
REVISION: 1
NOTES:
ci-accelerator 0.6.0 in namespace ci-accelerator.
...
```

The notes that follow are the install notes: the commands of the next
steps, and the `include:` block of the first build with its hash filled
in. They scroll away; print them again any time with

```sh
helm get notes ci-accelerator -n ci-accelerator
```

If the command fails or times out: `helm status ci-accelerator -n
ci-accelerator` shows the state, `kubectl -n ci-accelerator get pods`
which pod is not ready, and [troubleshooting.md](troubleshooting.md#the-install-fails)
the fixes (a pod `Pending` with a controller volume `Pending` means no
default StorageClass; `ImagePullBackOff` means a node cannot reach
`ghcr.io` or `docker.io`). When one of the chart's own checks stopped the install, nothing was
created: fix what the message names and run `helm install` again. When
`--wait` timed out, the release stays behind as `failed` and a second
`helm install` refuses the name: `helm uninstall ci-accelerator -n
ci-accelerator`, fix the cause, and install again.

## 8. Check the install

Every pod of the box, with one build node:

```sh
kubectl -n ci-accelerator get pods -o wide
```

```
NAME                                            READY   STATUS    NODE
ci-accelerator-buildkit-xxxxx                   2/2     Running   <the build node>
ci-accelerator-controller-xxxxxxxxxx-xxxxx      1/1     Running   <another node>
ci-accelerator-gitlab-runner-xxxxxxxxxx-xxxxx   1/1     Running   <another node>
ci-accelerator-router-xxxxx                     1/1     Running   <the build node>
ci-accelerator-web-xxxxxxxxxx-xxxxx             1/1     Running   <another node>
ci-accelerator-web-xxxxxxxxxx-xxxxx             1/1     Running   <another node>
```

One `buildkit` pod (the daemon and its disk probe, `2/2`) and one
`router` pod per build node; one controller; one runner; two web pods.
Then the certificates and the runner:

```sh
kubectl -n ci-accelerator get certificate
kubectl -n ci-accelerator logs deploy/ci-accelerator-gitlab-runner | grep -i -E 'registered|valid|error'
```

Three certificates, every one `READY True`: `ci-accelerator-buildkit-tls`
(the daemons'), `ci-accelerator-controller-client` and
`ci-accelerator-controller-tls`. The runner's log has the line
`Runner registered successfully`; in GitLab, under Settings > CI/CD >
Runners, the runner's row now shows a green dot and **Online**. If the
log says `is not valid`, the token in the Secret is wrong: re-create the
Secret (step 5), then `kubectl -n ci-accelerator rollout restart
deploy/ci-accelerator-gitlab-runner` (the token is read at start); more
in [troubleshooting.md](troubleshooting.md#the-runner).

## 9. Open the console

You have not set up a sign-in yet ([console.md](console.md#setting-up-the-console)),
so the console is reachable only from inside the cluster. Forward its port to your computer:

```sh
kubectl -n ci-accelerator port-forward svc/ci-accelerator-web 8080
```

It prints `Forwarding from 127.0.0.1:8080 -> 8080` and keeps running:
leave it in this terminal and work in another (Ctrl-C ends it). Then
open <http://localhost:8080/>. The pages are empty until the first
build. At this level the console sees the builds and the job pods, but
not GitLab: a job's result shows as "unknown" and no runner waits are
drawn. A GitLab token or sign-in adds them
([console.md](console.md#setting-up-the-console)).

## Next

- [first-build.md](first-build.md): the CI job that builds your Dockerfile
  on the daemons, and what its first and second run look like.
- [production.md](production.md): a dedicated cache disk, images pinned
  by digest, daemon sizes, metrics and traces, more build nodes.
- [upgrading.md](upgrading.md): the next release, with `values.yaml`.
- Uninstall: `helm uninstall ci-accelerator -n ci-accelerator` removes the
  box and the jobs namespace; the controller's volume
  (`ci-accelerator-controller-data`) and the CA Secret in cert-manager's
  namespace stay, see [troubleshooting.md](troubleshooting.md#uninstall-completely).

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

Outside its namespace the install creates: the jobs namespace
`ci-accelerator-jobs` (Pod Security `baseline`), the daemons'
PriorityClass `ci-accelerator-ci-accelerator-buildkit`, the controller's
ClusterRole and ClusterRoleBinding
`ci-accelerator-ci-accelerator-controller-tokenreview` (for TokenReviews,
its only cluster-wide right), and the CA for this installation in
cert-manager's namespace (an Issuer, a Certificate and its Secret) with a
ClusterIssuer `ci-accelerator-ci-accelerator-buildkit-ca`.

Each part is usable on its own:

- **Only the daemons:** `charts/buildkit`
  (`oci://ghcr.io/hoobiboobi/charts/buildkit`).
- **The control plane for daemons installed with `charts/buildkit`:**
  `charts/control-plane` (`oci://ghcr.io/hoobiboobi/charts/control-plane`).
- **An existing gitlab-runner:** the values fragment in
  [integrations/gitlab-runner](../integrations/gitlab-runner/README.md).
