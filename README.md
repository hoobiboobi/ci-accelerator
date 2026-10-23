# ci-accelerator

A self-hosted fast CI runner for GitLab, on your own Kubernetes cluster.

You give it a GitLab URL and a runner token. You get a runner whose jobs
build container images on persistent BuildKit daemons with a warm, shared
cache, and a build console that tells why a build was slow or missed the
cache. The daemons can also push their metrics and traces over OTLP to a
backend you already run. Hosted services such as Depot, Namespace or Docker
Build Cloud do this for you; this project does it on hardware you already
run.

> **Status:** v0.1.0 is released: signed charts on ghcr.io, installed as in
> the [Quickstart](#quickstart). `main` also holds unreleased work, listed
> under "Unreleased" in the [changelog](CHANGELOG.md).

## Why

Each CI job usually builds in a fresh container. The layer cache is empty
or has to be pulled, and every job repeats work that the previous one just
did. ci-accelerator keeps the build engine running between jobs instead:

- **The cache stays warm.** BuildKit daemons run on dedicated build nodes
  and keep their cache on local disk across jobs, pipelines and projects.
- **Jobs are thin clients.** A job sends its build context to the daemon
  on its own node and waits. The daemon builds and pushes the image.
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
                                              │  buildctl, mutual TLS
                                              ▼
                     buildkit daemon on the same node ── local cache
                                              │
                                              ├── build history ──► console (optional)
                                              └── OTLP ───────────► your backend: metrics, traces (optional)
```

- **BuildKit daemons:** a DaemonSet on the nodes labelled
  `ci-accelerator.io/node-role=build` (`charts/buildkit`). A Service keeps
  each job on its own node's daemon and falls back to another node when that
  daemon is not ready.
- **The official gitlab-runner chart,** configured so that job pods:
  - run on the build nodes, in their own namespace;
  - get `BUILDKIT_REMOTE_ADDR` and a client certificate (`BUILDKIT_TLS_DIR`);
  - use a small resource tier: the work happens in the daemon.
- **Mutual TLS** between jobs and daemons, issued by cert-manager with a CA
  dedicated to this installation. Without it, any build could drive its own
  daemon.
- **A NetworkPolicy:** daemons accept connections only from the jobs
  namespace, and builds cannot reach cloud instance metadata.
- **Metrics and traces over OTLP (optional):** the daemons push them to
  your own backend (`buildkit.otel.*`). The box stores none of it; build
  times and cache answers are in the console.
- **Runner cache and tiers:** GitLab's `cache:` in any S3-compatible
  bucket, and more runners (tiers) with their own tags, job size,
  placement and cache, each its own pod
  ([docs/runner-cache.md](docs/runner-cache.md)).
- **The console (preview, off by default: `console.enabled=true`):** why
  an image build was slow or not cached, and whether your change or the
  build farm caused it. Five sections: Overview (rebuild reasons ranked by
  the build time they cost), Pipelines (each pipeline's jobs on a time
  axis: how long each waited for a runner and ran), Builds (every image build, filtered by
  branch, outcome, image or your own), Images (cache rate and build time
  per image over 7 days to a year) and Nodes. A build's page starts with one
  sentence, then what changed since the previous build of the image and
  its steps. Every job's log can link to it (`global.console.url`). Jobs
  that build no image stay in GitLab. With
  sign-in through GitLab (`console.signIn`), each person sees the projects
  whose jobs GitLab lets them read; without it, keep it inside the cluster
  (`kubectl port-forward`).

Each part is usable on its own:

- **Only the daemons:** `charts/buildkit`.
- **An existing gitlab-runner:** the values fragment in
  [integrations/gitlab-runner](integrations/gitlab-runner/README.md).

## Quickstart

### Try it on one machine

To look at it before a real deployment, one clean Linux machine is enough:
Ubuntu 24.04 or Debian 13, amd64 or arm64, 4 CPUs, 8 GB of memory and 40
GB of free disk, without Docker or Kubernetes. First create a runner in
GitLab and copy its token (`glrt-...`). Then run:

```sh
curl -fsSLO https://github.com/hoobiboobi/ci-accelerator/releases/download/v0.2.0/quickstart.sh
curl -fsSL https://github.com/hoobiboobi/ci-accelerator/releases/download/v0.2.0/SHA256SUMS | grep quickstart.sh | sha256sum -c -
sudo RUNNER_TOKEN=glrt-... bash quickstart.sh --gitlab-url https://gitlab.example.com/
```

`quickstart.sh` installs the following, each pinned and checked against
its sha256:
- k3s, with the machine as its only node and build node;
- Helm and cert-manager;
- the box, with the build console.

It prints the job to add to `.gitlab-ci.yml` and the SSH tunnel to the
console. `--gitlab-token-file` gives the console a GitLab token with
`read_api`, so it shows how long jobs waited for a runner. `sudo bash
quickstart.sh --uninstall` removes everything again. It is not meant for
production: one node, no high availability, and the console has no sign-in.

### Requirements

- **Kubernetes 1.34 or newer.** Same-node routing (`PreferSameNode`) needs
  kube-proxy 1.34+ or a proxy that implements it.
- **Build nodes:** one or more, labelled and tainted. Daemons and job pods
  run only there. The runner manager (and the console, if enabled) run on
  the other nodes: keep at least one node without the taint (or see
  [troubleshooting](docs/troubleshooting.md#the-runner) for tolerations).

  ```sh
  kubectl label node <node> ci-accelerator.io/node-role=build
  kubectl taint node <node> ci-accelerator.io/node-role=build:NoSchedule
  ```

- **cert-manager:**

  ```sh
  helm install cert-manager oci://quay.io/jetstack/charts/cert-manager --version v1.21.2 \
    --namespace cert-manager --create-namespace --set crds.enabled=true
  ```

- **A default StorageClass,** if you enable the console: it keeps its
  history on a volume (or set `console.persistence.storageClass` or
  `console.persistence.existingClaim`).
- **A CNI that enforces NetworkPolicy** (Calico, Cilium, kindnet, ...).
  Without one, the daemons accept connections from any namespace and builds
  can reach cloud instance metadata; mutual TLS still applies.
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
helm install ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.1.0 \
  -n ci-accelerator \
  --set gitlab-runner.gitlabUrl=https://gitlab.example.com/ \
  --set gitlab-runner.runners.secret=gitlab-runner-token \
  --wait --timeout 10m
```

The package contains its subcharts. From a clone instead: `make deps`, then
`helm install ci-accelerator charts/ci-accelerator ...` with the same
values. The daemons alone: `oci://ghcr.io/hoobiboobi/charts/buildkit`.

- **Defaults:** amd64 build nodes, jobs in namespace `ci-accelerator-jobs`
  (created by the release).
- **arm64 build nodes:** add `--set global.jobs.architecture=arm64`.

The notes printed at the end show how to check that the runner registered.

### 4. A first build

In a project with a `Dockerfile`:

```yaml
# .gitlab-ci.yml
build:
  tags: [my-runner-tag]     # if the runner only takes tagged jobs
  image: moby/buildkit:v0.33.1
  script:
    - buildctl --addr "$BUILDKIT_REMOTE_ADDR" --tlsdir "$BUILDKIT_TLS_DIR"
        build --frontend dockerfile.v0 --local context=. --local dockerfile=.
        --progress plain
```

Add `.git` to `.dockerignore`. Every job has a fresh clone, so with `.git`
in the build context, every step after `COPY . .` runs again in every
pipeline. More in [docs/dockerfile-guide.md](docs/dockerfile-guide.md).

**Pushing an image** is not wrapped yet:

1. Write registry credentials to `$DOCKER_CONFIG/config.json`. For GitLab's
   registry they come from `CI_REGISTRY`, `CI_REGISTRY_USER` and
   `CI_REGISTRY_PASSWORD`.
2. Run `unset DOCKER_AUTH_CONFIG`. buildctl reads that variable too, and a
   read-only entry in it overrides the job's credentials.
3. Add `--output type=image,name=$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA,push=true`.

A reusable CI component that does this is planned.

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

Every release upgrades in place from the previous one. Read its **Upgrade
notes** in [CHANGELOG.md](CHANGELOG.md) first, then:

```sh
helm upgrade ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version <new version> \
  -n ci-accelerator --reset-then-reuse-values --wait --timeout 10m
```

`--reset-then-reuse-values` keeps your values and takes the new version's
defaults for everything else. The end-to-end tests upgrade the previous
release this way before every change is merged
([ADR-0027](docs/adr/0027-upgrades-and-branches.md)).

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

**Security model: one installation is one trust domain.**
- The daemons run as **privileged** pods. Their namespace must allow
  privileged pods.
- **Builds share the cache:** RUN steps can read and write cache mounts
  that other projects' builds left in the same daemon.
- **Builds share the daemon's network.** RUN steps run in the daemon pod's
  network namespace, so they see its localhost and the connections of
  concurrent builds. Their egress is whatever the daemon's NetworkPolicy
  allows. List your cluster CIDRs in
  `buildkit.networkPolicy.egress.clusterCIDRs` to keep builds out of
  in-cluster services. Blocking the API server reliably depends on the CNI.
- **Trace data is untrusted.** Any build (a RUN step) can write spans
  through BuildKit, so traces in your backend are input from builds. The
  buildctl command line, build-arg values included, is recorded in traces:
  **never pass secrets as build args**; use `--secret`.
- **Client certificates:** any certificate from the installation's CA is
  accepted. By default cert-manager lets namespace editors request
  certificates from any ClusterIssuer; on a shared cluster restrict that,
  e.g. with cert-manager approver-policy.
- **Job pods need Pod Security `baseline`.** The runner's job pods do not
  meet `restricted`.

**Cache and routing.**
- **The cache is per daemon.** A project's first build on each build node
  is cold. When a job's own daemon is not ready, the job goes to another
  node's daemon: it succeeds, but with a cold cache. Routing a project to a
  fixed daemon is planned.
- **emptyDir mode:** the cache lives only as long as the pod, so every
  rollout empties it. Use `hostPath` on a dedicated disk in production.
- **What uninstall leaves behind:**
  - with hostPath: the cache on the nodes;
  - the console's volume, if the console was enabled;
  - the CA key in cert-manager's namespace.

**Operations.**
- **Certificate renewal needs a restart.** buildkitd reads its certificate
  only at start: after a renewal (once a year by default) the daemons must
  restart. With Stakater Reloader in the cluster that happens by itself.
- **Probes miss some hangs.** They check that the daemon answers, not that
  builds make progress. Stuck registry pushes can hang all builds on one
  daemon without failing a probe (moby/buildkit#7275).
- **Upstream BuildKit issues that matter here:**
  - cancelling one of several concurrent builds that share a step can
    crash the daemon (moby/buildkit#7253): every build on that node fails
    and the pod restarts. Fixed in BuildKit v0.34.0, which this release does
    not ship yet;
  - a cancelled build can fail another build that shares its steps
    (moby/buildkit#7268);
  - heap retained after builds (moby/buildkit#7181);
  - metadata that grows outside the GC budget (moby/buildkit#7263).
- **Kernel risk on Talos 6.18:** a very large cache with high parallelism
  has led to node hangs (dying cgroups). This is under investigation, not
  solved.

**Scope.**
- **GitLab only.** GitHub Actions support (through Actions Runner
  Controller) is planned.
- **No CI component yet.** Registry pushes are configured by hand (see
  above).

## Testing

Every check is a `make` target. The same targets are meant for CI.

```sh
make lint unittest validate   # no cluster needed: helm lint, helm-unittest, kubeconform (k8s 1.34 and 1.37)
make go-test ui-test          # Go tests; the console's pages in headless Chrome
make kind-up                  # local kind cluster (examples/kind)
make e2e                      # ~13 min end-to-end on kind
```

- **The e2e** installs releases of its own and covers:
  - cache hits and step deduplication;
  - routing;
  - graceful shutdown and crash recovery;
  - GC limits;
  - mutual TLS, including a RUN step that tries to drive its own daemon;
  - the NetworkPolicy;
  - OTLP export;
  - the whole box.
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
| `docs/releasing.md` | How a release is cut and verified |
| `charts/ci-accelerator` | The box (umbrella chart) |
| `charts/buildkit` | The daemons, usable on their own |
| `integrations/gitlab-runner` | Values fragment for an existing gitlab-runner |
| `charts/console` | The console (preview), usable on its own |
| `cmd/console`, `internal/` | Go: the console (build history to builds and reasons, store, collector, pages) |
| `scripts/buildkit-certs.sh` | CA and certificates for `charts/buildkit` without cert-manager (`tls.source=existingSecret`) |
| `docs/architecture.md` | How the parts fit together: build path, cache, certificates, telemetry, rollouts |
| `docs/troubleshooting.md` | Symptoms and fixes, runbooks |
| `docs/dockerfile-guide.md` | Writing Dockerfiles that stay cached |
| `docs/adr` | Architecture decision records: why the charts are the way they are |
| `examples/kind` | Local cluster for development |
| `test/` | End-to-end checks and helpers |

## License

[Apache License 2.0](LICENSE)
