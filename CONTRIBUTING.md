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
make lint unittest validate   # no cluster needed: helm lint, helm-unittest, kubeconform, shellcheck
make go-test                  # Go: gofmt, go vet, go test (any Go >= 1.21 fetches the pinned toolchain)
make ui-test                  # the console's pages in headless Chrome (pinned Node; needs Chrome), ~2 min
make kind-up                  # a local kind cluster with three build nodes
make e2e                      # end-to-end on kind, ~15-25 min; SCENARIOS="upgrade cache" runs a subset
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

## Style

- **Comments, docs and commit messages are in English.**
- **Commit subjects** are short and imperative. The body says why.
- **Decisions** that shape the charts get a short record in [docs/adr](docs/adr/README.md): context, decision, consequences.
- **Pinned versions.** Every version and image is pinned, and new dependencies are pinned the same way (ADR-0015).

By contributing you agree that your work is licensed under the [Apache License 2.0](LICENSE).
