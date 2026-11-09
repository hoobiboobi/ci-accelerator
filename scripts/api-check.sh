#!/usr/bin/env bash
# The console API's contract (docs/api/openapi.yaml) against the newest
# release that carries one: a change that breaks a client of that release
# (a field or a value taken away, a type changed, a parameter made
# stricter) fails. internal/api's test checks the responses against the
# contract; this checks the contract against what was promised before.
#
#   scripts/api-check.sh <oasdiff binary>    (make api-check)
#
# Needs the repository's release tags (the CI checkout fetches them).
set -euo pipefail
oasdiff=${1:?usage: $0 <oasdiff binary>}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
spec=docs/api/openapi.yaml
cd "$ROOT"
base=""
for tag in $(git tag -l 'v[0-9]*' | grep -v -- - | sort -rV); do
  if git cat-file -e "$tag:$spec" 2>/dev/null; then
    base=$tag
    break
  fi
done
if [ -z "$base" ]; then
  echo "api-check: no release carries $spec yet: nothing to compare with"
  exit 0
fi
old=$(mktemp)
trap 'rm -f "$old"' EXIT
git show "$base:$spec" >"$old"
echo "api-check: $spec against $base"
# The contract's promise (its info.description): within v1 no field is
# removed, and a new field or value may appear. oasdiff's defaults
# differ on two points; docs/api/oasdiff-levels.txt sets them.
"$oasdiff" breaking "$old" "$spec" --fail-on WARN --severity-levels docs/api/oasdiff-levels.txt
