#!/usr/bin/env bash
# End-to-end checks for charts/buildkit on a kind cluster.
#
#   make kind-up && make e2e        # KEEP=1 make e2e leaves everything behind
#
# Installs a separate release ("e2e") so a development release in the same
# cluster is not touched, and runs CI-job-like clients from a second
# namespace with Pod Security "baseline", the way runner job pods will (D17).
# Daemons run with hostPath state and a dead OTLP endpoint for the whole run.
#
# Compatible with bash 3.2 (macOS). Refuses to run against non-kind contexts.

set -uo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
CHART=$ROOT/charts/buildkit
FIXTURES=$ROOT/test/e2e/fixtures

CTX=${KUBE_CONTEXT:-kind-buildkit}
NS=${E2E_NAMESPACE:-ci-accelerator-e2e}
JOBS_NS=$NS-jobs
PSA_NS=$NS-psa
REL=e2e
ADDR=tcp://$REL-buildkit.$NS.svc:1234
STATE_DIR=/var/lib/ci-accelerator-e2e
KEEP=${KEEP:-0}

case "$CTX" in
  kind-*) CLUSTER=${CTX#kind-} ;;
  *) echo "refusing to run e2e against non-kind context '$CTX'" >&2; exit 2 ;;
esac

APP_VERSION=$(awk -F'"' '/^appVersion:/ {print $2}' "$CHART/Chart.yaml")
IMAGE=moby/buildkit:$APP_VERSION
LOGS=$(mktemp -d "${TMPDIR:-/tmp}/ci-accelerator-e2e.XXXXXX")

kc() { kubectl --context "$CTX" "$@"; }
hc() { helm --kube-context "$CTX" "$@"; }

# --- reporting ---------------------------------------------------------------

PASSED=0
FAILED=0
SUMMARY=""
pass() { PASSED=$((PASSED + 1)); SUMMARY="$SUMMARY\n  PASS  $*"; echo "  PASS  $*"; }
fail() { FAILED=$((FAILED + 1)); SUMMARY="$SUMMARY\n  FAIL  $*"; echo "  FAIL  $*"; }
info() { echo "        $*"; }
step() { echo; echo "== $*"; }
die()  { echo "e2e setup failed: $*" >&2; exit 1; }

# Unique build-arg values. $RANDOM is not reseeded in subshells, so keep a
# counter in the main shell instead.
SEQ=0
next_id() { SEQ=$((SEQ + 1)); ID="$(date +%s)-$$-$SEQ"; }

# eventually <tries> <cmd...>: retry every 2 s.
eventually() {
  local tries=$1; shift
  local i=0
  while [ "$i" -lt "$tries" ]; do
    "$@" && return 0
    i=$((i + 1)); sleep 2
  done
  return 1
}

# --- helpers -----------------------------------------------------------------

# Ready, non-terminating daemon pod on a node.
daemon_on() {
  kc -n "$NS" get pods -l "app.kubernetes.io/instance=$REL" \
    --field-selector "spec.nodeName=$1" \
    -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.deletionTimestamp} {.status.containerStatuses[0].ready}{"\n"}{end}' \
    | awk 'NF == 2 && $2 == "true" {print $1}'
}

# Name of the daemon pod that answers a client (empty if none answers).
who() {
  kc -n "$JOBS_NS" exec "$1" -- buildctl --addr "$ADDR" --timeout 5 debug workers -v 2>/dev/null \
    | awk '/worker.hostname/ {print $2; exit}'
}

# build <client> <fixture> [buildctl build args...]
build() {
  local client=$1 fixture=$2; shift 2
  kc -n "$JOBS_NS" exec "$client" -- buildctl --addr "$ADDR" build \
    --frontend dockerfile.v0 --local "context=/f/$fixture" --local "dockerfile=/f/$fixture" \
    --progress plain "$@"
}

# The line after the "RUN sleep 3 ..." step header says CACHED or not.
deps_cached() { grep -A1 'RUN sleep 3' "$1" | grep -q '^#[0-9]* CACHED$'; }

# Match what a RUN step printed, not the step header: buildkit echoes the
# whole command in the header, so a plain grep would match `echo WORD` there.
printed() { grep -qE "^#[0-9]+ [0-9.]+ $2\$" "$1"; }
count_printed() { grep -cE "^#[0-9]+ [0-9.]+ $2\$" "$1"; }

no_answer()  { [ -z "$(who "$1")" ]; }
has_answer() { [ -n "$(who "$1")" ]; }
has_daemon() { [ -n "$(daemon_on "$1")" ]; }

# kube-proxy applies PreferSameNode hints ~1-2 s after endpoints change
# (measured on kind: the first request after `helm --wait` went to another
# node). Wait until the build client is routed locally three times in a row
# before scenarios that depend on hitting the same cache.
routed_locally() {
  local d; d=$(daemon_on "$BUILD_NODE")
  [ -n "$d" ] && [ "$(who client-build)" = "$d" ] && [ "$(who client-build)" = "$d" ] \
    && [ "$(who client-build)" = "$d" ]
}
settle() { eventually 15 routed_locally || fail "routing did not settle on $BUILD_NODE"; }

remove_state() {
  local node
  for node in $(kind get nodes --name "$CLUSTER" 2>/dev/null); do
    docker exec "$node" rm -rf "$STATE_DIR" >/dev/null 2>&1 || true
  done
}

cleanup() {
  if [ "$KEEP" = "1" ]; then
    echo; echo "KEEP=1: leaving namespaces $NS, $JOBS_NS and $STATE_DIR on the nodes"
    return
  fi
  echo; echo "== cleanup"
  hc uninstall "$REL" -n "$NS" --wait >/dev/null 2>&1 || true
  kc delete namespace "$NS" "$JOBS_NS" "$PSA_NS" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
  remove_state
}
trap cleanup EXIT

# --- setup -------------------------------------------------------------------

step "setup (context $CTX, image $IMAGE, logs in $LOGS)"

BUILD_NODES=$(kc get nodes -l ci-accelerator.io/node-role=build -o jsonpath='{.items[*].metadata.name}')
BUILD_NODE=$(echo "$BUILD_NODES" | awk '{print $1}')
CP_NODE=$(kc get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[0].metadata.name}')
[ "$(echo "$BUILD_NODES" | wc -w)" -ge 2 ] || die "need at least 2 build nodes (run make kind-up)"
[ -n "$CP_NODE" ] || die "no control-plane node found"
info "build nodes: $BUILD_NODES; client node: $BUILD_NODE; node without a daemon: $CP_NODE"

remove_state
kc create namespace "$NS" --dry-run=client -o yaml | kc apply -f - >/dev/null
kc label namespace "$NS" pod-security.kubernetes.io/enforce=privileged --overwrite >/dev/null
kc create namespace "$JOBS_NS" --dry-run=client -o yaml | kc apply -f - >/dev/null
kc label namespace "$JOBS_NS" pod-security.kubernetes.io/enforce=baseline --overwrite >/dev/null

hc upgrade --install "$REL" "$CHART" -n "$NS" -f "$ROOT/test/e2e/values.yaml" \
  --wait --timeout 5m >"$LOGS/install.log" 2>&1 || { cat "$LOGS/install.log"; die "helm install"; }
info "release $REL installed"

for spec in "client-build:$BUILD_NODE" "client-cp:$CP_NODE"; do
  name=${spec%%:*}; node=${spec#*:}
  kc -n "$JOBS_NS" delete pod "$name" --ignore-not-found --wait=true >/dev/null
  kc -n "$JOBS_NS" run "$name" --image="$IMAGE" --restart=Never \
    --overrides="{\"spec\":{\"nodeName\":\"$node\"}}" --command -- sleep 3600 >/dev/null
done
kc -n "$JOBS_NS" wait --for=condition=Ready pod/client-build pod/client-cp --timeout=120s >/dev/null \
  || die "client pods not ready"
for name in client-build client-cp; do
  kc -n "$JOBS_NS" cp "$FIXTURES" "$name:/f" || die "copy fixtures to $name"
done
info "clients ready in namespace $JOBS_NS (PSA baseline)"
settle

# --- scenarios ---------------------------------------------------------------

step "preflight: a namespace enforcing PSA baseline is refused up front"
kc create namespace "$PSA_NS" --dry-run=client -o yaml | kc apply -f - >/dev/null
kc label namespace "$PSA_NS" pod-security.kubernetes.io/enforce=baseline --overwrite >/dev/null
if hc install "$REL-psa" "$CHART" -n "$PSA_NS" >"$LOGS/psa.log" 2>&1; then
  fail "install into a baseline namespace succeeded"
  hc uninstall "$REL-psa" -n "$PSA_NS" >/dev/null 2>&1
elif grep -q 'enforces Pod Security level "baseline"' "$LOGS/psa.log"; then
  pass "install refused with the PSA message"
else
  fail "install failed for another reason (see $LOGS/psa.log)"
fi
[ -z "$(hc list -n "$PSA_NS" -q)" ] && pass "no release left behind" || fail "release left behind in $PSA_NS"
kc delete namespace "$PSA_NS" --wait=false >/dev/null

step "cache: the second build reuses layers and the cache mount"
next_id; build client-build cache --opt "build-arg:BUST=$ID" >"$LOGS/cache-1.log" 2>&1; e1=$?
next_id; build client-build cache --opt "build-arg:BUST=$ID" >"$LOGS/cache-2.log" 2>&1; e2=$?
[ "$e1" = 0 ] && [ "$e2" = 0 ] && pass "both builds succeed" || fail "build exit codes $e1/$e2"
deps_cached "$LOGS/cache-2.log" && pass "deps step CACHED on the second build" || fail "deps step not CACHED"
printed "$LOGS/cache-2.log" CACHE_MOUNT_WARM && pass "cache mount WARM on the second build" || fail "cache mount COLD"

step "dedup: two concurrent builds execute a shared step once"
next_id; seed=$ID
SECONDS=0
build client-build dedup --opt target=main --opt "build-arg:SEED=$seed" --opt build-arg:WHO=A >"$LOGS/dedup-a.log" 2>&1 & pa=$!
build client-build dedup --opt target=main --opt "build-arg:SEED=$seed" --opt build-arg:WHO=B >"$LOGS/dedup-b.log" 2>&1 & pb=$!
ea=0; eb=0; wait "$pa" || ea=$?; wait "$pb" || eb=$?
took=$SECONDS
[ "$ea" = 0 ] && [ "$eb" = 0 ] && pass "both builds succeed (${took}s for two 8s steps)" || fail "build exit codes $ea/$eb"
next_id; build client-build dedup --opt target=read --opt "build-arg:READ_BUST=$ID" >"$LOGS/dedup-read.log" 2>&1
runs=$(count_printed "$LOGS/dedup-read.log" "exec seed=$seed")
[ "$runs" = 1 ] && pass "shared step executed once" || fail "shared step executed $runs times"

step "routing (PreferSameNode): a client reaches its own node's daemon"
local_daemon=$(daemon_on "$BUILD_NODE")
ok=1
for _ in 1 2 3; do [ "$(who client-build)" = "$local_daemon" ] || ok=0; done
[ "$ok" = 1 ] && pass "3/3 requests answered by $local_daemon on $BUILD_NODE" || fail "requests left the node"
answer=$(who client-cp)
[ -n "$answer" ] && pass "client on a node without a daemon falls back to $answer" || fail "no fallback answer"

step "routing (Local): strict node-local, no fallback"
hc upgrade "$REL" "$CHART" -n "$NS" -f "$ROOT/test/e2e/values.yaml" --set service.routing=Local \
  --wait --timeout 3m >"$LOGS/upgrade-local.log" 2>&1 || fail "upgrade to routing=Local"
eventually 15 no_answer client-cp && pass "client on a node without a daemon gets no answer" \
  || fail "client without a local daemon still answered"
[ "$(who client-build)" = "$local_daemon" ] && pass "client on a build node still served locally" \
  || fail "build-node client lost its daemon"
hc upgrade "$REL" "$CHART" -n "$NS" -f "$ROOT/test/e2e/values.yaml" \
  --wait --timeout 3m >"$LOGS/upgrade-back.log" 2>&1 || fail "upgrade back to defaults"
eventually 15 has_answer client-cp || true

step "graceful stop: a running build survives SIGTERM, new requests go elsewhere"
victim=$(daemon_on "$BUILD_NODE")
next_id
SECONDS=0
build client-build long --opt "build-arg:BUST=$ID" >"$LOGS/long.log" 2>&1 & plong=$!
sleep 8
kc -n "$NS" delete pod "$victim" --wait=false >/dev/null
info "t+${SECONDS}s SIGTERM to $victim"
sleep 3
during=$(who client-build)
if [ -n "$during" ] && [ "$during" != "$victim" ]; then
  pass "request during the drain answered by $during"
else
  fail "request during the drain answered by '${during:-nobody}'"
fi
elong=0; wait "$plong" || elong=$?
if [ "$elong" = 0 ] && printed "$LOGS/long.log" finished; then
  pass "30s build finished after SIGTERM (t+${SECONDS}s)"
else
  fail "running build aborted (exit $elong, see $LOGS/long.log)"
fi
kc -n "$NS" rollout status "ds/$REL-buildkit" --timeout=180s >/dev/null || fail "daemon not replaced"
eventually 30 has_daemon "$BUILD_NODE" || true
replacement=$(daemon_on "$BUILD_NODE")
info "replacement daemon on $BUILD_NODE: ${replacement:-none}"
settle

step "hostPath: the replacement daemon starts with the old cache"
next_id; build client-build cache --opt "build-arg:BUST=$ID" >"$LOGS/cache-3.log" 2>&1 \
  && pass "build on the replacement daemon succeeds" || fail "build on the replacement daemon"
deps_cached "$LOGS/cache-3.log" && pass "deps step still CACHED" || fail "layer cache lost on restart"
printed "$LOGS/cache-3.log" CACHE_MOUNT_WARM && pass "cache mount still WARM" || fail "cache mount lost on restart"
[ "$(who client-build)" = "$replacement" ] && pass "traffic back on the local daemon" \
  || fail "traffic not back on $replacement"

step "probes: healthy for the whole run with a dead OTLP endpoint"
restarts=$(kc -n "$NS" get pods -l "app.kubernetes.io/instance=$REL" \
  -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}' | awk '{s += $1} END {print s + 0}')
[ "$restarts" = 0 ] && pass "0 daemon restarts" || fail "$restarts daemon restarts"
# A draining daemon legitimately fails readiness; ignore the SIGTERM victim.
unhealthy=$(kc -n "$NS" get events --field-selector "reason=Unhealthy,involvedObject.name!=$victim" -o name | wc -l | tr -d ' ')
[ "$unhealthy" = 0 ] && pass "no Unhealthy probe events" || fail "$unhealthy Unhealthy probe events"
probe_pod=$(daemon_on "$BUILD_NODE")
# Run the liveness command exactly as deployed, not a copy of it.
probe_cmd=()
while IFS= read -r arg; do probe_cmd+=("$arg"); done < <(kc -n "$NS" get "ds/$REL-buildkit" \
  -o jsonpath='{range .spec.template.spec.containers[0].livenessProbe.exec.command[*]}{@}{"\n"}{end}')
SECONDS=0
kc -n "$NS" exec "$probe_pod" -- "${probe_cmd[@]}" >/dev/null 2>&1
probe_s=$SECONDS
[ "$probe_s" -le 2 ] && pass "deployed probe command answers in ${probe_s}s" || fail "deployed probe command took ${probe_s}s"
SECONDS=0
kc -n "$NS" exec "$probe_pod" -- buildctl debug workers >/dev/null 2>&1
info "for comparison, plain buildctl with the inherited OTEL env took ${SECONDS}s"

# --- result ------------------------------------------------------------------

echo
echo "== result: $PASSED passed, $FAILED failed"
printf "%b\n" "$SUMMARY"
[ "$FAILED" = 0 ] || echo "logs: $LOGS"
[ "$FAILED" = 0 ]
