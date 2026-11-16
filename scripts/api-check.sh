#!/usr/bin/env bash
# The console API's contract (docs/api/openapi.yaml) against the newest
# release that carries one: a change that breaks a client of that release
# (a field or a value taken away, a type changed, a parameter made
# stricter) fails. internal/api's test checks the responses against the
# contract; this checks the contract against what was promised before.
#
#   scripts/api-check.sh <oasdiff binary>    (make api-check)
#
# Needs the repository's release tags (the CI checkout fetches them): with
# none, or with git failing, it fails rather than compare nothing.
# API_CHECK_BASE=<git ref> compares with that ref's contract instead.
set -euo pipefail
oasdiff=${1:?usage: $0 <oasdiff binary>}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
spec=docs/api/openapi.yaml
cd "$ROOT"
base=${API_CHECK_BASE:-}
if [ -z "$base" ]; then
  # Read into a variable first: a failing git inside a for list would
  # pass unnoticed.
  tags=$(git tag -l 'v[0-9]*')
  releases=$(printf '%s\n' "$tags" | grep -v -- - | sort -rV || true)
  if [ -z "$releases" ]; then
    echo "api-check: no release tag (v*) in this clone: fetch the tags (git fetch --tags) or set API_CHECK_BASE" >&2
    exit 1
  fi
  for tag in $releases; do
    if git cat-file -e "$tag:$spec" 2>/dev/null; then
      base=$tag
      break
    fi
  done
  if [ -z "$base" ]; then
    # v0.6.0 and later carry the contract: their tags are missing here.
    echo "api-check: no release tag here carries $spec (v0.6.0 does): fetch the tags or set API_CHECK_BASE" >&2
    exit 1
  fi
fi
old=$(mktemp)
trap 'rm -f "$old"' EXIT
git show "$base:$spec" >"$old"
echo "api-check: $spec against $base"
# The contract's promise (its info.description): within v1 no field is
# removed, and a new field or value may appear; an operationId (a method
# name in generated clients) and a parameter's default stay. oasdiff's
# defaults differ on these points; docs/api/oasdiff-levels.txt sets them.
"$oasdiff" breaking "$old" "$spec" --fail-on WARN --severity-levels docs/api/oasdiff-levels.txt
