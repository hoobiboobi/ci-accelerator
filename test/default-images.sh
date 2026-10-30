#!/bin/sh
# scripts/default-images.sh, the images the weekly security run scans:
# the control plane's and, while it lags behind, the disk probe's released
# image; never a probe tag that is not published yet.
set -eu
cd "$(dirname "$0")/.."
app=$(awk '/^appVersion:/ {gsub(/"/, "", $2); print $2; exit}' charts/control-plane/Chart.yaml)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail=0
# want <description> <expected tags, space-separated> <disk probe tag>
want() {
  desc=$1 expect=$2
  rm -rf "$work/tree"
  mkdir -p "$work/tree/charts/control-plane" "$work/tree/charts/buildkit/templates"
  cp charts/control-plane/Chart.yaml "$work/tree/charts/control-plane/"
  sed "s/default \"v[0-9][^\"]*\" \$img\.tag/default \"$3\" \$img.tag/" charts/buildkit/templates/_helpers.tpl \
    >"$work/tree/charts/buildkit/templates/_helpers.tpl"
  got=$(scripts/default-images.sh "$work/tree" 2>&1 | tr '\n' ' ' | sed 's/ $//')
  if [ "$got" = "$expect" ]; then
    echo "  ok   $desc"
  else
    echo "  FAIL $desc: got '$got', want '$expect'"
    fail=1
  fi
}
want "the probe on the current release" "v$app" "v$app"
want "the probe on an earlier release" "v$app v0.3.0" v0.3.0
want "the probe on a release not published yet" "v$app" v9.9.9
got=$(scripts/default-images.sh "$work/none" 2>/dev/null) && {
  echo "  FAIL a tree without the charts: printed '$got' instead of failing"
  fail=1
}
exit $fail
