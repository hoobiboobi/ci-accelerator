#!/usr/bin/env bash
# End-to-end checks for charts/buildkit on a kind cluster.
#
#   make kind-up && make e2e
#   KEEP=1 make e2e                  # leave release, namespaces and node dirs
#   SCENARIOS="cache dedup" make e2e # run a subset (setup always runs)
#   make e2e-clean                   # remove what an interrupted run left
#
# For checking the checks: E2E_CHART=<chart dir> runs against another copy of
# the chart, E2E_EXTRA_VALUES="<file> ..." adds values files after the e2e
# ones. A deliberately broken chart or value must turn scenarios red.
#
# Installs its own release ("e2e") in its own namespace, so a development
# release in the same cluster is not touched, and runs CI-job-like clients
# from a second namespace with Pod Security "baseline", the way runner job
# pods will. The daemons run with hostPath state, small GC caps, a dead
# OpenTelemetry backend and mutual TLS (a CA made by scripts/buildkit-certs.sh;
# the clients mount a client certificate from a Secret, as job pods will). Never run two e2e runs against the
# same namespace at the same time.
#
# Compatible with bash 3.2 (macOS). Refuses to run against anything that is
# not a local kind cluster.

set -uo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
CHART=${E2E_CHART:-$ROOT/charts/buildkit}
FIXTURES=$ROOT/test/e2e/fixtures
VALUES=$ROOT/test/e2e/values.yaml

CTX=${KUBE_CONTEXT:-kind-buildkit}
NS=${E2E_NAMESPACE:-ci-accelerator-e2e}
JOBS_NS=$NS-jobs
PSA_NS=$NS-psa
REL=e2e
SVC_HOST=$REL-buildkit.$NS.svc
ADDR=tcp://$SVC_HOST:1234
TLS_SECRET=$REL-buildkit-tls
CLIENT_SECRET=$REL-client
# Every client call: the client certificate, and the Service name for the
# server certificate check also when a call goes to a pod IP.
TLS=(--tlsdir /certs --tlsservername "$SVC_HOST")
# Per namespace, so runs in different namespaces never share a state dir.
STATE_DIR=/var/lib/$NS
HOST_PATH=$STATE_DIR/buildkit
E2E_LABEL=ci-accelerator.io/e2e
KEEP=${KEEP:-0}
SCENARIOS=${SCENARIOS:-all}
MODE=${1:-run}
ALL_SCENARIOS="preflight tls cache dedup routing local bump graceful persist sigkill badconfig gc debug probes"

refuse() { echo "refusing to run e2e: $*" >&2; exit 2; }

# --- safety: only ever act on a local kind cluster ---------------------------

case "$CTX" in
  kind-*) CLUSTER=${CTX#kind-} ;;
  *) refuse "context '$CTX' is not a kind context" ;;
esac
for tool in kind docker kubectl helm; do
  command -v "$tool" >/dev/null 2>&1 || refuse "$tool is not installed"
done
kind get clusters 2>/dev/null | grep -qx "$CLUSTER" || refuse "no local kind cluster named '$CLUSTER'"
provider_ids=$(kubectl --context "$CTX" get nodes -o jsonpath='{range .items[*]}{.spec.providerID}{"\n"}{end}' 2>/dev/null) \
  || refuse "cannot list the nodes of '$CTX'"
if [ -z "$provider_ids" ] || printf '%s\n' "$provider_ids" | grep -qvE "^kind://[a-z]+/$CLUSTER/"; then
  refuse "'$CTX' does not point at kind cluster '$CLUSTER'"
fi

# A sleeping laptop pauses the Docker VM and everything in it, and the timing
# checks below use the wall clock: keep macOS awake for as long as this script
# runs, and say so if the host slept anyway (2026-10-18: two runs showed
# 16-minute "hangs" that were Maintenance Sleep).
if command -v caffeinate >/dev/null 2>&1; then caffeinate -i -s -w $$ & disown; fi
WAKE_AT_START=$(sysctl -n kern.waketime 2>/dev/null || true)

APP_VERSION=$(awk -F'"' '/^appVersion:/ {print $2}' "$CHART/Chart.yaml")
IMAGE=moby/buildkit:$APP_VERSION
LOGS=$(mktemp -d "${TMPDIR:-/tmp}/ci-accelerator-e2e.XXXXXX")

kc() { kubectl --context "$CTX" "$@"; }
hc() { helm --kube-context "$CTX" "$@"; }

# --- reporting ---------------------------------------------------------------

PASSED=0
FAILED=0
SETUP_FAILED=0
SUMMARY=""
pass() { PASSED=$((PASSED + 1)); SUMMARY="$SUMMARY\n  PASS  $*"; echo "  PASS  $*"; }
fail() { FAILED=$((FAILED + 1)); SUMMARY="$SUMMARY\n  FAIL  $*"; echo "  FAIL  $*"; }
check() { local desc=$1; shift; if "$@"; then pass "$desc"; else fail "$desc"; fi; }
info() { echo "        $*"; }
quiet() { "$@" >/dev/null 2>&1; }
START=$(date +%s)
step() { echo; echo "== [$(elapsed)] $*"; }
elapsed() { local t=$(( $(date +%s) - START )); printf '%d:%02d' $((t / 60)) $((t % 60)); }
die()  { SETUP_FAILED=1; echo "e2e setup failed: $*" >&2; exit 1; }
want() { [ "$SCENARIOS" = all ] || case " $SCENARIOS " in *" $1 "*) return 0 ;; *) return 1 ;; esac; }

# Unique build-arg values. $RANDOM is not reseeded in subshells (zsh) and
# repeats across runs, so use time, PID and a counter kept in the main shell.
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

# --- cluster helpers ---------------------------------------------------------

INSTALL_ARGS=(-f "$VALUES")
for extra in ${E2E_EXTRA_VALUES:-}; do INSTALL_ARGS+=(-f "$extra"); done
INSTALL_ARGS+=(--set "storage.hostPath.path=$HOST_PATH" --set "tls.existingSecret=$TLS_SECRET")

# Ready, non-terminating daemon pod on a node.
daemon_on() {
  kc -n "$NS" get pods -l "app.kubernetes.io/instance=$REL" \
    --field-selector "spec.nodeName=$1" \
    -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.deletionTimestamp} {.status.containerStatuses[0].ready}{"\n"}{end}' \
    | awk 'NF == 2 && $2 == "true" {print $1}'
}
pod_ip() { kc -n "$NS" get pod "$1" -o jsonpath='{.status.podIP}'; }
daemon_ips() {
  kc -n "$NS" get pods -l "app.kubernetes.io/instance=$REL" \
    -o jsonpath='{range .items[*]}{.status.podIP} {.metadata.deletionTimestamp}{"\n"}{end}' | awk 'NF == 1 {print $1}'
}
pod_uids() {
  kc -n "$NS" get pods -l "app.kubernetes.io/instance=$REL" -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' | sort | tr '\n' ' '
}

# Name of the daemon pod answering a client through the Service (or at an
# explicit address); empty if none answers.
who() { who_at "$1" "$ADDR"; }
who_at() {
  kc -n "$JOBS_NS" exec "$1" -- buildctl --addr "$2" "${TLS[@]}" --timeout 5 debug workers -v 2>/dev/null \
    | awk '/worker.hostname/ {print $2; exit}'
}
# The request fails the way an unreachable daemon fails, not for another reason.
no_answer() {
  local out
  out=$(kc -n "$JOBS_NS" exec "$1" -- buildctl --addr "$ADDR" "${TLS[@]}" --timeout 5 debug workers -v 2>&1)
  ! printf '%s' "$out" | grep -q 'worker.hostname' \
    && printf '%s' "$out" | grep -qiE 'deadline exceeded|i/o timeout|connection refused|Unavailable'
}
has_answer() { [ -n "$(who "$1")" ]; }
has_daemon() { [ -n "$(daemon_on "$1")" ]; }

# build_at <client> <addr> <fixture> [buildctl build args...]
build_at() {
  local client=$1 addr=$2 fixture=$3; shift 3
  kc -n "$JOBS_NS" exec "$client" -- buildctl --addr "$addr" "${TLS[@]}" build \
    --frontend dockerfile.v0 --local "context=/f/$fixture" --local "dockerfile=/f/$fixture" \
    --progress plain "$@"
}
build() { local client=$1; shift; build_at "$client" "$ADDR" "$@"; }

# What a RUN step printed, not the step header: buildkit echoes the whole
# command in the header, so a plain grep would match `echo WORD` there.
printed() { grep -qE "^#[0-9]+ [0-9.]+ $2\$" "$1"; }
count_printed() { grep -cE "^#[0-9]+ [0-9.]+ $2\$" "$1"; }
# The line after the "RUN sleep 3 ..." step header says CACHED or not.
deps_cached() { grep -A1 'RUN sleep 3' "$1" | grep -q '^#[0-9]* CACHED$'; }

# Unshared cache records of the daemon at an IP, in bytes (what GC caps see).
cache_bytes() {
  kc -n "$JOBS_NS" exec client-build -- buildctl --addr "tcp://$1:1234" "${TLS[@]}" du \
    --format '{{range .}}{{if not .Shared}}{{.Size}} {{end}}{{end}}' 2>/dev/null \
    | tr ' ' '\n' | awk '{s += $1} END {printf "%d", s}'
}

# kube-proxy applies PreferSameNode hints ~1-2 s after endpoints change
# (measured on kind: the first request after `helm --wait` went to another
# node). Cache-dependent scenarios first wait for three local answers in a row.
routed_locally() {
  local d; d=$(daemon_on "$BUILD_NODE")
  [ -n "$d" ] && [ "$(who client-build)" = "$d" ] && [ "$(who client-build)" = "$d" ] \
    && [ "$(who client-build)" = "$d" ]
}
settle() { eventually 15 routed_locally || fail "routing did not settle on $BUILD_NODE"; }

# Create (or reuse) a namespace owned by this script, with a PSA level.
ensure_ns() {
  local ns=$1 level=$2
  if kc get namespace "$ns" >/dev/null 2>&1; then
    [ "$(kc get namespace "$ns" -o jsonpath="{.metadata.labels.ci-accelerator\.io/e2e}")" = true ] \
      || die "namespace $ns exists and was not created by this script; refusing to use it"
  else
    kc create namespace "$ns" >/dev/null || die "create namespace $ns"
  fi
  kc label namespace "$ns" "$E2E_LABEL=true" "pod-security.kubernetes.io/enforce=$level" --overwrite >/dev/null
}

# certs <dir>: a fresh CA with server and client certificates for the release.
certs() {
  "$ROOT/scripts/buildkit-certs.sh" --name "$REL-buildkit" --namespace "$NS" --out "$1" >"$1.log" 2>&1 \
    || { cat "$1.log"; die "create certificates in $1"; }
}
# put_secret <namespace> <name> <dir>: (re)create a Secret from a cert dir.
put_secret() {
  kc -n "$1" delete secret "$2" --ignore-not-found >/dev/null
  kc -n "$1" create secret generic "$2" \
    --from-file="$3/tls.crt" --from-file="$3/tls.key" --from-file="$3/ca.crt" >/dev/null || die "create Secret $1/$2"
}

node_names() { kind get nodes --name "$CLUSTER" 2>/dev/null; }
remove_state() {
  local node
  for node in $(node_names); do docker exec "$node" rm -rf "$STATE_DIR" >/dev/null 2>&1 || true; done
}
create_state() {
  local node
  for node in $BUILD_NODES; do docker exec "$node" mkdir -p "$HOST_PATH" || die "mkdir $HOST_PATH on $node"; done
}

# Uninstall a previous run and wait until its daemons are gone, so their
# state dir is not deleted under running processes.
remove_release() {
  if hc status "$REL" -n "$NS" >/dev/null 2>&1; then
    hc uninstall "$REL" -n "$NS" --wait --timeout 5m >/dev/null 2>&1 || true
  fi
  kc -n "$NS" wait --for=delete pod -l "app.kubernetes.io/instance=$REL" --timeout=300s >/dev/null 2>&1 || true
}

collect_diagnostics() {
  {
    echo "### daemons"; kc -n "$NS" get ds,pods -o wide
    echo "### daemonset"; kc -n "$NS" describe ds "$REL-buildkit" | tail -40
    echo "### events"; kc -n "$NS" get events --sort-by=.lastTimestamp | tail -60
    echo "### logs"; kc -n "$NS" logs -l "app.kubernetes.io/instance=$REL" --tail=80 --prefix --all-containers
    echo "### clients"; kc -n "$JOBS_NS" get pods -o wide
  } >"$LOGS/diagnostics.txt" 2>&1
}

BG_PIDS=""
cleanup() {
  local pid
  for pid in $BG_PIDS; do kill "$pid" 2>/dev/null; done
  for pid in $BG_PIDS; do wait "$pid" 2>/dev/null; done
  if [ "$FAILED" != 0 ] || [ "$SETUP_FAILED" != 0 ]; then
    collect_diagnostics
    echo "logs and diagnostics: $LOGS (rerun with KEEP=1 to keep the cluster state)"
  fi
  if [ "$KEEP" = "1" ]; then
    echo; echo "KEEP=1: leaving namespaces $NS, $JOBS_NS and $STATE_DIR on the nodes (make e2e-clean removes them)"
    return
  fi
  echo; echo "== [$(elapsed)] cleanup"
  remove_release
  echo "   release removed at $(elapsed)"
  local ns
  for ns in "$NS" "$JOBS_NS" "$PSA_NS"; do
    if [ "$(kc get namespace "$ns" -o jsonpath="{.metadata.labels.ci-accelerator\.io/e2e}" 2>/dev/null)" = true ]; then
      kc delete namespace "$ns" --wait=true --timeout=180s >/dev/null 2>&1 || true
    fi
  done
  remove_state
  echo "   cleanup done at $(elapsed)"
  if [ "$FAILED" = 0 ] && [ "$SETUP_FAILED" = 0 ]; then rm -rf "$LOGS"; fi
}

if [ "$MODE" = clean ]; then
  KEEP=0
  trap cleanup EXIT
  exit 0
fi
trap cleanup EXIT

# --- setup -------------------------------------------------------------------

step "setup (context $CTX, image $IMAGE, scenarios: $SCENARIOS)"

BUILD_NODES=$(kc get nodes -l ci-accelerator.io/node-role=build -o jsonpath='{.items[*].metadata.name}')
BUILD_NODE=$(echo "$BUILD_NODES" | awk '{print $1}')
CP_NODE=$(kc get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[0].metadata.name}')
[ "$(echo "$BUILD_NODES" | wc -w)" -ge 2 ] || die "need at least 2 build nodes (run make kind-up)"
[ -n "$CP_NODE" ] || die "no control-plane node found"
info "build nodes: $BUILD_NODES; client node: $BUILD_NODE; node without a daemon: $CP_NODE"

remove_release
remove_state
ensure_ns "$NS" privileged
ensure_ns "$JOBS_NS" baseline
create_state
# The daemons' CA, and an unrelated one whose client certificate must be
# refused.
certs "$LOGS/tls"
certs "$LOGS/rogue"
put_secret "$NS" "$TLS_SECRET" "$LOGS/tls/server"
put_secret "$JOBS_NS" "$CLIENT_SECRET" "$LOGS/tls/client"

hc upgrade --install "$REL" "$CHART" -n "$NS" "${INSTALL_ARGS[@]}" \
  --wait --timeout 5m >"$LOGS/install.log" 2>&1 || { cat "$LOGS/install.log"; die "helm install"; }
info "release $REL installed (state in $HOST_PATH)"

for spec in "client-build:$BUILD_NODE" "client-cp:$CP_NODE"; do
  name=${spec%%:*}; node=${spec#*:}
  kc -n "$JOBS_NS" delete pod "$name" --ignore-not-found --wait=true >/dev/null
  # The client certificate is mounted from a Secret, as in a runner job pod.
  kc -n "$JOBS_NS" run "$name" --image="$IMAGE" --restart=Never \
    --overrides="{\"spec\":{\"nodeName\":\"$node\",\"terminationGracePeriodSeconds\":0,
      \"containers\":[{\"name\":\"$name\",\"image\":\"$IMAGE\",\"command\":[\"sleep\",\"86400\"],
        \"volumeMounts\":[{\"name\":\"certs\",\"mountPath\":\"/certs\",\"readOnly\":true}]}],
      \"volumes\":[{\"name\":\"certs\",\"secret\":{\"secretName\":\"$CLIENT_SECRET\"}}]}}" >/dev/null
done
kc -n "$JOBS_NS" wait --for=condition=Ready pod/client-build pod/client-cp --timeout=120s >/dev/null \
  || die "client pods not ready"
for name in client-build client-cp; do
  kc -n "$JOBS_NS" cp "$FIXTURES" "$name:/f" || die "copy fixtures to $name"
done
kc -n "$JOBS_NS" cp "$LOGS/rogue/client" client-build:/rogue || die "copy the rogue client certificate"
info "clients ready in namespace $JOBS_NS (PSA baseline)"
# Fail fast when the cluster itself is broken (DNS, kube-proxy, CNI): every
# scenario would otherwise time out one request at a time.
if ! eventually 30 has_answer client-build; then
  if quiet kc -n "$JOBS_NS" exec client-build -- buildctl --addr "$ADDR" --timeout 5 debug workers; then
    die "the daemons answer a plaintext client: they are not serving mutual TLS"
  fi
  die "a client cannot reach the daemons through the Service (check DNS and kube-proxy: kubectl -n kube-system get pods)"
fi
settle

SIGKILLED_POD=""
VICTIM=""

# --- scenarios ---------------------------------------------------------------

s_preflight() {
  step "preflight: installs that would end with zero daemons are refused"
  ensure_ns "$PSA_NS" baseline
  if hc install "$REL-psa" "$CHART" -n "$PSA_NS" --set "tls.existingSecret=$TLS_SECRET" >"$LOGS/psa.log" 2>&1; then
    fail "install into a baseline namespace succeeded"
    hc uninstall "$REL-psa" -n "$PSA_NS" >/dev/null 2>&1
  elif grep -q 'enforces Pod Security level "baseline"' "$LOGS/psa.log"; then
    pass "install into a PSA-baseline namespace refused with the fix in the message"
  else
    fail "install failed for another reason (see $LOGS/psa.log)"
  fi
  check "no release left behind" test -z "$(hc list -n "$PSA_NS" -q)"

  # --dry-run=server runs the lookups but creates nothing.
  if hc install "$REL-nonodes" "$CHART" -n "$NS" --dry-run=server "${INSTALL_ARGS[@]}" \
      --set 'global.buildNodes.selector.ci-accelerator\.io/node-role=none' >"$LOGS/nonodes.log" 2>&1; then
    fail "install with a selector no node matches succeeded"
  elif grep -q 'no node matches the build-node selector' "$LOGS/nonodes.log"; then
    pass "install with a selector no node matches refused"
  else
    fail "selector check failed for another reason (see $LOGS/nonodes.log)"
  fi

  # An installer that is admin of its namespace only: it may read its own
  # Namespace object (so the PSA check works), but not list Nodes.
  kc -n "$PSA_NS" create serviceaccount e2e-installer >/dev/null 2>&1
  kc -n "$PSA_NS" create rolebinding e2e-installer --clusterrole=admin \
    --serviceaccount="$PSA_NS:e2e-installer" >/dev/null 2>&1
  local ns_admin=(--kube-as-user "system:serviceaccount:$PSA_NS:e2e-installer" --set "tls.existingSecret=$TLS_SECRET")
  if hc install "$REL-ns" "$CHART" -n "$PSA_NS" --dry-run=server "${ns_admin[@]}" \
      --set preflight.buildNodes=false >"$LOGS/ns-admin-psa.log" 2>&1; then
    fail "namespace admin: PSA check did not refuse the baseline namespace"
  elif grep -q 'enforces Pod Security level "baseline"' "$LOGS/ns-admin-psa.log"; then
    pass "namespace admin: the PSA check works (own Namespace is readable)"
  else
    fail "namespace admin: PSA check failed for another reason (see $LOGS/ns-admin-psa.log)"
  fi
  if hc install "$REL-ns" "$CHART" -n "$PSA_NS" --dry-run=server "${ns_admin[@]}" \
      --set preflight.podSecurity=false >"$LOGS/ns-admin-nodes.log" 2>&1; then
    fail "namespace admin: listing nodes did not fail"
  elif grep -qi 'nodes is forbidden' "$LOGS/ns-admin-nodes.log"; then
    pass "namespace admin: the build-node check fails with Forbidden (needs cluster-wide node list)"
  else
    fail "namespace admin: node check failed for another reason (see $LOGS/ns-admin-nodes.log)"
  fi
  check "namespace admin: renders with both checks switched off" \
    quiet hc install "$REL-ns" "$CHART" -n "$PSA_NS" --dry-run=server "${ns_admin[@]}" \
      --set preflight.podSecurity=false --set preflight.buildNodes=false
  kc delete namespace "$PSA_NS" --wait=false >/dev/null
}

# refused <desc> <log> <buildctl args...>: a call from client-build that the
# TLS layer must turn away. A timeout or a refused connection would also
# "fail", so the error has to be about TLS. With TLS 1.3 the server rejects a
# missing client certificate after the client has finished its handshake, so
# the client only sees the connection die on its first write (broken pipe,
# reset); that case is paired with a control that differs only by the
# certificate.
refused() {
  local desc=$1 log=$2; shift 2
  kc -n "$JOBS_NS" exec client-build -- buildctl --addr "$ADDR" --timeout 5 "$@" debug workers -v >"$log" 2>&1
  if grep -q 'worker.hostname' "$log"; then
    fail "$desc: the daemon answered"
  elif grep -qiE 'tls|x509|certificate|handshake|server preface|broken pipe|connection reset' "$log"; then
    pass "$desc ($(grep -oiE 'tls: [a-z ]+|x509: [a-z ]+|server preface[a-z :]*|broken pipe|connection reset by peer' "$log" | head -1))"
  else
    fail "$desc: failed, but not in TLS (see $log)"
  fi
}

s_tls() {
  step "tls: only clients with a certificate from the daemons' CA get in"
  # Positive control: without it the refusals below prove nothing.
  check "a client with a certificate from the CA is served" has_answer client-build
  refused "plaintext client refused" "$LOGS/tls-plain.log"
  check "control: the same flags plus the client certificate are served" \
    quiet kc -n "$JOBS_NS" exec client-build -- buildctl --addr "$ADDR" --timeout 5 \
      --tlscacert /certs/ca.crt --tlscert /certs/tls.crt --tlskey /certs/tls.key --tlsservername "$SVC_HOST" debug workers
  refused "client without a certificate refused" "$LOGS/tls-nocert.log" \
    --tlscacert /certs/ca.crt --tlsservername "$SVC_HOST"
  refused "client certificate from another CA refused" "$LOGS/tls-rogue.log" \
    --tlscacert /certs/ca.crt --tlscert /rogue/tls.crt --tlskey /rogue/tls.key --tlsservername "$SVC_HOST"
  # The client side checks the daemon too.
  refused "a client trusting another CA rejects the daemon" "$LOGS/tls-distrust.log" \
    --tlsdir /rogue --tlsservername "$SVC_HOST"
  refused "a name the server certificate does not cover is rejected" "$LOGS/tls-name.log" \
    --tlsdir /certs --tlsservername wrong.example
  next_id
  build client-build tls --opt "build-arg:BUST=$ID" --opt "build-arg:BUILDKIT_IMAGE=$IMAGE" >"$LOGS/tls-run.log" 2>&1
  check "a RUN step on the daemon builds" test $? = 0
  check "the RUN step reaches the daemon's port 1234" printed "$LOGS/tls-run.log" PORT_OPEN
  check "the RUN step cannot drive its own daemon without a certificate" printed "$LOGS/tls-run.log" DAEMON_REFUSED
  # `kubectl create secret tls` writes no ca.crt; the chart must say so
  # instead of leaving pods in ContainerCreating.
  kc -n "$NS" delete secret "$REL-noca" --ignore-not-found >/dev/null
  kc -n "$NS" create secret tls "$REL-noca" --cert "$LOGS/tls/server/tls.crt" --key "$LOGS/tls/server/tls.key" >/dev/null
  if hc install "$REL-noca" "$CHART" -n "$NS" --dry-run=server "${INSTALL_ARGS[@]}" \
      --set "tls.existingSecret=$REL-noca" >"$LOGS/tls-noca.log" 2>&1; then
    fail "install with a Secret from \`kubectl create secret tls\` succeeded"
  elif grep -q "has no ca.crt" "$LOGS/tls-noca.log"; then
    pass "install with a Secret without ca.crt refused with the fix in the message"
  else
    fail "Secret check failed for another reason (see $LOGS/tls-noca.log)"
  fi
  kc -n "$NS" delete secret "$REL-noca" >/dev/null
}

s_cache() {
  step "cache: the second build reuses layers and the cache mount"
  local e1 e2
  next_id; build client-build cache --opt "build-arg:BUST=$ID" >"$LOGS/cache-1.log" 2>&1; e1=$?
  next_id; build client-build cache --opt "build-arg:BUST=$ID" >"$LOGS/cache-2.log" 2>&1; e2=$?
  if [ "$e1" = 0 ] && [ "$e2" = 0 ]; then pass "both builds succeed"; else fail "build exit codes $e1/$e2"; fi
  # Without a cold baseline the warm checks would prove nothing.
  if ! deps_cached "$LOGS/cache-1.log" && printed "$LOGS/cache-1.log" CACHE_MOUNT_COLD; then
    pass "first build runs cold (no state from earlier runs)"
  else
    fail "first build was not cold: stale state"
  fi
  check "deps step CACHED on the second build" deps_cached "$LOGS/cache-2.log"
  check "cache mount WARM on the second build" printed "$LOGS/cache-2.log" CACHE_MOUNT_WARM
}

s_dedup() {
  step "dedup: two concurrent builds execute a shared step once"
  settle
  local seed pa pb ea eb took ip runs total
  next_id; seed=$ID
  SECONDS=0
  build client-build dedup --opt target=main --opt "build-arg:SEED=$seed" --opt build-arg:WHO=A >"$LOGS/dedup-a.log" 2>&1 & pa=$!
  build client-build dedup --opt target=main --opt "build-arg:SEED=$seed" --opt build-arg:WHO=B >"$LOGS/dedup-b.log" 2>&1 & pb=$!
  BG_PIDS="$BG_PIDS $pa $pb"
  ea=0; eb=0; wait "$pa" || ea=$?; wait "$pb" || eb=$?
  took=$SECONDS
  if [ "$ea" = 0 ] && [ "$eb" = 0 ]; then pass "both builds succeed"; else fail "build exit codes $ea/$eb"; fi
  check "the builds overlapped (${took}s for two 8 s steps)" test "$took" -lt 16
  if grep -A1 'target=/count' "$LOGS/dedup-a.log" "$LOGS/dedup-b.log" | grep -q 'CACHED$'; then
    fail "the shared step came from cache: the check proves nothing"
  else
    pass "neither build took the shared step from cache"
  fi
  # Count executions on every daemon: a fallback to another node would
  # otherwise hide a second execution.
  total=0
  for ip in $(daemon_ips); do
    next_id
    build_at client-build "tcp://$ip:1234" dedup --opt target=read --opt "build-arg:READ_BUST=$ID" >"$LOGS/dedup-read-$ip.log" 2>&1
    runs=$(count_printed "$LOGS/dedup-read-$ip.log" "exec seed=$seed")
    total=$((total + runs))
  done
  check "shared step executed once across all daemons ($total)" test "$total" = 1
}

s_routing() {
  step "routing (PreferSameNode): a client reaches its own node's daemon"
  settle
  local local_daemon ok answer
  local_daemon=$(daemon_on "$BUILD_NODE")
  ok=1
  for _ in 1 2 3 4 5 6 7 8 9 10; do [ "$(who client-build)" = "$local_daemon" ] || ok=0; done
  if [ "$ok" = 1 ] && [ -n "$local_daemon" ]; then
    pass "10/10 requests answered by $local_daemon on $BUILD_NODE"
  else
    fail "requests left the node"
  fi
  answer=$(who client-cp)
  check "client on a node without a daemon falls back (${answer:-nobody})" test -n "$answer"
}

s_local() {
  step "routing (Local): strict node-local, no fallback"
  local local_daemon
  local_daemon=$(daemon_on "$BUILD_NODE")
  hc upgrade "$REL" "$CHART" -n "$NS" "${INSTALL_ARGS[@]}" --set service.routing=Local \
    --wait --timeout 3m >"$LOGS/upgrade-local.log" 2>&1 || fail "upgrade to routing=Local"
  check "client on a node without a daemon gets no answer (timeout/refused)" eventually 15 no_answer client-cp
  check "client on a build node is still served locally" test "$(who client-build)" = "$local_daemon"
  hc upgrade "$REL" "$CHART" -n "$NS" "${INSTALL_ARGS[@]}" \
    --wait --timeout 3m >"$LOGS/upgrade-back.log" 2>&1 || fail "upgrade back to defaults"
  eventually 15 has_answer client-cp || true
  settle
}

s_bump() {
  step "upgrades: a chart-version-only bump does not restart the daemons"
  local before after gen_before gen_after bumped
  before=$(pod_uids)
  gen_before=$(kc -n "$NS" get ds "$REL-buildkit" -o jsonpath='{.metadata.generation}')
  bumped=$LOGS/chart-bump/buildkit
  mkdir -p "$LOGS/chart-bump" && cp -R "$CHART" "$LOGS/chart-bump/"
  sed -i.bak 's/^version: .*/version: 0.0.0-e2e.bump/' "$bumped/Chart.yaml"
  hc upgrade "$REL" "$bumped" -n "$NS" "${INSTALL_ARGS[@]}" --wait --timeout 3m >"$LOGS/bump.log" 2>&1 \
    || fail "upgrade to the bumped chart"
  sleep 5
  after=$(pod_uids)
  gen_after=$(kc -n "$NS" get ds "$REL-buildkit" -o jsonpath='{.metadata.generation}')
  check "same daemon pods after the bump" test "$before" = "$after"
  check "DaemonSet spec unchanged (generation $gen_before -> $gen_after)" test "$gen_before" = "$gen_after"
  hc upgrade "$REL" "$CHART" -n "$NS" "${INSTALL_ARGS[@]}" --wait --timeout 3m >"$LOGS/unbump.log" 2>&1 \
    || fail "upgrade back to the chart"
  check "same daemon pods after going back" test "$before" = "$(pod_uids)"
}

s_graceful() {
  step "graceful stop: a running build survives SIGTERM, new requests go elsewhere"
  settle
  local victim victim_ip plong elong t_del fails other out finished_after
  victim=$(daemon_on "$BUILD_NODE")
  victim_ip=$(pod_ip "$victim")
  VICTIM=$victim
  next_id
  SECONDS=0
  # Straight to the victim: the established stream survives the listener close.
  build_at client-build "tcp://$victim_ip:1234" long --opt "build-arg:BUST=$ID" >"$LOGS/long.log" 2>&1 & plong=$!
  BG_PIDS="$BG_PIDS $plong"
  sleep 8
  t_del=$SECONDS
  kc -n "$NS" delete pod "$victim" --wait=false >/dev/null
  info "t+${t_del}s deleted $victim (preStop sleep, then SIGTERM)"
  # The drain window: new requests through the Service must not be refused.
  fails=0; other=""
  for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do
    out=$(who client-build)
    if [ -z "$out" ]; then fails=$((fails + 1)); elif [ "$out" != "$victim" ]; then other=$out; fi
    sleep 0.5
  done
  check "no request failed during the drain ($fails failures)" test "$fails" = 0
  check "new requests went to another daemon (${other:-none})" test -n "$other"
  elong=0; wait "$plong" || elong=$?
  finished_after=$((SECONDS - t_del))
  if [ "$elong" = 0 ] && printed "$LOGS/long.log" finished; then
    pass "the 45 s build finished ${finished_after}s after the deletion"
  else
    fail "running build aborted (exit $elong, see $LOGS/long.log)"
  fi
  # More than the 30 s default: the chart's terminationGracePeriodSeconds is applied.
  check "it outlived the Kubernetes default grace (30 s)" test "$finished_after" -gt 30
  kc -n "$NS" rollout status "ds/$REL-buildkit" --timeout=180s >/dev/null || fail "daemon not replaced"
  eventually 30 has_daemon "$BUILD_NODE" || true
  info "replacement daemon on $BUILD_NODE: $(daemon_on "$BUILD_NODE")"
  settle
}

s_persist() {
  step "hostPath: a replacement daemon starts with the old cache"
  settle
  next_id; build client-build cache --opt "build-arg:BUST=$ID" >"$LOGS/cache-3.log" 2>&1
  check "build on the current daemon succeeds" test $? = 0
  check "deps step still CACHED" deps_cached "$LOGS/cache-3.log"
  check "cache mount still WARM" printed "$LOGS/cache-3.log" CACHE_MOUNT_WARM
  check "traffic on the local daemon" test "$(who client-build)" = "$(daemon_on "$BUILD_NODE")"
}

s_sigkill() {
  step "crash: SIGKILL in the middle of a build (an OOM kill looks the same)"
  settle
  local d ip before cid pid p e
  d=$(daemon_on "$BUILD_NODE"); ip=$(pod_ip "$d")
  before=$(kc -n "$NS" get pod "$d" -o jsonpath='{.status.containerStatuses[0].restartCount}')
  next_id
  build_at client-build "tcp://$ip:1234" long --opt "build-arg:BUST=$ID" >"$LOGS/sigkill-build.log" 2>&1 & p=$!
  BG_PIDS="$BG_PIDS $p"
  sleep 6
  # Kill exactly this container's buildkitd (other releases may run on the node).
  cid=$(kc -n "$NS" get pod "$d" -o jsonpath='{.status.containerStatuses[0].containerID}' | sed 's|.*://||')
  pid=$(docker exec "$BUILD_NODE" crictl inspect -o go-template --template '{{.info.pid}}' "$cid" 2>/dev/null)
  if [ -z "$pid" ] || ! docker exec "$BUILD_NODE" kill -9 "$pid"; then
    fail "could not SIGKILL $d"; return
  fi
  SIGKILLED_POD=$d
  e=0; wait "$p" || e=$?
  check "the running build fails (exit $e)" test "$e" != 0
  restarted() {
    [ "$(kc -n "$NS" get pod "$d" -o jsonpath='{.status.containerStatuses[0].restartCount}')" -gt "$before" ] \
      && [ "$(kc -n "$NS" get pod "$d" -o jsonpath='{.status.containerStatuses[0].ready}')" = true ]
  }
  check "$d restarted in place and is ready again" eventually 60 restarted
  kc -n "$NS" logs "$d" 2>/dev/null | grep -iE 'reset|corrupt|recover|bak' | head -3 | while read -r line; do info "log: $line"; done
  settle
  next_id; build client-build cache --opt "build-arg:BUST=$ID" >"$LOGS/cache-4.log" 2>&1
  check "build after the crash succeeds" test $? = 0
  check "layer cache survived the SIGKILL" deps_cached "$LOGS/cache-4.log"
  check "cache mount survived the SIGKILL" printed "$LOGS/cache-4.log" CACHE_MOUNT_WARM
}

s_badconfig() {
  step "bad upgrade: a config that crashes buildkitd takes down one node only"
  local rev bad desired available updated broken
  rev=$(hc history "$REL" -n "$NS" --max 1 -o yaml | awk '/^ *revision:/ {print $NF}')
  bad=$LOGS/badconfig.yaml
  # Valid TOML (the chart's render-time check passes), wrong type for buildkit.
  printf 'config:\n  extraConfig: |\n    [registry."e2e.invalid"]\n      mirrors = 5\n' >"$bad"
  hc upgrade "$REL" "$CHART" -n "$NS" "${INSTALL_ARGS[@]}" -f "$bad" >"$LOGS/badconfig-upgrade.log" 2>&1 \
    || fail "helm upgrade with the bad config"
  stalled() {
    desired=$(kc -n "$NS" get ds "$REL-buildkit" -o jsonpath='{.status.desiredNumberScheduled}')
    available=$(kc -n "$NS" get ds "$REL-buildkit" -o jsonpath='{.status.numberAvailable}')
    updated=$(kc -n "$NS" get ds "$REL-buildkit" -o jsonpath='{.status.updatedNumberScheduled}')
    [ "$updated" = 1 ] && [ "$available" = $((desired - 1)) ]
  }
  if eventually 30 stalled; then
    sleep 20
    if stalled; then
      pass "rollout stalled after one node ($available/$desired daemons available)"
    else
      fail "bad config spread: available=$available updated=$updated of $desired"
    fi
  else
    fail "rollout did not stall as expected: available=$available updated=$updated of $desired"
  fi
  broken=$(kc -n "$NS" get pods -l "app.kubernetes.io/instance=$REL" \
    -o jsonpath='{range .items[*]}{.metadata.name} {.status.containerStatuses[0].ready}{"\n"}{end}' | awk '$2 != "true" {print $1; exit}')
  if kc -n "$NS" logs "$broken" 2>/dev/null | grep -qiE 'failed to (load|parse) config|cannot decode|mirrors'; then
    pass "the broken daemon logs the config error"
  else
    info "logs of $broken: $(kc -n "$NS" logs "$broken" 2>&1 | tail -2 | tr '\n' ' ')"
    fail "no config error in the broken daemon's log"
  fi
  next_id; build client-build cache --opt "build-arg:BUST=$ID" >"$LOGS/cache-bad.log" 2>&1
  check "builds are still served meanwhile" test $? = 0
  hc rollback "$REL" "$rev" -n "$NS" --wait --timeout 5m >"$LOGS/rollback.log" 2>&1 || fail "helm rollback"
  kc -n "$NS" rollout status "ds/$REL-buildkit" --timeout=180s >/dev/null
  available=$(kc -n "$NS" get ds "$REL-buildkit" -o jsonpath='{.status.numberAvailable}')
  desired=$(kc -n "$NS" get ds "$REL-buildkit" -o jsonpath='{.status.desiredNumberScheduled}')
  check "helm rollback restores every daemon ($available/$desired)" test "$available" = "$desired"
  settle
}

s_gc() {
  step "gc: buildkit enforces maxUsedSpace (1GB in the e2e values)"
  settle
  local d ip cap bytes
  d=$(daemon_on "$BUILD_NODE"); ip=$(pod_ip "$d")
  cap=1073741824
  next_id
  build_at client-build "tcp://$ip:1234" gc --opt "build-arg:BUST=$ID" >"$LOGS/gc.log" 2>&1
  check "a build writing 1.2 GB of layers succeeds" test $? = 0
  info "cache right after the build: $(( $(cache_bytes "$ip") / 1048576 )) MiB"
  under_cap() { bytes=$(cache_bytes "$ip"); [ -n "$bytes" ] && [ "$bytes" -le "$cap" ]; }
  # GC runs about a second after a build, at most once a minute.
  if eventually 45 under_cap; then
    pass "GC brought the cache under the cap ($((bytes / 1048576)) MiB <= 1024 MiB)"
  else
    fail "cache still above the cap after 90 s ($((bytes / 1048576)) MiB)"
  fi
}

s_debug() {
  step "debug server: not reachable, not even from a RUN step on the daemon"
  local ip
  ip=$(pod_ip "$(daemon_on "$BUILD_NODE")")
  next_id; build client-build debug --opt "build-arg:BUST=$ID" >"$LOGS/debug.log" 2>&1
  check "a RUN step finds :6060 closed" printed "$LOGS/debug.log" DEBUG_CLOSED
  if kc -n "$JOBS_NS" exec client-build -- nc -z -w 2 "$ip" 6060 >/dev/null 2>&1; then
    fail "the daemon pod answers on :6060"
  else
    pass "the daemon pod does not answer on :6060"
  fi
}

s_probes() {
  step "probes: healthy for the whole run with a dead OTLP backend"
  local restarts unhealthy probe_pod out rc probe_s plain_s
  check "the hostile OTEL env is deployed" \
    test -n "$(kc -n "$NS" get ds "$REL-buildkit" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="OTEL_TRACES_EXPORTER")].value}')"
  restarts=$(kc -n "$NS" get pods -l "app.kubernetes.io/instance=$REL" \
    -o jsonpath='{range .items[*]}{.metadata.name} {.status.containerStatuses[0].restartCount}{"\n"}{end}' \
    | awk -v k="$SIGKILLED_POD" '$1 != k {s += $2} END {print s + 0}')
  check "0 unexpected daemon restarts" test "$restarts" = 0
  # Readiness or liveness failures of this run's daemons; a draining or
  # SIGKILLed daemon legitimately fails readiness for a moment.
  unhealthy=$(kc -n "$NS" get events --field-selector reason=Unhealthy \
    -o jsonpath='{range .items[*]}{.involvedObject.name}{"\t"}{.message}{"\n"}{end}' \
    | awk -F'\t' -v a="$VICTIM" -v b="$SIGKILLED_POD" '$1 != a && $1 != b && $2 ~ /^(Readiness|Liveness|Startup) probe/' | wc -l | tr -d ' ')
  check "no probe failures ($unhealthy)" test "$unhealthy" = 0
  probe_pod=$(daemon_on "$BUILD_NODE")
  # Run the liveness command exactly as deployed, not a copy of it.
  probe_cmd=()
  while IFS= read -r arg; do probe_cmd+=("$arg"); done < <(kc -n "$NS" get "ds/$REL-buildkit" \
    -o jsonpath='{range .spec.template.spec.containers[0].livenessProbe.exec.command[*]}{@}{"\n"}{end}')
  if [ "${#probe_cmd[@]}" -eq 0 ]; then
    fail "no liveness command found"; return
  fi
  SECONDS=0
  out=$(kc -n "$NS" exec "$probe_pod" -- "${probe_cmd[@]}" 2>&1); rc=$?
  probe_s=$SECONDS
  if [ "$rc" = 0 ] && printf '%s' "$out" | grep -q 'linux/' && [ "$probe_s" -le 3 ]; then
    pass "deployed probe command answers in ${probe_s}s"
  else
    fail "deployed probe command: rc=$rc after ${probe_s}s"
  fi
  # Positive control: without the guard the same call waits for the trace
  # backend; otherwise this scenario proves nothing.
  SECONDS=0
  kc -n "$NS" exec "$probe_pod" -- buildctl debug workers >/dev/null 2>&1
  plain_s=$SECONDS
  check "control: plain buildctl with the deployed env takes ${plain_s}s (>= 5)" test "$plain_s" -ge 5
}

DURATIONS=""
for s in $ALL_SCENARIOS; do
  if want "$s"; then
    t0=$(date +%s); "s_$s"; DURATIONS="$DURATIONS $s=$(( $(date +%s) - t0 ))s"
  fi
done

# --- result ------------------------------------------------------------------

echo
echo "== result: $PASSED passed, $FAILED failed in $(elapsed) (scenarios:$DURATIONS)"
if [ "$(sysctl -n kern.waketime 2>/dev/null || true)" != "$WAKE_AT_START" ]; then
  echo "WARNING: the host slept during this run; durations include the sleep and timing checks are not meaningful"
fi
printf "%b\n" "$SUMMARY"
[ "$FAILED" = 0 ]
