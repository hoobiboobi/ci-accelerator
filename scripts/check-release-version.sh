#!/usr/bin/env bash
# Checks that a release tag matches the charts before anything is published:
# the three charts carry the tag's version, the umbrella depends on exactly
# those buildkit and console versions, the console's appVersion (its image
# tag, v<appVersion>) is the same, the README installs it, and CHANGELOG.md
# has an entry with upgrade notes for it.
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
# The one-node installer installs this release.
qs=$(awk -F= '/^VERSION=/ {print $2; exit}' "$ROOT/scripts/quickstart.sh")
if [ "$qs" != "$version" ]; then
  echo "scripts/quickstart.sh installs $qs, the tag says $version" >&2; fail=1
fi
# The README installs this release (its quickstart download and helm install).
for pat in "releases/download/$tag/quickstart.sh" "charts/ci-accelerator --version $version "; do
  grep -qF -- "$pat" "$ROOT/README.md" || { echo "README.md does not mention $pat" >&2; fail=1; }
done
# Every release says what an upgrade to it means.
"$ROOT/scripts/changelog-section.sh" "$version" >/dev/null || fail=1
[ "$fail" = 0 ] || exit 1
echo "$version"
