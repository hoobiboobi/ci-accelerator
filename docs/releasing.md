# Releasing

How a version gets from `main` to users. The rules behind it (patch vs minor, upgrade compatibility, branches) are in [ADR-0027](adr/0027-upgrades-and-branches.md); publishing is automated by `.github/workflows/release.yml` ([ADR-0025](adr/0025-releases.md)).

`<v>` below is the new version without the `v` (`0.2.0`), `<prev>` the last release.

## 1. Decide

- **Patch or minor?** A patch is fixes only. A minor brings something new that is ready and on by default.
- **Is `main` releasable?** Unfinished work must be switched off by default. If `main` already holds changes that should not ship in a patch, release the patch from a branch instead (see [A patch from a release branch](#a-patch-from-a-release-branch)).
- **Pinned components.** Look at the releases and security advisories of everything that is pinned: BuildKit, cert-manager, the gitlab-runner chart and image, VictoriaMetrics, VictoriaTraces, the tools in the Makefile, and the actions in the workflows. Bump what has a fix and is at least two weeks old.

## 2. The release pull request

On a branch `release/v<v>`:

1. Set the version in three places:
   - `version:` in `charts/buildkit/Chart.yaml`;
   - `version:` in `charts/ci-accelerator/Chart.yaml`;
   - the `buildkit` dependency's `version:` in `charts/ci-accelerator/Chart.yaml`.
2. In `CHANGELOG.md`:
   - rename `## Unreleased` to `## <v> - <yyyy-mm-dd>`;
   - read its **Upgrade notes** as a user would, and make sure they say what to do;
   - start a new `## Unreleased` above it with `### Upgrade notes` and `None.`.
3. Run `scripts/check-release-version.sh v<v>`. It checks the versions and the changelog entry, and prints the version.
4. Open the pull request. The checks and the e2e (including `upgrade` from `<prev>`) must be green. Merge it.

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
- packages both charts reproducibly and pushes them to `oci://ghcr.io/hoobiboobi/charts`;
- signs them with cosign;
- creates the GitHub release with the changelog entry, the archives, `SHA256SUMS` and the digests.

## 5. Verify as a user

Without registry credentials:

```sh
export HELM_REGISTRY_CONFIG=$(mktemp -d)/registry.json
helm pull oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version <v>
helm pull oci://ghcr.io/hoobiboobi/charts/buildkit --version <v>
sha256sum ci-accelerator-<v>.tgz buildkit-<v>.tgz   # compare with SHA256SUMS of the release
cosign verify ghcr.io/hoobiboobi/charts/ci-accelerator:<v> \
  --certificate-identity https://github.com/hoobiboobi/ci-accelerator/.github/workflows/release.yml@refs/tags/v<v> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Then install the umbrella from the registry on a kind cluster and run a build, as the README quickstart does.

## 6. After the release

Open a pull request that moves the upgrade test to the new release. In `test/e2e/run.sh`, set:
- `UPGRADE_FROM=<v>`;
- `UPGRADE_FROM_SHA256` and `UPGRADE_FROM_BUILDKIT_SHA256` to the two lines of the release's `SHA256SUMS`.

From then on every change is tested against the version users have.

## A patch from a release branch

When `main` already contains changes that must not ship in the patch:

```sh
git switch -c release-0.<n> v0.<n>.<last patch>   # once per minor
git cherry-pick -x <commit from main>             # the fix lands in main first
```

Then follow steps 2 to 6 on that branch (pull request into `release-0.<n>`, tag on it). The e2e workflow runs on pull requests into `main` only; on the release branch start it by hand: `gh workflow run e2e.yml --ref release-0.<n>`. The changelog entry of the patch also goes into `main`.
