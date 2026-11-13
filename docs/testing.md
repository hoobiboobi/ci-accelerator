# How the project is tested

Every check is a `make` target. The same targets are meant for CI.

```sh
make quick                    # before every push: lint, unittest, go-test and go-lint
make lint unittest validate   # no cluster needed: helm lint, helm-unittest, kubeconform (k8s 1.34 and 1.37), the workflows (actionlint, zizmor: needs docker)
make api-check                # the console API's contract against the last release's
make go-test ui-test          # Go tests; the console's pages in headless Chrome
make kind-up                  # local kind cluster (examples/kind)
make e2e                      # end-to-end on kind
make renovate-check pins-check # Renovate sees every pin (docker); every pinned sha256 against upstream
```

- **The e2e** installs releases of its own and covers:
  - cache hits and step deduplication;
  - routing: the Service's same-node routing, and the build router (every
    build node has one, a job's build page says why its node was chosen,
    a job cannot reach a daemon around the router, a router rollout lets
    a running build finish);
  - graceful shutdown and crash recovery;
  - GC limits;
  - mutual TLS, including a RUN step that tries to drive its own daemon;
  - the NetworkPolicy;
  - OTLP export;
  - the whole box, with the console;
  - an in-place upgrade from the last published release, and a rollback.

  On GitHub it runs in four parallel groups, each on its own kind cluster.
  A pull request runs only what its change touches: documentation alone
  runs nothing, Go code the Go checks and the two groups that run the
  control plane, a chart every group
  ([ADR-0049](adr/0049-ci-runs-what-a-change-touches.md)).
- **The console's pages** are checked in headless Chrome: on test data by
  `make ui-test`, and against the real console in the e2e's box scenario.
- **The quickstart** is checked on a clean Ubuntu machine:
  `test/quickstart/run.sh` runs `quickstart.sh`, two CI jobs and the
  console, then uninstalls (`.github/workflows/quickstart.yml`).
  `test/quickstart/offline.sh` (`make quickstart-test`) runs the script in a
  container with stand-ins for k3s and Helm: where the build cache is on
  later runs, the free-disk check, what `--uninstall` removes, what a
  second run keeps and when it stops before changing anything.
- **CI jobs in the e2e** come from a real gitlab-runner. Most scenarios
  talk to a stand-in for the GitLab API; the component scenario runs real
  pipelines in GitLab CE (checkout, pushes with the job token to GitLab's
  registry, builds from a git context, GitLab's comparison of commits in
  the console), and the component is linted by GitLab CE 17.9 and 19.4.
  The box has also been tested by hand against gitlab.com.
- **The project builds itself on its own build farm.** Every branch pushed
  to GitHub goes to the read-only mirror
  [gitlab.com/ci-accelerator/ci-accelerator](https://gitlab.com/ci-accelerator/ci-accelerator)
  (`.github/workflows/mirror.yml`), whose pipeline (`.gitlab-ci.yml`)
  builds the ci-accelerator image with this repository's CI component on a
  ci-accelerator box, next to two jobs that run `go test` and `go vet` as
  builds (`ci/showcase/test.Dockerfile`; the three share their first
  steps), and pushes the image with the job's token to the mirror's
  registry (`registry.gitlab.com/ci-accelerator/ci-accelerator`, `:edge`
  for main, `:<commit>` for every commit). Those are unsigned development
  builds: releases are built, signed and published to ghcr.io by GitHub
  Actions. The commit on GitHub gets the status "ci-accelerator / own
  build farm" with a link to that pipeline; the box's console is public at
  [demo.ci-accelerator.io](https://demo.ci-accelerator.io).
- **Mutation testing:** the checks are broken on purpose (the code they
  guard is changed or removed) to make sure each one can fail.
- **Helm versions:** the checks pass on Helm 3 and Helm 4.
- **Dependencies:** every version is pinned with a checksum or a digest,
  and taken only once it is two weeks old. Renovate opens one pull
  request per subject from this repository's own weekly workflow, with
  the sums recomputed from where each project publishes them
  ([dependencies.md](dependencies.md)).
