# Contributing

Thanks for looking. Bug reports, questions and pull requests are welcome.

## Reporting

- **Bugs and ideas:** open an issue. Include the chart version, the Kubernetes version and distribution, your values (without secrets), and what `kubectl describe` or the pod logs say. [docs/troubleshooting.md](docs/troubleshooting.md) may already have the answer.
- **Security issues:** do not open a public issue. Use **Security → Report a vulnerability** on GitHub, which reaches the maintainer privately.

## Changes

Development is trunk-based ([ADR-0027](docs/adr/0027-upgrades-and-branches.md)):

- Branch from `main`, keep the change small, and open a pull request. `main` accepts nothing else: the checks and the end-to-end tests must pass, and history stays linear (squash or rebase).
- Larger features arrive in several pull requests, behind a value that is off by default until they are ready.
- Add a line to the **Unreleased** section of [CHANGELOG.md](CHANGELOG.md). If users have to do anything when they upgrade, say so under its **Upgrade notes**.
- Keep upgrades in place working:
  - read new values keys with a default;
  - do not rename objects users refer to;
  - do not restart the daemons without a reason.

  The `upgrade` end-to-end scenario checks this against the last release.

## Checks

Everything runs through `make`. The pinned tools are downloaded and verified into `.tools/`.

```sh
make quick                    # before every push: lint, unittest, go-test and go-lint
make lint unittest validate   # no cluster needed: helm lint, helm-unittest, kubeconform, shellcheck, the workflows (actionlint, zizmor; needs docker)
make go-test go-lint          # Go: gofmt, go vet, go test; golangci-lint (any Go >= 1.21 fetches the pinned toolchain)
make api-check                # the console API's contract (docs/api/openapi.yaml) against the last release's
make ui-test                  # the console's pages in headless Chrome (pinned Node; needs Chrome), ~2 min
make kind-up                  # a local kind cluster with three build nodes
make e2e                      # end-to-end on kind, ~35 min; SCENARIOS="upgrade cache" runs a subset (one run per cluster at a time)
make e2e-copy                 # the same on a copy of the tree taken now: keep editing while it runs
```

Working on the console's pages:

```sh
make console-dev              # the console on its fixtures at 127.0.0.1:8095; templates, CSS and JS are read from internal/web on every request
make ui-shots                 # screenshots of every kind of page at 1400 and 390 px: .ui-shots/TestUI/index.html
```

On GitHub the e2e runs in four parallel groups (one kind cluster each) on pull requests to `main` and on pushes of `wip/*` branches; `test/e2e-matrix.sh` (part of `make validate`) fails when a scenario is in no group.

- **Tests for every change.** A change to a template needs a unit test in `charts/*/tests`. A change in behaviour needs an end-to-end check in `test/e2e/run.sh`. A change to the console's pages or script needs to pass `make ui-test` (test/ui), and an interaction it adds belongs there.
- **Make sure a new check can fail.** Break the code it guards and see it go red before you trust it.

## Repository

| Path | What |
|---|---|
| `CHANGELOG.md` | Changes and upgrade notes of every release |
| `CONTRIBUTING.md` | How to report issues and propose changes |
| `SECURITY.md` | How to report a vulnerability, and what is scanned |
| `docs/install.md`, `docs/console.md`, `docs/limitations.md` | Installing, the console, what it does not do |
| `docs/upgrading.md` | Upgrading an installation: Helm, one machine, GitOps, rollbacks |
| `docs/api/openapi.yaml` | The console API's contract |
| `docs/testing.md` | How the project is tested |
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

## Style

- **Comments, docs and commit messages are in English.**
- **Commit subjects** are short and imperative. The body says why.
- **Decisions** that shape the charts get a short record in [docs/adr](docs/adr/README.md): context, decision, consequences.
- **Pinned versions.** Every version and image is pinned, and new dependencies are pinned the same way (ADR-0015).

By contributing you agree that your work is licensed under the [Apache License 2.0](LICENSE).
