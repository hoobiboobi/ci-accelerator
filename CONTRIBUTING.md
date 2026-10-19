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
make kind-up                  # a local kind cluster with three build nodes
make e2e                      # end-to-end on kind, ~15 min; SCENARIOS="upgrade cache" runs a subset
```

- **Tests for every change.** A change to a template needs a unit test in `charts/*/tests`. A change in behaviour needs an end-to-end check in `test/e2e/run.sh`.
- **Make sure a new check can fail.** Break the code it guards and see it go red before you trust it.

## Style

- **Comments, docs and commit messages are in English.**
- **Commit subjects** are short and imperative. The body says why.
- **Decisions** that shape the charts get a short record in [docs/adr](docs/adr/README.md): context, decision, consequences.
- **Pinned versions.** Every version and image is pinned, and new dependencies are pinned the same way (ADR-0015).

By contributing you agree that your work is licensed under the [Apache License 2.0](LICENSE).
