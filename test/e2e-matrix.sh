#!/bin/sh
# Every e2e scenario runs in exactly one group of the e2e workflow's
# matrix (scripts/ci-changes.sh prints it): a scenario added to
# test/e2e/run.sh and forgotten there would never run in CI.
set -eu
cd "$(dirname "$0")/.."
all=$(sed -n 's/^ALL_SCENARIOS="\(.*\)"$/\1/p' test/e2e/run.sh)
grouped=$(grep -o '"scenarios":"[^"]*"' scripts/ci-changes.sh | sed 's/"scenarios":"//; s/"$//')
[ -n "$all" ] && [ -n "$grouped" ] || { echo "e2e matrix: scenarios not found"; exit 1; }
bad=0
for s in $all; do
  n=$(printf '%s\n' $grouped | grep -cx "$s" || true)
  [ "$n" = 1 ] || { echo "e2e matrix: scenario $s is in $n groups, want 1"; bad=1; }
done
for s in $grouped; do
  printf '%s\n' $all | grep -qx "$s" || { echo "e2e matrix: unknown scenario $s"; bad=1; }
done
[ "$bad" = 0 ] && echo "e2e matrix: every scenario in one group"
exit "$bad"
