#!/bin/sh
# The released ci-accelerator images listed in charts/buildkit and
# charts/control-plane (a pinned digest of another release is refused) are
# the same list, and it has the release the e2e upgrades from (whose
# install pins that digest: a wrong one fails to pull). That release is the
# newest release tag, so the step after a release (docs/releasing.md,
# "After the release") cannot be forgotten: with the repository's tags at
# hand (the checks fetch them), the next pull request fails until it is
# done. RELEASE_TAG (set by the release workflow) is the tag being released
# now, not yet one to upgrade from.
set -eu
cd "$(dirname "$0")/.."
list() {
  sed -n "/define \"$1.releasedImages\"/,/{{- end }}/p" "charts/$2/templates/_helpers.tpl" | grep -E '^v[0-9]'
}
bk=$(list buildkit buildkit)
cp=$(list control-plane control-plane)
if [ "$bk" != "$cp" ]; then
  echo "the released images differ between charts/buildkit and charts/control-plane:"
  tmp=$(mktemp)
  printf '%s\n' "$bk" >"$tmp"
  printf '%s\n' "$cp" | diff "$tmp" - || true
  rm -f "$tmp"
  exit 1
fi
from=$(sed -n 's/^UPGRADE_FROM=\${UPGRADE_FROM:-\(.*\)}$/\1/p' test/e2e/run.sh)
[ -n "$from" ] || { echo "test/e2e/run.sh has no UPGRADE_FROM"; exit 1; }
# Final releases only: a release candidate is not upgraded from.
newest=$(git tag -l 'v[0-9]*' 2>/dev/null | grep -v -- - | grep -vx "${RELEASE_TAG:-none}" | sort -V | tail -n 1)
if [ -z "$newest" ]; then
  echo "no release tags here: the newest release is not compared with UPGRADE_FROM"
elif [ "$newest" != "v$from" ]; then
  echo "the newest release is $newest, the e2e upgrades from $from: do the step after the release (docs/releasing.md, \"After the release\")"
  exit 1
fi
printf '%s\n' "$bk" | grep -qE "^v$from: sha256:[0-9a-f]{64}\$" || {
  echo "the released images in charts/*/templates/_helpers.tpl lack v$from (the release the e2e upgrades from)"
  exit 1
}
printf '%s\n' "$bk" | awk -F': ' 'seen[$2]++ { print "the digest " $2 " is listed twice"; bad = 1 } END { exit bad }'
echo "released images: $(printf '%s\n' "$bk" | wc -l | tr -d ' ') listed in both charts, v$from among them"
