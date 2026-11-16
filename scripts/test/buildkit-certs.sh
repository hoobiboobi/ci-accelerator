#!/usr/bin/env bash
# scripts/buildkit-certs.sh: the CA is never replaced behind the user's
# back, and the printed commands name the right namespace and paths.
#
#   scripts/test/buildkit-certs.sh      (needs openssl)
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
pass=0 failn=0
O=$(mktemp -d "${TMPDIR:-/tmp}/buildkit-certs-test.XXXXXX")
trap 'rm -rf "$O"' EXIT
check() { # <name> <command...>
  local name=$1; shift
  if "$@"; then pass=$((pass + 1)); echo "  ok    $name"
  else failn=$((failn + 1)); echo "  FAIL  $name"; sed 's/^/      | /' "$O/out" | tail -n 6; fi
}
# shellcheck disable=SC2034 # rc and line are read by the checks, through eval
certs() { rc=0; scripts/buildkit-certs.sh --name ci-accelerator-buildkit "$@" >"$O/out" 2>&1 || rc=$?; }

echo "buildkit-certs.sh"
certs --out "$O/c" --namespace ci-accelerator
check "a first run makes a CA and the certificates" eval '[ "$rc" = 0 ] && [ -f "$O/c/ca/ca.key" ] && [ -f "$O/c/client/tls.crt" ]'
cp "$O/c/ca/ca.crt" "$O/ca-before.crt"
mv "$O/c/ca/ca.key" "$O/offline-ca.key"
certs --out "$O/c" --no-server --client runner
check "with ca.key kept offline, one more client stops instead of making a new CA" \
  eval '[ "$rc" != 0 ] && grep -q "ca.key is missing" "$O/out" && cmp -s "$O/ca-before.crt" "$O/c/ca/ca.crt" && [ ! -e "$O/c/runner" ]'
mv "$O/offline-ca.key" "$O/c/ca/ca.key"
certs --out "$O/c" --no-server --client runner
check "with ca.key back, the client is issued from the same CA" \
  eval '[ "$rc" = 0 ] && grep -q "reusing CA" "$O/out" && openssl verify -CAfile "$O/ca-before.crt" "$O/c/runner/tls.crt" >/dev/null 2>&1'

certs --out "$O/c" --namespace ci-accelerator --no-server --client controller
check "the controller's client Secret goes to the box's namespace" \
  eval '[ "$rc" = 0 ] && grep -q "ci-accelerator-buildkit-controller -n ci-accelerator " "$O/out" && grep -q "control-plane.tls.existingSecret=ci-accelerator-buildkit-controller" "$O/out" && ! grep -q "jobs-namespace" "$O/out"'
certs --out "$O/c" --no-server --client runner
check "another client's Secret goes to the jobs' namespace" eval '[ "$rc" = 0 ] && grep -q "ci-accelerator-buildkit-runner -n <jobs-namespace>" "$O/out"'

certs --out "$O/with space" --namespace ci-accelerator
# shellcheck disable=SC2034
line=$(grep -m1 -- "--from-file=" "$O/out")
check "an --out with a space prints paths a shell reads back whole" \
  eval '[ "$rc" = 0 ] && eval "set -- $line" && [ "$1" = "--from-file=$O/with space/server/tls.crt" ] && [ -f "${1#--from-file=}" ]'

echo "buildkit-certs.sh: $pass passed, $failn failed"
[ "$failn" = 0 ]
