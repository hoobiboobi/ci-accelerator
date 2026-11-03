#!/usr/bin/env bash
# Runs scripts/quickstart.sh in a throwaway container with stand-ins for
# k3s, Helm, curl and systemctl: what the script decides by itself (where
# the build cache is, what a second run keeps, the free-disk check, what
# --uninstall removes), without a cluster. The real install on a clean
# machine is test/quickstart/run.sh.
#
#   test/quickstart/offline.sh        (needs docker)
set -euo pipefail
# Ubuntu 24.04 (2026-09-30), the release the quickstart is tested on.
IMAGE=ubuntu:24.04@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac

if [ "${1:-}" != --inside ]; then
  ROOT=$(cd "$(dirname "$0")/../.." && pwd)
  echo "quickstart.sh offline, in $IMAGE"
  exec docker run --rm -v "$ROOT/scripts/quickstart.sh:/quickstart.sh:ro" \
    -v "$ROOT/test/quickstart/offline.sh:/offline.sh:ro" "$IMAGE" bash /offline.sh --inside
fi

STATE=/var/lib/ci-accelerator
TOOLS=/usr/local/lib/ci-accelerator
HELM_VERSION=$(sed -n 's/^HELM_VERSION=//p' /quickstart.sh)
[ -f /sys/fs/cgroup/cgroup.controllers ] || { echo "the container has no cgroup v2, which quickstart.sh checks for"; exit 1; }
mkdir -p /run/systemd/system /stub

# Stand-ins: curl "downloads" k3s and its installer, whose uninstall script
# removes them again; df reports STUB_FREE_KB available (100 GiB unless a
# case sets less: the container's own disk is the machine's, whose free
# space would decide the cases).
stub() { printf '#!/bin/sh\n%s\n' "$2" >"$1" && chmod +x "$1"; }
stub /stub/systemctl 'exit 3'
stub /stub/sha256sum 'cat >/dev/null; exit 0'
stub /stub/k3s 'echo "$*" >>/tmp/k3s-calls
[ "$1" != --version ] || { echo "k3s version v1.36.5+k3s1 (stub)"; exit 0; }
[ "$1" = kubectl ] && shift
[ "$1 $2" != "get nodes" ] || echo node/stub
case "$*" in *"get secret gitlab-api-token"*) [ -e /tmp/secret-left ] ;; *) exit 0 ;; esac'
stub /stub/k3s-install.sh "cp /stub/k3s-uninstall.sh /usr/local/bin/"
stub /stub/k3s-uninstall.sh 'rm -rf /usr/local/bin/k3s /usr/local/bin/k3s-uninstall.sh /etc/rancher/k3s'
stub /stub/curl 'while [ $# -gt 0 ]; do [ "$1" != -o ] || out=$2; shift; done
case "$out" in */k3s) cp /stub/k3s "$out" ;; */install.sh) cp /stub/k3s-install.sh "$out" ;; esac'
stub /stub/df 'echo "Filesystem 1024-blocks Used Available Capacity Mounted on"
echo "stub 209715200 1 ${STUB_FREE_KB:-104857600} 50% /"'
setup() {
  mkdir -p "$TOOLS/helm-$HELM_VERSION"
  stub "$TOOLS/helm-$HELM_VERSION/helm" 'echo "$*" >>/tmp/helm-calls
case "$1 $2" in
  "get values") [ ! -e /tmp/has-api-token ] || printf "control-plane:\n  gitlab:\n    token:\n      existingSecret: gitlab-api-token\n" ;;
  "get notes") echo "(the notes)" ;;
esac
exit 0'
}

# qs <args>: one run of the script; its output in /tmp/out, status in $rc.
qs() {
  setup
  rc=0
  # shellcheck disable=SC2034 # read by the checks below, through eval
  env PATH="/stub:$PATH" RUNNER_TOKEN=glrt-offline CI_ACCELERATOR_CHART=/chart \
    bash /quickstart.sh --gitlab-url https://gitlab.example.com/ --yes "$@" >/tmp/out 2>&1 || rc=$?
}
pass=0 failn=0
check() {
  local name=$1; shift
  if "$@"; then pass=$((pass + 1)); echo "  ok    $name"; else failn=$((failn + 1)); echo "  FAIL  $name"; sed 's/^/      | /' /tmp/out | tail -n 6; fi
}
recorded() { [ "$(cat "$STATE/build-dir" 2>/dev/null)" = "$1" ]; }

qs --build-dir /srv/cache-a
check "a first install keeps the cache where --build-dir says" \
  eval '[ "$rc" = 0 ] && recorded /srv/cache-a && [ -d /srv/cache-a ] && grep -q "^      path: /srv/cache-a$" "$STATE/values.yaml"'
qs
check "a second run without --build-dir keeps the cache of values.yaml" \
  eval '[ "$rc" = 0 ] && recorded /srv/cache-a && [ ! -e "$STATE/buildkit" ]'
qs --build-dir /srv/other
check "a second run with another --build-dir stops and names values.yaml" \
  eval '[ "$rc" != 0 ] && grep -q "values.yaml" /tmp/out && recorded /srv/cache-a && [ ! -e /srv/other ]'
qs --build-dir /srv/cache-a
check "a second run with the same --build-dir goes on" eval '[ "$rc" = 0 ] && recorded /srv/cache-a'
STUB_FREE_KB=$((15 * 1048576)) qs
check "a second run on a disk the build cache filled warns, not stops" \
  eval '[ "$rc" = 0 ] && grep -q "WARNING: .*build cache" /tmp/out'
mkdir -p /srv/other && touch /srv/other/keep
qs --uninstall --yes
check "--uninstall removes the cache in use and nothing else" \
  eval '[ "$rc" = 0 ] && [ ! -e /srv/cache-a ] && [ ! -e "$STATE" ] && [ -e /srv/other/keep ]'

STUB_FREE_KB=$((15 * 1048576)) qs
check "a first install on a small disk stops" \
  eval '[ "$rc" != 0 ] && grep -q "at least 20 GB of free disk" /tmp/out'
qs
# Earlier releases rewrote the record on every run, whatever values.yaml said.
printf '/srv/other\n' >"$STATE/build-dir"
qs --uninstall --yes
check "--uninstall goes by values.yaml, not by an older record" \
  eval '[ "$rc" = 0 ] && [ ! -e "$STATE" ] && [ -e /srv/other/keep ]'

# A cache directory that was there before the install stays: --uninstall
# removes only what the script made.
mkdir -p /srv/shared && touch /srv/shared/keep
qs --build-dir /srv/shared
qs --uninstall --yes
check "--uninstall leaves a cache directory that was there before" \
  eval '[ "$rc" = 0 ] && [ -e /srv/shared/keep ] && [ ! -e "$STATE" ] && grep -q "Left /srv/shared" /tmp/out'
# k3s that quickstart.sh did not install is not its to remove.
mkdir -p "$STATE" /usr/local/bin && cp /stub/k3s-uninstall.sh /usr/local/bin/
qs --uninstall --yes
check "--uninstall refuses without its own install's marker" \
  eval '[ "$rc" != 0 ] && grep -q "not installed by quickstart.sh" /tmp/out && [ -x /usr/local/bin/k3s-uninstall.sh ] && [ -d "$STATE" ]'
rm -rf "$STATE" /usr/local/bin/k3s-uninstall.sh

qs --build-dir /srv/cache-b
sed -i 's/^    type: hostPath$/    type: emptyDir/' "$STATE/values.yaml"
qs
check "with the cache moved off hostPath in values.yaml, a second run records no directory" \
  eval '[ "$rc" = 0 ] && recorded /srv/cache-b && [ ! -e "$STATE/buildkit" ]'
qs --uninstall --yes

# Upgrades: a second run says k3s stays, keeps a GitLab token whose Secret
# exists although values.yaml does not name it, and a 0.2.x install stops
# before anything on the machine changes.
qs
rm -f /tmp/k3s-calls /tmp/helm-calls
qs
check "a second run says k3s stays as installed" \
  eval '[ "$rc" = 0 ] && grep -q "k3s: kept as installed (v1.36.5+k3s1)" /tmp/out && ! grep -q "^  - k3s v" /tmp/out'
rm -f /tmp/helm-calls && touch /tmp/has-api-token
qs
check "a GitLab token the running release uses stays in use" \
  eval '[ "$rc" = 0 ] && grep "upgrade --install ci-accelerator" /tmp/helm-calls | grep -q "existingSecret=gitlab-api-token"'
# Switched to sign-in: the token's Secret is still there, the running
# release no longer names it.
rm -f /tmp/has-api-token /tmp/helm-calls && touch /tmp/secret-left
qs
check "a token the running release no longer uses (sign-in, or taken out) stays out" \
  eval '[ "$rc" = 0 ] && ! grep -q "existingSecret=gitlab-api-token" /tmp/helm-calls'
printf 'console:\n  enabled: true\n' >>"$STATE/values.yaml"
rm -f /tmp/k3s-calls /tmp/helm-calls
qs
check "a 0.2.x install stops before anything on the machine changes" \
  eval '[ "$rc" != 0 ] && grep -q "cannot upgrade in place" /tmp/out && [ ! -e /tmp/helm-calls ] && ! grep -q "apply" /tmp/k3s-calls 2>/dev/null'
qs --uninstall --yes

echo "quickstart.sh offline: $pass passed, $failn failed"
[ "$failn" = 0 ]
