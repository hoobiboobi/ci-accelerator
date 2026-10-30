#!/bin/sh
# The published ci-accelerator image tags that the charts in a tree (default:
# this one) run by default, one per line: the control plane's, v<appVersion>,
# and the disk probe's in every daemon pod, which stays on the release that
# last changed the probe and so can lag behind. A probe tag that is not
# among the released images yet (it moves before the release that carries
# it) is left out: there is nothing to pull.
#
#   scripts/default-images.sh [<tree>]
set -eu
root=${1:-$(cd "$(dirname "$0")/.." && pwd)}
helpers=$root/charts/buildkit/templates/_helpers.tpl
app=$(awk '/^appVersion:/ {gsub(/"/, "", $2); print $2; exit}' "$root/charts/control-plane/Chart.yaml")
probe=$(sed -n 's/.*default "\(v[0-9][^"]*\)" \$img\.tag.*/\1/p' "$helpers")
[ -n "$app" ] || { echo "no appVersion in $root/charts/control-plane/Chart.yaml" >&2; exit 1; }
[ -n "$probe" ] || { echo "no disk probe tag in $helpers" >&2; exit 1; }
echo "v$app"
if [ "$probe" != "v$app" ] && grep -q "^$probe: sha256:[0-9a-f]\{64\}\$" "$helpers"; then
  echo "$probe"
fi
