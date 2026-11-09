# ci-accelerator

[![Live demo: this project's own builds](https://img.shields.io/badge/Live%20demo-this%20project%27s%20own%20builds-1f6feb?style=for-the-badge)](https://demo.ci-accelerator.io)

A self-hosted fast CI runner for GitLab, on your own Kubernetes cluster.

You give it a GitLab URL and a runner token. You get a runner whose jobs
build container images on persistent BuildKit daemons with a warm, shared
cache, a build router that sends each project's builds to the node where
its cache is, and a build console that tells why a build was slow or
missed the cache. The daemons can also push their metrics and traces over
OTLP to a backend you already run. Hosted services such as Depot,
Namespace or Docker Build Cloud do this for you; this project does it on
hardware you already run.

> **Live demo:** [demo.ci-accelerator.io](https://demo.ci-accelerator.io) is the
> console of a ci-accelerator box that builds this repository on every
> push: the image, and the Go tests and vet as builds of their own, through
> its [GitLab mirror](https://gitlab.com/ci-accelerator/ci-accelerator/-/pipelines).
> Open a running job to watch its build live.

> **Status:** v0.5.2 is released: signed charts and the ci-accelerator image on
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

- **BuildKit daemons** on your build nodes (`charts/buildkit`, BuildKit
  v0.34.2, pinned by digest), each with a disk probe for the console.
- **The control plane** (`charts/control-plane`, image
  `ghcr.io/hoobiboobi/ci-accelerator`): the build router on every build
  node, the controller (one pod, its own store) and the web (the console).
- **The official gitlab-runner chart,** whose jobs run on the build nodes
  in their own namespace and reach the daemons through the router.
- **Mutual TLS** between jobs and daemons (cert-manager, a CA of the
  installation's own) and **NetworkPolicies** around every part.
- Optional: **OTLP** metrics and traces to your backend, the **runners' S3
  cache** and **runner tiers**.

Each part is usable on its own: only the daemons (`charts/buildkit`), the
control plane for them (`charts/control-plane`), or an existing
gitlab-runner with [a values fragment](integrations/gitlab-runner/README.md).
Every part in detail: [docs/install.md](docs/install.md).

### The console

The console tells why an image build was slow or not cached, and whether
your change or the build farm caused it. Every build gets one of five
outcomes (from cache, your change, build farm, unclear, failed) and a page
that says what changed since the previous build of its image, which step
ran first and why, what its steps used, and why the router chose its node.
Its sections:

- **Pipelines** (the home page): each pipeline's jobs on a time axis, with
  runner waits; a running job's build follows live.
- **Insights:** why images were rebuilt, ranked by the time it cost.
- **Builds** and **Images:** every build, and cache rate and build time per
  image; any two builds of an image compare side by side.
- **Nodes:** the daemons' disk, cache, load and the router's decisions.

On a build's page, **speed-ups**: steps that run a package manager without
a working cache mount get a ready Dockerfile fragment, with measured times.

Every figure is measured, not estimated. The console works by itself, with
a GitLab token, or with sign-in through GitLab:
[docs/console.md](docs/console.md). Its JSON API is described in
[docs/api/openapi.yaml](docs/api/openapi.yaml).

## Quickstart

### Try it on one machine

To look at it before a real deployment, one clean Linux machine is enough:
Ubuntu 24.04 or Debian 13, amd64 or arm64, 4 CPUs, 8 GB of memory and 40
GB of free disk, without Docker or Kubernetes. First create a runner in
GitLab and copy its token (`glrt-...`): the script asks for it, or reads
it from a file (`--runner-token-file`), so it never sits on a command line
where other users and the shell history see it. Then run:

```sh
curl -fsSLO https://github.com/hoobiboobi/ci-accelerator/releases/download/v0.5.2/quickstart.sh
curl -fsSL https://github.com/hoobiboobi/ci-accelerator/releases/download/v0.5.2/SHA256SUMS | grep quickstart.sh | sha256sum -c -
sudo bash quickstart.sh --gitlab-url https://gitlab.example.com/
```

`quickstart.sh` installs the following; k3s, Helm, cert-manager and the
chart are pinned and checked against their sha256 (the box's images come
by tag, the daemons' by digest):
- k3s v1.36.5+k3s1, with the machine as its only node and build node;
- Helm v4.3.1 and cert-manager v1.21.3;
- the box, with the build router and the console. The daemon keeps its
  cache in `/var/lib/ci-accelerator/buildkit` (a hostPath, so it survives
  restarts; `--build-dir` on the first install puts it elsewhere). Its
  resources are sized from the machine on the first install (half its CPUs
  and a quarter of its memory requested, three quarters of the memory at
  most) and kept, with the cache's path, in
  `/var/lib/ci-accelerator/values.yaml`, which later runs leave alone.

It prints a job to add to `.gitlab-ci.yml` and the SSH tunnel to the
console. The console starts in its first level (builds only, no GitLab
access; see [the console's levels](docs/console.md#setting-up-the-console)):
`--gitlab-token-file` adds job results and runner waits, and sign-in
through GitLab works on this box too. To upgrade, run a later release's
`quickstart.sh` the same way
([docs/upgrading.md](docs/upgrading.md#one-machine-installs-quickstartsh)).
`sudo bash quickstart.sh --uninstall` removes everything again. It is not meant for production: one node, no high
availability.

To change its values later (`/var/lib/ci-accelerator/values.yaml`), apply
them with the Helm the script installed:

```sh
sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml /usr/local/lib/ci-accelerator/helm-v4.3.1/helm upgrade ci-accelerator \
  oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.5.2 \
  -n ci-accelerator -f /var/lib/ci-accelerator/values.yaml --wait --timeout 15m
```

### On your cluster

Kubernetes 1.34 or newer with at least one build node, cert-manager, a
default StorageClass and a CNI that enforces NetworkPolicy; the full list
and every option: [docs/install.md](docs/install.md).

```sh
# Build nodes: the daemons, the routers and the job pods run only there.
kubectl label node <node> ci-accelerator.io/node-role=build
kubectl taint node <node> ci-accelerator.io/node-role=build:NoSchedule

helm install cert-manager oci://quay.io/jetstack/charts/cert-manager --version v1.21.3 \
  --namespace cert-manager --create-namespace --set crds.enabled=true

# The daemons are privileged pods, so their namespace must allow them.
kubectl create namespace ci-accelerator
kubectl label namespace ci-accelerator pod-security.kubernetes.io/enforce=privileged

# A runner token from GitLab (project or group > Settings > CI/CD > Runners >
# New runner; choose tags, or allow untagged jobs). The chart reads both
# keys, so the empty one must be there too.
kubectl -n ci-accelerator create secret generic gitlab-runner-token \
  --from-literal=runner-token=glrt-... --from-literal=runner-registration-token=

helm install ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.5.2 \
  -n ci-accelerator \
  --set gitlab-runner.gitlabUrl=https://gitlab.example.com/ \
  --set gitlab-runner.runners.secret=gitlab-runner-token \
  --wait --timeout 10m
```

The notes printed at the end show how to check that the runner registered.
For production, give the daemons a dedicated disk
([where the cache lives](docs/install.md#where-the-cache-lives)) and pin the
ci-accelerator images by digest
([install options](docs/install.md#install-options)); with Flux or Argo CD:
[docs/gitops.md](docs/gitops.md).

### A first build

In a project with a `Dockerfile`, include the GitLab CI component: one job
that builds on the build farm and pushes `$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA`
with the job token. The release notes print this block with the file's
hash:

```yaml
# .gitlab-ci.yml
include:
  - remote: https://raw.githubusercontent.com/hoobiboobi/ci-accelerator/v0.5.2/ci/gitlab/build.yml
    integrity: sha256-<from the release notes>
    inputs:
      runner-tags: [my-runner-tag]   # if the runner only takes tagged jobs
```

Build arguments, secrets, several images, another Dockerfile, the digest
for later jobs, GitLab without access to GitHub, and large repositories
built without a checkout (`context-source: git`):
[ci/gitlab/README.md](ci/gitlab/README.md).

Add `.git` to `.dockerignore` (with the default context, the job's own
files). Every job has a fresh clone, so with `.git` in the build context,
every step after `COPY . .` runs again in every pipeline. More in [docs/dockerfile-guide.md](docs/dockerfile-guide.md).

Without the component, call buildctl yourself: `buildctl --addr
"$BUILDKIT_REMOTE_ADDR" --tlsdir "$BUILDKIT_TLS_DIR" build ...` in the
`moby/buildkit` image. To push, write the registry credentials to
`$DOCKER_CONFIG/config.json` and run `unset DOCKER_AUTH_CONFIG` first:
buildctl reads that variable too, and a read-only entry in it overrides the
job's credentials.

## Upgrading

Every release upgrades in place from the release before it (a 0.2.x
install reaches 0.3.0 or later by installing again). Read the release's
**Upgrade notes** in [CHANGELOG.md](CHANGELOG.md) first, then:

```sh
helm upgrade ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version <new version> \
  -n ci-accelerator --reset-then-reuse-values --wait --timeout 15m
```

A one-machine install upgrades by running the new release's
`quickstart.sh` again. Both ways step by step, with pinned digests, GitOps,
checks and rollbacks: [docs/upgrading.md](docs/upgrading.md).

## Limitations

Known, deliberate or not yet solved; the full list, with the reasons:
[docs/limitations.md](docs/limitations.md). The ones to know first:

- **One installation is one trust domain.** The daemons are privileged,
  builds share their cache (and cache mounts) and their network, and a
  fetched commit is readable without a token. Use one installation per
  team that must not see another's builds.
- **Without sign-in the console is open** to whoever reaches its Service,
  with a shared GitLab token if one is set. Keep it inside the cluster, or
  turn on sign-in through GitLab.
- **The cache is per daemon.** The router sends a project to the node where
  it built; a project new to a node starts cold there.
- **Probes miss some hangs,** and a renewed certificate needs a restart.
- **One controller** (SQLite on a ReadWriteOnce volume) and **GitLab only**.

## Documentation

| Page | What |
|---|---|
| [docs/install.md](docs/install.md) | What gets installed, requirements, install options, where the cache lives |
| [docs/console.md](docs/console.md) | The console: its pages, its three levels, sign-in, logs, OTLP |
| [docs/upgrading.md](docs/upgrading.md) | Upgrading: Helm, one machine, GitOps, rollbacks |
| [docs/limitations.md](docs/limitations.md) | What it does not do, and the security model |
| [docs/architecture.md](docs/architecture.md) | How the parts fit together |
| [docs/troubleshooting.md](docs/troubleshooting.md) | Symptoms and fixes |
| [docs/dockerfile-guide.md](docs/dockerfile-guide.md) | Dockerfiles that stay cached |
| [docs/runner-cache.md](docs/runner-cache.md) | The runners' S3 cache and runner tiers |
| [docs/gitops.md](docs/gitops.md) | Installing with Flux or Argo CD |
| [ci/gitlab/README.md](ci/gitlab/README.md) | The GitLab CI component |
| [docs/api/openapi.yaml](docs/api/openapi.yaml) | The console's JSON API |
| [docs/testing.md](docs/testing.md) | How the project is tested |
| [docs/adr](docs/adr/README.md) | Why it is the way it is |
| [CHANGELOG.md](CHANGELOG.md) | Every release, with upgrade notes |

Contributing: [CONTRIBUTING.md](CONTRIBUTING.md); reporting a vulnerability:
[SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE)
