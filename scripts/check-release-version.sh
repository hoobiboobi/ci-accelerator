#!/usr/bin/env bash
# Checks that a release tag matches the charts before anything is published:
# the three charts carry the tag's version, the umbrella depends on exactly
# those buildkit and console versions, the console's appVersion (its image
# tag, v<appVersion>) is the same, the README and docs/gitops.md install
# it, the README's one-node upgrade uses the quickstart's Helm, and
# CHANGELOG.md has an entry with upgrade notes for it.
# Prints the version on success.
#
#   scripts/check-release-version.sh v0.1.0
#   scripts/check-release-version.sh v0.1.0-rc.1

set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
tag=${1:?usage: $0 <tag, e.g. v0.1.0>}

semver='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$'
printf '%s' "$tag" | grep -Eq "$semver" \
  || { echo "tag $tag is not v<semver> (no build metadata: OCI tags cannot hold '+')" >&2; exit 1; }
version=${tag#v}

# version: of a Chart.yaml (top-level key only).
chart_version() { awk '/^version:/ {print $2; exit}' "$1"; }

fail=0
for chart in buildkit console ci-accelerator; do
  v=$(chart_version "$ROOT/charts/$chart/Chart.yaml")
  if [ "$v" != "$version" ]; then
    echo "charts/$chart/Chart.yaml has version $v, the tag says $version" >&2; fail=1
  fi
done
for sub in buildkit console; do
  dep=$(awk "/^  - name: $sub\$/ {found = 1; next} found && /^    version:/ {print \$2; exit}" "$ROOT/charts/ci-accelerator/Chart.yaml")
  if [ "$dep" != "$version" ]; then
    echo "charts/ci-accelerator depends on $sub $dep, the tag says $version" >&2; fail=1
  fi
done
# The console chart's default image is the one this release builds.
app=$(awk '/^appVersion:/ {gsub(/"/, "", $2); print $2; exit}' "$ROOT/charts/console/Chart.yaml")
if [ "$app" != "$version" ]; then
  echo "charts/console/Chart.yaml has appVersion $app (its image tag), the tag says $version" >&2; fail=1
fi
# The box's appVersion is its own version (the console's), not BuildKit's.
box=$(awk '/^appVersion:/ {gsub(/"/, "", $2); print $2; exit}' "$ROOT/charts/ci-accelerator/Chart.yaml")
if [ "$box" != "$version" ]; then
  echo "charts/ci-accelerator/Chart.yaml has appVersion $box, the tag says $version" >&2; fail=1
fi
# The Artifact Hub image annotations name this release's console image.
for chart in console ci-accelerator; do
  grep -q "image: ghcr.io/hoobiboobi/ci-accelerator-console:v$version\$" "$ROOT/charts/$chart/Chart.yaml" \
    || { echo "charts/$chart/Chart.yaml: artifacthub.io/images does not name ci-accelerator-console:v$version" >&2; fail=1; }
done
# The one-node installer installs this release.
qs=$(awk -F= '/^VERSION=/ {print $2; exit}' "$ROOT/scripts/quickstart.sh")
if [ "$qs" != "$version" ]; then
  echo "scripts/quickstart.sh installs $qs, the tag says $version" >&2; fail=1
fi
# The README installs this release (its quickstart download and helm install).
for pat in "releases/download/$tag/quickstart.sh" "charts/ci-accelerator --version $version "; do
  grep -qF -- "$pat" "$ROOT/README.md" || { echo "README.md does not mention $pat" >&2; fail=1; }
done
# The GitLab CI component is included from this release's tag.
for f in README.md ci/gitlab/README.md; do
  grep -qF -- "hoobiboobi/ci-accelerator/$tag/ci/gitlab/build.yml" "$ROOT/$f" \
    || { echo "$f does not include the component from $tag" >&2; fail=1; }
done
# The README's upgrade command for the one-node install uses the Helm the
# script installs.
qs_helm=$(awk -F= '/^HELM_VERSION=/ {print $2; exit}' "$ROOT/scripts/quickstart.sh")
grep -qF -- "/helm-$qs_helm/helm upgrade" "$ROOT/README.md" \
  || { echo "README.md: the quickstart upgrade command does not use helm-$qs_helm (scripts/quickstart.sh HELM_VERSION)" >&2; fail=1; }
# The GitOps examples install this release (Flux version, Argo CD targetRevision).
for pat in "      version: $version" "targetRevision: $version\`"; do
  grep -qF -- "$pat" "$ROOT/docs/gitops.md" || { echo "docs/gitops.md does not mention '$pat'" >&2; fail=1; }
done
# Artifact Hub shows the umbrella's change list on the version page; it is
# written per release (an entry for this version in CHANGELOG.md is checked
# below, the annotation must at least exist).
grep -q '^  artifacthub.io/changes: |$' "$ROOT/charts/ci-accelerator/Chart.yaml" \
  || { echo "charts/ci-accelerator/Chart.yaml has no artifacthub.io/changes" >&2; fail=1; }
# Every release says what an upgrade to it means.
"$ROOT/scripts/changelog-section.sh" "$version" >/dev/null || fail=1
[ "$fail" = 0 ] || exit 1
echo "$version"
