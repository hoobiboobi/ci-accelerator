# Releasing

How a version gets from `main` to users. The rules behind it (patch vs minor, upgrade compatibility, branches) are in [ADR-0027](adr/0027-upgrades-and-branches.md); publishing is automated by `.github/workflows/release.yml` ([ADR-0025](adr/0025-releases.md)).

`<v>` below is the new version without the `v` (`0.2.0`), `<prev>` the last release.

## 1. Decide

- **Patch or minor?** A patch is fixes only. A minor brings something new that is ready and on by default.
- **Is `main` releasable?** Unfinished work must be switched off by default. If `main` already holds changes that should not ship in a patch, release the patch from a branch instead (see [A patch from a release branch](#a-patch-from-a-release-branch)).
- **Pinned components.** Look at the releases and security advisories of everything that is pinned: BuildKit, cert-manager, the gitlab-runner chart and image, Go and the console's base images (`cmd/console/Dockerfile`), Node, the kind node image, the e2e images (`test/e2e/run.sh`), the tools in the Makefile, and the actions in the workflows. Bump what has a fix and is at least two weeks old - except a security patch (see below).
- **Security patches skip the two-weeks wait** ([ADR-0041](adr/0041-security-patch-exception.md)): a patch release of the same minor line that fixes a vulnerability which affects the project (govulncheck, trivy, Dependabot or the advisory says so) may be pinned at once, provided the full CI passes; the changelog names the exception. A new minor waits the two weeks even when it carries security fixes.

## 2. The release pull request

On a branch `release/v<v>`:

1. Set the version in eight places:
   - `version:` in `charts/buildkit/Chart.yaml`;
   - `version:` and `appVersion:` in `charts/console/Chart.yaml` (the appVersion is the console image's tag, `v<appVersion>`);
   - `version:` and `appVersion:` in `charts/ci-accelerator/Chart.yaml` (the box's version is its own, not BuildKit's);
   - the `buildkit` and `console` dependencies' `version:` in `charts/ci-accelerator/Chart.yaml`;
   - `VERSION=` in `scripts/quickstart.sh` (the one-node installer installs it; the release attaches the script);
   - the README's quickstart download links and `helm install ... --version`;
   - the console image tag in `artifacthub.io/images` of `charts/console/Chart.yaml` and `charts/ci-accelerator/Chart.yaml`, and the release's `artifacthub.io/changes` in the umbrella's `Chart.yaml` (Artifact Hub shows them on the version page);
   - the disk probe's image tag (`diskProbe.image.tag` in `charts/buildkit/values.yaml` and the default in `buildkit.diskProbeImage`) only when `internal/diskprobe` changed since that tag: a new tag restarts every daemon on upgrade, so it needs an upgrade note and `UPGRADE_ROLLOUT_EXPECTED=1`;
   - `make chart-docs` regenerates the charts' READMEs (the packaged README is what Artifact Hub shows; `make lint` fails when they are stale).

   Also update the status line and the `--version` of the quickstart in `README.md`.
2. In `CHANGELOG.md`:
   - rename `## Unreleased` to `## <v> - <yyyy-mm-dd>`;
   - read its **Upgrade notes** as a user would, and make sure they say what to do;
   - start a new `## Unreleased` above it with `### Upgrade notes` and `None.`.
3. Run `scripts/check-release-version.sh v<v>`. It checks the versions and the changelog entry, and prints the version.
4. Run `make vulncheck` and look at `.govulncheck-allow` and `.trivyignore.yaml`: a release should not ship with a grace period that a bump can end.
5. Open the pull request. The checks and the e2e (including `upgrade` from `<prev>`) must be green. Merge it.

## 3. Optional: a release candidate

To rehearse a minor exactly as users will get it, tag the merged commit `v<v>-rc.1` and push the tag (see step 4). The workflow publishes a pre-release. Install it from the registry on a test cluster. Release candidates use the `<v>` changelog entry.

## 4. Tag

```sh
git switch main && git pull --ff-only
git tag -a v<v> -m v<v>
git push origin v<v>
```

Tags `v*` cannot be moved or deleted once pushed. The workflow:
- runs the checks again;
- packages the three charts (ci-accelerator, buildkit, console) reproducibly and pushes them to `oci://ghcr.io/hoobiboobi/charts`;
- builds the console image for amd64 and arm64 (`scripts/build-console-image.sh`, the same build the checks run on every pull request) and pushes it as `ghcr.io/hoobiboobi/ci-accelerator-console:v<v>`;
- signs all four with cosign;
- creates the GitHub release with the changelog entry, the archives, `SHA256SUMS` and the digests.

## 5. Verify as a user

Without registry credentials, with cosign 2.6 or newer (the signatures are
Sigstore bundles stored as OCI referrers; older versions answer "no
signatures found"):

```sh
export HELM_REGISTRY_CONFIG=$(mktemp -d)/registry.json
helm pull oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version <v>
helm pull oci://ghcr.io/hoobiboobi/charts/buildkit --version <v>
helm pull oci://ghcr.io/hoobiboobi/charts/console --version <v>
sha256sum ci-accelerator-<v>.tgz buildkit-<v>.tgz console-<v>.tgz   # compare with SHA256SUMS of the release
for ref in charts/buildkit:<v> charts/console:<v> charts/ci-accelerator:<v> ci-accelerator-console:v<v>; do
  cosign verify ghcr.io/hoobiboobi/$ref \
    --certificate-identity https://github.com/hoobiboobi/ci-accelerator/.github/workflows/release.yml@refs/tags/v<v> \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
done
docker pull ghcr.io/hoobiboobi/ci-accelerator-console:v<v>
```

About half an hour later [Artifact Hub](https://artifacthub.io/packages/helm/ci-accelerator/ci-accelerator) lists the version with the Signed badge and the verified publisher mark. It checks the signature once, when it first indexes the version (every half hour), and never again for the same digest; the workflow therefore signs right after the push. A version indexed inside that window stays unsigned there for good (0.2.1 did; `cosign verify` is what counts).

Then check the release the way a user installs it on one machine: run the Quickstart workflow by hand with the release's version (`gh workflow run quickstart.yml -f release=<v>`). It downloads `quickstart.sh` from the release, checks it against `SHA256SUMS` and installs from the registry on a clean runner.

A package pushed for the first time is private: make it public under the repository's Packages (`charts/console` and `ci-accelerator-console` came with 0.2.0). Then install the umbrella from the registry on a kind cluster with `console.enabled=true` and run a build, as the README quickstart does.

## 6. After the release

Open a pull request that moves the upgrade test to the new release. In `test/e2e/run.sh`, set:
- `UPGRADE_FROM=<v>`;
- `UPGRADE_FROM_SHA256` and `UPGRADE_FROM_BUILDKIT_SHA256` to the ci-accelerator and buildkit lines of the release's `SHA256SUMS`;
- `UPGRADE_ROLLOUT_EXPECTED` back to `0`, unless `main` already holds a change that restarts the daemons on purpose;
- `test/e2e/box-values-old.yaml` to what the new release needs on kind (tolerations for what runs on the control-plane node, small volumes).

From then on every change is tested against the version users have.

## A patch from a release branch

When `main` already contains changes that must not ship in the patch:

```sh
git switch -c release-0.<n> v0.<n>.<last patch>   # once per minor
git cherry-pick -x <commit from main>             # the fix lands in main first
```

Then follow steps 2 to 6 on that branch (pull request into `release-0.<n>`, tag on it). The e2e workflow runs on pull requests into `main` only; on the release branch start it by hand: `gh workflow run e2e.yml --ref release-0.<n>`. The changelog entry of the patch also goes into `main`.
