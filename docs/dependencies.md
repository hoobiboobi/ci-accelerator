# Dependencies: pinned, with their checksums, bumped by Renovate

Every version this repository depends on is pinned: the daemons' BuildKit
and the control plane's Go, the charts the box bundles, the tools the
checks download, the actions the workflows run. A pin is taken only when
it is at least two weeks old, so that a bad release is found by others
first; a security patch of a pinned line is the exception, taken at once
(the changelog says so). Downloads are checked against a sha256 next to
the version, images are pulled by digest.

[Renovate](https://docs.renovatebot.com/) keeps the pins current. It runs
from this repository's own workflow once a week ([renovate.yml](../.github/workflows/renovate.yml)),
as a GitHub App of the repository, so its pull requests get the checks
like anyone's. Its rules are in [renovate.json5](../renovate.json5); what
only the runner may decide is in [renovate-global.js](../.github/renovate-global.js).

## What a Renovate pull request looks like

One pull request per subject, not per file: the Go toolchain is one PR
(go.mod's `toolchain`, the image's `golang` base, the Makefile's download
with its sums); BuildKit is one (the chart's `appVersion` and default
digest, the CI component's client image, the quickstart test's); the
gitlab-runner chart comes with the runner image it runs. The groups are
the `packageRules` of renovate.json5, each with a note in the PR body
saying what to check.

The checksums follow by themselves: after a bump, Renovate runs
`scripts/update-pins.sh <dependency>` in its branch (`postUpgradeTasks`),
which fetches the sums from where each project publishes them - the
release's CHECKSUMS file, nodejs.org's SHASUMS256.txt, go.dev's listing,
get.helm.sh's `.sha256sum` files, a chart layer's digest in its OCI
manifest - and rewrites the Makefile, `test/e2e/run.sh` or
`scripts/quickstart.sh`. ShellCheck publishes no sums: those are the sums
of the archives downloaded then, and the script says so. The same script
moves what names the version besides the pin, comparing the tree with
`HEAD` (Renovate runs it before it commits): the generated chart READMEs,
the unit tests that expect the default BuildKit image or the runner
image, the chart's own copies of BuildKit's default digest, the
cert-manager install command the chart prints (and the README and the
GitOps guide repeat), the Helm path in the README's upgrade commands. Image digests
need no script: Renovate updates a digest with its tag, which is why a
bare digest in the tree is written with its image after it
(`digest: "sha256:..." # moby/buildkit:v0.34.1`).

Renovate opens branches on Monday mornings (Europe/Nicosia) and at most
three pull requests at once; a PR already open is rebased whenever main
moves. Nothing is merged by Renovate.

## What stays by hand, and why

- **kind's node images** (`examples/kind/kind-config.yaml`): each kind
  release lists the images it supports, with their digests; a kind bump
  (one PR with kubectl) says to take them from there.
- **`KUBE_VERSIONS`** in the Makefile (the Kubernetes versions the
  manifests are validated against): the chart's minimum is a promise, the
  newest is what the e2e tests.
- **Helm 3** in the checks' matrix and the Makefile: pinned by hand until
  its end of life (security fixes end 2026-11-11); Helm 4 is Renovate's.
- **The `go` directive** of go.mod (the minimum Go): a compatibility
  promise. The `toolchain` line is bumped.
- **This project's own images and charts** (`ghcr.io/hoobiboobi/...`,
  the e2e's `UPGRADE_FROM`, the chart's list of released images): the
  release workflow's.
- **The runners' Ubuntu** (`runs-on`): a PR of its own, decided with the
  tools' versions on it.

Registries that publish no release dates (ghcr.io, quay.io, gcr.io,
registry.gitlab.com) cannot be held to the two weeks: such a PR carries
the label `age-unverified`, and the reviewer checks the date.

The [Dependency Dashboard](https://github.com/hoobiboobi/ci-accelerator/issues?q=is%3Aissue+Dependencies)
issue lists what Renovate holds back (too new, or waiting for a slot) and
what it skipped.

## Bumping by hand

Change the version where it is annotated (`# renovate: ...` above it, or
the image written after a digest), then:

```sh
scripts/update-pins.sh <dependency>   # the sums, from upstream
scripts/update-pins.sh --check all    # every sum against upstream
```

`<dependency>` is one of `helm-unittest kubeconform shellcheck
gitlab-runner helm kind kubectl oras trivy go node k3s cert-manager
victoria-metrics-single victoria-traces-single component buildkit`. Run it
before committing: the old version it replaces elsewhere is the one `HEAD`
has. For BuildKit it also writes the CI component's integrity and adds the
previous default digest to the chart's list of earlier defaults (or name
it: `buildkit sha256:<previous default digest>`). Prose that names a
version (the README's "What gets installed", the troubleshooting guide's
upstream issues) stays for the reviewer: the pull request says so.

## Checking the setup

```sh
make renovate-check   # the configs validate; every pin is found (docker)
make pins-check       # every sum against upstream (network)
```

`renovate-check` runs Renovate's own image over the tree without a token
(`--platform=local --dry-run=extract`) and compares the pins it finds with
[test/renovate-deps.txt](../test/renovate-deps.txt). A pin that lost its
annotation shows up there; so does a new pin without a line (add it with
`test/renovate-check.sh <image> --write` after looking). The checks
workflow runs both when Renovate's files change.

To see what Renovate would do right now, without a branch: run the
workflow by hand with `dry-run: lookup`, or locally with a token for
github.com's rate limit:

```sh
docker run --rm -v "$PWD":/usr/src/app -w /usr/src/app \
  -e GITHUB_COM_TOKEN="$(gh auth token)" -e LOG_LEVEL=info \
  "$(awk '/^RENOVATE_IMAGE/ {print $3}' Makefile)" --platform=local --dry-run=lookup
```

## The GitHub App

The workflow needs a GitHub App installed on this repository (a
`GITHUB_TOKEN` would not trigger the checks on the pull requests it
opens). The App has, as repository permissions: Contents, Pull requests,
Issues, Workflows and Commit statuses read and write, Metadata read;
nothing else, no organization permissions, no webhook. Its client id and
private key are the repository secrets `RENOVATE_APP_CLIENT_ID` and
`RENOVATE_APP_PRIVATE_KEY`. Commits are made through GitHub's API
as the App (`platformCommit`), so they are signed and verified.
