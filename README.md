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

Two ways to start; each is a step-by-step page with what every command
should print.

### Try it on one machine

One clean Linux machine (Ubuntu 24.04 or Debian 13, 4 CPUs, 8 GB, 40 GB
of disk) and a runner token from GitLab, in a file:

```sh
curl -fsSLO https://github.com/hoobiboobi/ci-accelerator/releases/download/v0.5.2/quickstart.sh
curl -fsSL https://github.com/hoobiboobi/ci-accelerator/releases/download/v0.5.2/SHA256SUMS | grep quickstart.sh | sha256sum -c -
sudo bash quickstart.sh --gitlab-url https://gitlab.com/ --runner-token-file ./runner-token
```

`quickstart.sh` installs k3s, Helm, cert-manager and the box on that
machine, pinned and checked, and prints the SSH tunnel to the console.
Everything from the runner token to the first build, a GitLab token,
sign-in, upgrades and removal: [docs/quickstart.md](docs/quickstart.md).

### On your cluster

Kubernetes 1.34 or newer, Helm 3.19 or 4, cert-manager, a default
StorageClass, a CNI that enforces NetworkPolicy, and one node to give to
builds. In short:

```sh
kubectl label node <node> ci-accelerator.io/node-role=build
kubectl taint node <node> ci-accelerator.io/node-role=build:NoSchedule
helm install cert-manager oci://quay.io/jetstack/charts/cert-manager --version v1.21.3 \
  --namespace cert-manager --create-namespace --set crds.enabled=true --wait
kubectl create namespace ci-accelerator
kubectl label namespace ci-accelerator pod-security.kubernetes.io/enforce=privileged
kubectl -n ci-accelerator create secret generic gitlab-runner-token \
  --from-literal=runner-token=glrt-... --from-literal=runner-registration-token=
helm install ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.5.2 \
  -n ci-accelerator -f values.yaml --wait --timeout 10m
```

Each step with its checks, the runner in GitLab, `values.yaml` and the
console: [docs/install.md](docs/install.md). A cluster that already has
cert-manager, a GitLab runner or admission policies:
[docs/install-existing-cluster.md](docs/install-existing-cluster.md).
With Flux or Argo CD: [docs/gitops.md](docs/gitops.md). For production
(a dedicated cache disk, images pinned by digest, OTLP):
[docs/production.md](docs/production.md).

### A first build

One job in `.gitlab-ci.yml` builds the project's `Dockerfile` on the
daemons and pushes `$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA`; the install
notes print it with the release's hash:

```yaml
include:
  - remote: https://raw.githubusercontent.com/hoobiboobi/ci-accelerator/v0.5.2/ci/gitlab/build.yml
    integrity: sha256-<from the install notes or the release notes>
    inputs:
      runner-tags: [ci-accelerator]   # the tag you gave the runner
```

What to check before the push, what a cold and a cached run look like,
and where the build is in the console: [docs/first-build.md](docs/first-build.md).
Every input of the component: [ci/gitlab/README.md](ci/gitlab/README.md).

## Upgrading

Every release upgrades in place from the release before it. Read the
release's **Upgrade notes** in [CHANGELOG.md](CHANGELOG.md), then

```sh
helm upgrade ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version <new version> \
  -n ci-accelerator --reset-then-reuse-values --wait --timeout 15m
```

with the new digests when you pin the images. A one-machine install
upgrades by running the new release's `quickstart.sh` again. Each kind
of install step by step, with the checks and the rollback:
[docs/upgrading.md](docs/upgrading.md).

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
| [docs/install.md](docs/install.md) | Install on your cluster, step by step, with what gets installed |
| [docs/install-existing-cluster.md](docs/install-existing-cluster.md) | A cluster that already runs cert-manager, a runner or admission policies |
| [docs/quickstart.md](docs/quickstart.md) | One machine with quickstart.sh: install, console, token, sign-in, upgrade, remove |
| [docs/first-build.md](docs/first-build.md) | The first CI job, a cold and a cached run |
| [docs/production.md](docs/production.md) | A dedicated cache disk, daemon sizes, digests, zones, OTLP, mirrors |
| [docs/console.md](docs/console.md) | The console: its pages, its three levels, sign-in, logs |
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
