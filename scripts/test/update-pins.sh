#!/usr/bin/env bash
# scripts/update-pins.sh on a copy of the files it edits, with a stand-in
# for curl: a run that fails half-way leaves every file as it was, and the
# runner fragment must run the box's runner image.
#
#   scripts/test/update-pins.sh
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
pass=0 failn=0
T=$(mktemp -d "${TMPDIR:-/tmp}/update-pins-test.XXXXXX")
trap 'rm -rf "$T"' EXIT
check() { # <name> <command...>
  local name=$1; shift
  if "$@"; then pass=$((pass + 1)); echo "  ok    $name"
  else failn=$((failn + 1)); echo "  FAIL  $name"; sed 's/^/      | /' "$T/out" | tail -n 6; fi
}
# copy: the files a run reads and writes, outside git (no HEAD to compare).
copy() {
  rm -rf "$T/tree" && mkdir -p "$T/tree"
  local f
  for f in scripts/update-pins.sh scripts/quickstart.sh docs/quickstart.md Makefile test/e2e/run.sh \
    charts/ci-accelerator/values.yaml integrations/gitlab-runner/values.yaml; do
    mkdir -p "$T/tree/$(dirname "$f")" && cp -p "$f" "$T/tree/$f"
  done
  cp -R "$T/tree" "$T/before"
}
# curl: the k3s amd64 sums file is served (with a sum the tree does not
# have), every other download fails.
mkdir -p "$T/bin"
cat >"$T/bin/curl" <<'EOF'
#!/bin/sh
out=; url=
while [ $# -gt 0 ]; do case "$1" in -o) out=$2; shift ;; http*) url=$1 ;; esac; shift; done
case "$url" in
  *sha256sum-amd64.txt) echo "1111111111111111111111111111111111111111111111111111111111111111  k3s" >"$out" ;;
  *) echo "curl: (22) The requested URL returned error: 503" >&2; exit 22 ;;
esac
EOF
chmod +x "$T/bin/curl"
# shellcheck disable=SC2034 # rc is read by the checks, through eval
pins() { rc=0; (cd "$T/tree" && PATH="$T/bin:$PATH" scripts/update-pins.sh "$@") >"$T/out" 2>&1 || rc=$?; }

echo "update-pins.sh"
copy
pins k3s
check "a download that fails after the first sum changed: nothing changed" \
  eval '[ "$rc" != 0 ] && grep -q "download failed" "$T/out" && diff -r "$T/before" "$T/tree" >/dev/null'

copy
sed -i.bak 's/^  tag: alpine-v[0-9.]*$/  tag: alpine-v1.0.0/' "$T/tree/integrations/gitlab-runner/values.yaml" && rm -f "$T/tree/integrations/gitlab-runner/values.yaml.bak"
pins gitlab-runner
check "the runner fragment's image must be the box's" \
  eval '[ "$rc" != 0 ] && grep -q "gitlab-runner image of the box and of integrations/gitlab-runner differs" "$T/out"'

echo "update-pins.sh: $pass passed, $failn failed"
[ "$failn" = 0 ]
