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
# ones, E2E_RUNNER_FRAGMENT=<file> replaces integrations/gitlab-runner/values.yaml,
# E2E_LOGS=<dir> writes the logs there instead of a temporary directory,
# E2E_BOX_EXTRA_VALUES="<file> ..." adds values files to the umbrella install. A deliberately broken chart or value must turn scenarios red.
#
# Installs its own release ("e2e") in its own namespace, so a development
# release in the same cluster is not touched, and runs CI-job-like clients
# from a second namespace with Pod Security "baseline", the way runner job
# pods will. The daemons run with hostPath state, small GC caps, a dead
# OpenTelemetry backend and mutual TLS from the chart's default source:
# cert-manager (installed into the cluster when missing, and left there as
# the prerequisite it is) issues a dedicated CA, the daemon certificate and a
# client certificate that the clients mount, as job pods will. The
# existingSecret source is exercised by switching to it and back. The
# daemons run behind their NetworkPolicy; a second, short-lived release
# pushes metrics and traces to a throwaway VictoriaMetrics/VictoriaTraces.
# A real gitlab-runner (official chart + integrations/gitlab-runner) takes
# jobs from a stand-in GitLab API (test/e2e/fake-gitlab) and runs buildctl
# in job pods it creates; so does the umbrella chart (charts/ci-accelerator),
# installed as a whole into its own namespaces. Never run two e2e runs against the
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
OTHER_NS=$NS-other
OBS_NS=$NS-obs
BOX_NS=$NS-box
BOX_JOBS_NS=$NS-box-jobs
REL=e2e
OTEL_REL=$REL-otel
# Not "box": a manual install of that name may share the cluster, and
# cleanup deletes CA Secrets by release label.
BOX_REL=e2e-box
SVC_HOST=$REL-buildkit.$NS.svc
ADDR=tcp://$SVC_HOST:1234
CLIENT_SECRET=$REL-buildkit-client
OWN_SECRET=$REL-own-tls
CM_NS=cert-manager
CA_NAME=$NS-$REL-buildkit-ca
CERT_MANAGER_VERSION=v1.21.2
# sha256 of the chart archive pulled from oci://quay.io/jetstack/charts.
CERT_MANAGER_SHA256=73a56e1728edd6c99f1f31082618c3259d279a76b7ebd3d4bdc5475c2442d34a
# OTLP backends for the otel scenario, from the VictoriaMetrics Helm repo.
VICTORIA_REPO=https://victoriametrics.github.io/helm-charts/
VM_CHART_VERSION=0.48.0
VM_CHART_SHA256=51d26125182388a734f5d9dc65086811436a67d5aede08fde5f2a4247b532381
VT_CHART_VERSION=0.1.11
VT_CHART_SHA256=59471db9c6e053fc740b1ad0eb6365dc872bba3f0c9e06f5780ddde9655c9e88
# The official gitlab-runner chart the integration fragment is written for.
RUNNER_REPO=https://charts.gitlab.io
RUNNER_CHART_VERSION=0.93.0
RUNNER_CHART_SHA256=a8932d04c94724aa93e3147811696e3512df9cbaa5901818d2b3872b0f02f0a9
# The stand-in GitLab API runs on this image (python stdlib only).
PYTHON_IMAGE=python:3.13-alpine3.22@sha256:e81548ac35b07a3bd4805f275107592ef458b1e893c0e04d45aedaa19416cca5
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
ALL_SCENARIOS="preflight tls certmanager existing netpol otel runner box cache dedup routing local bump graceful persist sigkill badconfig gc debug probes"

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
# E2E_LOGS=<dir> keeps the logs in a known place (CI uploads it on failure).
if [ -n "${E2E_LOGS:-}" ]; then
  LOGS=$E2E_LOGS; mkdir -p "$LOGS"
else
  LOGS=$(mktemp -d "${TMPDIR:-/tmp}/ci-accelerator-e2e.XXXXXX")
fi

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

BASE_ARGS=(-f "$VALUES")
for extra in ${E2E_EXTRA_VALUES:-}; do BASE_ARGS+=(-f "$extra"); done
BASE_ARGS+=(--set "storage.hostPath.path=$HOST_PATH" --set "networkPolicy.ingress.namespaces={$JOBS_NS}")
# Helm applies --set after --set-json whatever the order, so the client
# namespaces stay out of BASE_ARGS for installs that must clear them.
INSTALL_ARGS=("${BASE_ARGS[@]}" --set "tls.client.namespaces={$JOBS_NS}")

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
  local r
  if hc status gitlab-runner -n "$NS" >/dev/null 2>&1; then
    hc uninstall gitlab-runner -n "$NS" --wait --timeout 3m >/dev/null 2>&1 || true
  fi
  if hc status "$BOX_REL" -n "$BOX_NS" >/dev/null 2>&1; then
    hc uninstall "$BOX_REL" -n "$BOX_NS" --wait --timeout 5m >/dev/null 2>&1 || true
  fi
  # The box's jobs namespace belongs to its release; remove a leftover one.
  if [ "$(kc get namespace "$BOX_JOBS_NS" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/instance}' 2>/dev/null)" = "$BOX_REL" ]; then
    kc delete namespace "$BOX_JOBS_NS" --wait=true --timeout=180s >/dev/null 2>&1 || true
  fi
  kc -n "$CM_NS" delete secret -l "app.kubernetes.io/instance=$BOX_REL,ci-accelerator.io/component=buildkit" \
    --ignore-not-found >/dev/null 2>&1 || true
  for r in "$REL" "$OTEL_REL"; do
    if hc status "$r" -n "$NS" >/dev/null 2>&1; then
      hc uninstall "$r" -n "$NS" --wait --timeout 5m >/dev/null 2>&1 || true
    fi
    kc -n "$NS" wait --for=delete pod -l "app.kubernetes.io/instance=$r" --timeout=300s >/dev/null 2>&1 || true
    # The CA key Secret outlives its Certificate (cert-manager does not
    # delete Secrets), in cert-manager's namespace.
    kc -n "$CM_NS" delete secret -l "app.kubernetes.io/instance=$r,ci-accelerator.io/component=buildkit" \
      --ignore-not-found >/dev/null 2>&1 || true
  done
}

sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }

# cert-manager is a prerequisite of the chart, never part of it. Install the
# pinned version when the cluster has none; an existing one is used as is.
ensure_cert_manager() {
  # A working cert-manager is used as it is, whoever installed it. Only a
  # missing one is installed, or a release left broken by an earlier run is
  # repaired.
  if kc -n "$CM_NS" rollout status deploy/cert-manager-webhook --timeout=5s >/dev/null 2>&1; then
    info "cert-manager present: $(kc -n "$CM_NS" get deploy cert-manager -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)"
  else
    local tgz=$LOGS/cert-manager-$CERT_MANAGER_VERSION.tgz
    helm pull oci://quay.io/jetstack/charts/cert-manager --version "$CERT_MANAGER_VERSION" -d "$LOGS" >/dev/null 2>&1 \
      || die "pull the cert-manager chart"
    echo "$CERT_MANAGER_SHA256  $tgz" | sha256 -c - >/dev/null 2>&1 || die "cert-manager chart checksum mismatch"
    hc upgrade --install cert-manager "$tgz" -n "$CM_NS" --create-namespace \
      -f "$ROOT/test/e2e/cert-manager-values.yaml" --wait --timeout 5m >"$LOGS/cert-manager.log" 2>&1 \
      || { cat "$LOGS/cert-manager.log"; die "install cert-manager"; }
    info "installed cert-manager $CERT_MANAGER_VERSION into $CM_NS (left there: a cluster-wide prerequisite)"
  fi
  kc -n "$CM_NS" rollout status deploy/cert-manager-webhook --timeout=180s >/dev/null || die "cert-manager webhook not ready"
}

collect_diagnostics() {
  {
    echo "### daemons"; kc -n "$NS" get ds,pods -o wide
    echo "### daemonset"; kc -n "$NS" describe ds "$REL-buildkit" | tail -40
    echo "### events"; kc -n "$NS" get events --sort-by=.lastTimestamp | tail -60
    echo "### logs"; kc -n "$NS" logs -l "app.kubernetes.io/instance=$REL" --tail=80 --prefix --all-containers
    echo "### clients"; kc -n "$JOBS_NS" get pods -o wide
    echo "### certificates"; kc get certificates,certificaterequests -A -l "app.kubernetes.io/instance=$REL" -o wide
    kc describe certificates -A -l "app.kubernetes.io/instance=$REL" | grep -A8 '^Events' 
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
    echo; echo "KEEP=1: leaving namespaces $NS, $JOBS_NS (and $OTHER_NS, $OBS_NS) and $STATE_DIR on the nodes (make e2e-clean removes them)"
    return
  fi
  echo; echo "== [$(elapsed)] cleanup"
  remove_release
  echo "   release removed at $(elapsed)"
  local ns
  for ns in "$NS" "$JOBS_NS" "$PSA_NS" "$OTHER_NS" "$OBS_NS" "$BOX_NS"; do
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
ensure_cert_manager
# A CA for the existingSecret source, and an unrelated one whose client
# certificate must be refused.
certs "$LOGS/tls"
certs "$LOGS/rogue"

hc upgrade --install "$REL" "$CHART" -n "$NS" "${INSTALL_ARGS[@]}" \
  --wait --timeout 5m >"$LOGS/install.log" 2>&1 || { cat "$LOGS/install.log"; die "helm install"; }
info "release $REL installed (state in $HOST_PATH)"
eventually 30 quiet kc -n "$JOBS_NS" get secret "$CLIENT_SECRET" || die "cert-manager issued no client certificate into $JOBS_NS"

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
kc -n "$JOBS_NS" cp "$LOGS/tls/client" client-build:/own || die "copy the existingSecret client certificate"
info "clients ready in namespace $JOBS_NS (PSA baseline)"
# Fail fast when the cluster itself is broken (DNS, kube-proxy, CNI): every
# scenario would otherwise time out one request at a time.
if ! eventually 30 has_answer client-build; then
  if quiet kc -n "$JOBS_NS" exec client-build -- buildctl --addr "$ADDR" --timeout 5 debug workers; then
    die "the daemons answer a plaintext client: they are not serving mutual TLS"
  fi
  last=$(kc -n "$JOBS_NS" exec client-build -- buildctl --addr "$ADDR" "${TLS[@]}" --timeout 5 debug workers 2>&1 | grep -v "command terminated" | tail -1)
  die "a client cannot reach the daemons through the Service: $last (a TLS error or a reset connection points at the certificates; a timeout at DNS or kube-proxy: kubectl -n kube-system get pods)"
fi
settle

SIGKILLED_POD=""

# --- scenarios ---------------------------------------------------------------

# For installs that test something other than TLS: a provided Secret, so no
# cert-manager objects are created (a namespace admin may not create them in
# cert-manager's namespace).
# (--set x={} would set [""]; combine with BASE_ARGS, not INSTALL_ARGS.)
NO_CM=(--set tls.source=existingSecret --set "tls.existingSecret=$OWN_SECRET" --set-json 'tls.client.namespaces=[]')

s_preflight() {
  step "preflight: installs that would end with zero daemons are refused"
  ensure_ns "$PSA_NS" baseline
  if hc install "$REL-psa" "$CHART" -n "$PSA_NS" "${NO_CM[@]}" >"$LOGS/psa.log" 2>&1; then
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
  local ns_admin=(--kube-as-user "system:serviceaccount:$PSA_NS:e2e-installer" "${NO_CM[@]}")
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
  if hc install "$REL-noca" "$CHART" -n "$NS" --dry-run=server "${BASE_ARGS[@]}" "${NO_CM[@]}" \
      --set "tls.existingSecret=$REL-noca" >"$LOGS/tls-noca.log" 2>&1; then
    fail "install with a Secret from \`kubectl create secret tls\` succeeded"
  elif grep -q "has no ca.crt" "$LOGS/tls-noca.log"; then
    pass "install with a Secret without ca.crt refused with the fix in the message"
  else
    fail "Secret check failed for another reason (see $LOGS/tls-noca.log)"
  fi
  kc -n "$NS" delete secret "$REL-noca" >/dev/null
}

# pem <namespace> <secret> <key>: one PEM value of a Secret.
pem() { kc -n "$1" get secret "$2" -o jsonpath="{.data.${3//./\\.}}" | base64 -d; }
# lifetime_days <pem file> <days>: valid for (days - 1) more days, not days + 1.
lifetime_days() {
  openssl x509 -in "$1" -noout -checkend $(( ($2 - 1) * 86400 )) >/dev/null \
    && ! openssl x509 -in "$1" -noout -checkend $(( ($2 + 1) * 86400 )) >/dev/null
}

s_certmanager() {
  step "cert-manager: a dedicated CA, daemon and client certificates as specified"
  local spec ready d=$LOGS/issued
  for spec in "$CM_NS/$CA_NAME" "$NS/$REL-buildkit-tls" "$JOBS_NS/$CLIENT_SECRET"; do
    ready=$(kc -n "${spec%%/*}" get certificate "${spec#*/}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
    check "Certificate $spec is Ready" test "$ready" = True
  done
  # The API server prunes fields a CRD does not know: check they arrived.
  check "the daemon certificate keeps renewBeforePercentage 33" \
    test "$(kc -n "$NS" get certificate "$REL-buildkit-tls" -o jsonpath='{.spec.renewBeforePercentage}')" = 33
  check "the CA keeps its key on renewal (rotationPolicy Never)" \
    test "$(kc -n "$CM_NS" get certificate "$CA_NAME" -o jsonpath='{.spec.privateKey.rotationPolicy}')" = Never
  # What was actually issued.
  mkdir -p "$d"
  pem "$CM_NS" "$CA_NAME" tls.crt >"$d/ca.crt"
  pem "$NS" "$REL-buildkit-tls" tls.crt >"$d/server.crt"
  pem "$NS" "$REL-buildkit-tls" ca.crt >"$d/server-ca.crt"
  pem "$JOBS_NS" "$CLIENT_SECRET" tls.crt >"$d/client.crt"
  pem "$JOBS_NS" "$CLIENT_SECRET" ca.crt >"$d/client-ca.crt"
  check "daemon and client Secrets carry the release's own CA" \
    eval 'cmp -s "$d/ca.crt" "$d/server-ca.crt" && cmp -s "$d/ca.crt" "$d/client-ca.crt"'
  check "the CA is a CA, valid ~10 years" \
    eval 'openssl x509 -in "$d/ca.crt" -noout -text | grep -q "CA:TRUE" && lifetime_days "$d/ca.crt" 3650'
  openssl x509 -in "$d/server.crt" -noout -text >"$d/server.txt"
  openssl x509 -in "$d/client.crt" -noout -text >"$d/client.txt"
  check "daemon certificate: server auth only, ECDSA, covers $SVC_HOST" \
    eval 'grep -q "TLS Web Server Authentication" "$d/server.txt" && ! grep -q "Client Authentication" "$d/server.txt" \
      && grep -q "id-ecPublicKey" "$d/server.txt" && grep -q "DNS:$SVC_HOST," "$d/server.txt"'
  check "client certificate: client auth only" \
    eval 'grep -q "TLS Web Client Authentication" "$d/client.txt" && ! grep -q "Server Authentication" "$d/client.txt"'
  check "both valid ~1 year" eval 'lifetime_days "$d/server.crt" 365 && lifetime_days "$d/client.crt" 365'
  check "both chain to the CA" quiet openssl verify -CAfile "$d/ca.crt" "$d/server.crt" "$d/client.crt"
  # cert-manager is a prerequisite: rendering without its API must say how
  # to get it, also offline.
  if helm template "$REL" "$CHART" -n "$NS" >"$LOGS/no-cm.log" 2>&1; then
    fail "rendered without the cert-manager API"
  elif grep -q 'helm install cert-manager oci://quay.io/jetstack/charts/cert-manager --version' "$LOGS/no-cm.log"; then
    pass "rendering without the cert-manager API is refused with the install command"
  else
    fail "rendering without the cert-manager API failed for another reason (see $LOGS/no-cm.log)"
  fi
  check "offline rendering works with --api-versions cert-manager.io/v1" \
    quiet helm template "$REL" "$CHART" -n "$NS" --api-versions cert-manager.io/v1
}

s_existing() {
  step "existingSecret: the daemons switch to a provided Secret and its CA, and back"
  put_secret "$NS" "$OWN_SECRET" "$LOGS/tls/server"
  hc upgrade "$REL" "$CHART" -n "$NS" "${BASE_ARGS[@]}" "${NO_CM[@]}" \
    --wait --timeout 5m >"$LOGS/existing-up.log" 2>&1 || fail "upgrade to tls.source=existingSecret"
  kc -n "$NS" rollout status "ds/$REL-buildkit" --timeout=300s >/dev/null || fail "rollout to the provided Secret"
  own_served() {
    kc -n "$JOBS_NS" exec client-build -- buildctl --addr "$ADDR" --tlsdir /own --tlsservername "$SVC_HOST" \
      --timeout 5 debug workers -v 2>/dev/null | grep -q worker.hostname
  }
  check "a client of the provided CA is served" eventually 15 own_served
  # The cert-manager client certificate, trusting the new server: the daemon
  # itself must turn it away.
  refused "the cert-manager client certificate is refused after the switch" "$LOGS/existing-cm-client.log" \
    --tlscacert /own/ca.crt --tlscert /certs/tls.crt --tlskey /certs/tls.key --tlsservername "$SVC_HOST"
  hc upgrade "$REL" "$CHART" -n "$NS" "${INSTALL_ARGS[@]}" \
    --wait --timeout 5m >"$LOGS/existing-back.log" 2>&1 || fail "upgrade back to cert-manager"
  kc -n "$NS" rollout status "ds/$REL-buildkit" --timeout=300s >/dev/null || fail "rollout back to cert-manager"
  check "back on cert-manager, its client is served again" eventually 15 has_answer client-build
  settle
}

# pull_chart <name> <version> <sha256> [repo]: a pinned chart archive into $LOGS.
pull_chart() {
  local tgz=$LOGS/$1-$2.tgz
  [ -f "$tgz" ] || helm pull "$1" --repo "${4:-$VICTORIA_REPO}" --version "$2" -d "$LOGS" >/dev/null 2>&1 || return 1
  echo "$3  $tgz" | sha256 -c - >/dev/null 2>&1 || { echo "checksum mismatch for $tgz" >&2; return 1; }
  echo "$tgz"
}

s_netpol() {
  step "netpol: builds reach DNS and the internet, not the cluster; clients only from the jobs namespace"
  local target out k
  kc -n "$JOBS_NS" delete pod np-target --ignore-not-found --wait=true >/dev/null
  kc -n "$JOBS_NS" run np-target --image="$IMAGE" --restart=Never \
    --overrides="{\"spec\":{\"nodeName\":\"$BUILD_NODE\",\"terminationGracePeriodSeconds\":0}}" \
    --command -- sh -c 'while true; do echo ok | nc -l -p 8080; done' >/dev/null
  kc -n "$JOBS_NS" wait --for=condition=Ready pod/np-target --timeout=60s >/dev/null || { fail "target pod not ready"; return; }
  target=$(kc -n "$JOBS_NS" get pod np-target -o jsonpath='{.status.podIP}')
  # Positive control: the target answers a pod without a policy.
  check "control: the in-cluster target answers an unrestricted pod" \
    eventually 10 quiet kc -n "$JOBS_NS" exec client-build -- nc -z -w 3 "$target" 8080
  next_id
  build client-build netpol --opt "build-arg:BUST=$ID" --opt "build-arg:TARGET=$target" >"$LOGS/netpol.log" 2>&1
  check "the RUN step builds" test $? = 0
  check "a RUN step resolves cluster DNS" printed "$LOGS/netpol.log" DNS_OK
  check "a RUN step reaches the internet (registry-1.docker.io:443)" printed "$LOGS/netpol.log" WORLD_OK
  check "a RUN step cannot reach a pod in the cluster CIDRs" printed "$LOGS/netpol.log" CLUSTER_BLOCKED
  # Ingress: a pod with a valid client certificate in another namespace.
  ensure_ns "$OTHER_NS" baseline
  mkdir -p "$LOGS/outsider"
  for k in tls.crt tls.key ca.crt; do pem "$JOBS_NS" "$CLIENT_SECRET" "$k" >"$LOGS/outsider/$k"; done
  kc -n "$OTHER_NS" delete pod outsider --ignore-not-found --wait=true >/dev/null
  kc -n "$OTHER_NS" run outsider --image="$IMAGE" --restart=Never \
    --overrides="{\"spec\":{\"nodeName\":\"$BUILD_NODE\",\"terminationGracePeriodSeconds\":0}}" \
    --command -- sleep 3600 >/dev/null
  kc -n "$OTHER_NS" wait --for=condition=Ready pod/outsider --timeout=60s >/dev/null || { fail "outsider pod not ready"; return; }
  kc -n "$OTHER_NS" cp "$LOGS/outsider" outsider:/c >/dev/null || { fail "copy certificates to the outsider"; return; }
  out=$(kc -n "$OTHER_NS" exec outsider -- buildctl --addr "$ADDR" --tlsdir /c --tlsservername "$SVC_HOST" \
    --timeout 5 debug workers -v 2>&1)
  if printf '%s' "$out" | grep -q worker.hostname; then
    fail "a client in $OTHER_NS reached the daemons"
  elif printf '%s' "$out" | grep -qiE 'deadline exceeded|i/o timeout'; then
    pass "a client with a valid certificate in another namespace gets no connection"
  else
    fail "the outsider failed for another reason: $(printf '%s' "$out" | tail -1)"
  fi
  check "the same certificate works from the jobs namespace" has_answer client-build
  kc -n "$JOBS_NS" delete pod np-target --wait=false >/dev/null
}

s_otel() {
  step "otel: metrics and traces reach VictoriaMetrics and VictoriaTraces over OTLP"
  local vm vt vm_svc vt_svc tid sid series trace
  ensure_ns "$OBS_NS" baseline
  vm=$(pull_chart victoria-metrics-single "$VM_CHART_VERSION" "$VM_CHART_SHA256") || { fail "pull VictoriaMetrics chart"; return; }
  vt=$(pull_chart victoria-traces-single "$VT_CHART_VERSION" "$VT_CHART_SHA256") || { fail "pull VictoriaTraces chart"; return; }
  hc upgrade --install vm "$vm" -n "$OBS_NS" -f "$ROOT/test/e2e/victoria-metrics-values.yaml" \
    --wait --timeout 5m >"$LOGS/vm.log" 2>&1 || { fail "install VictoriaMetrics"; return; }
  hc upgrade --install vt "$vt" -n "$OBS_NS" -f "$ROOT/test/e2e/victoria-traces-values.yaml" \
    --wait --timeout 5m >"$LOGS/vt.log" 2>&1 || { fail "install VictoriaTraces"; return; }
  vm_svc=http://vm-victoria-metrics-single-server.$OBS_NS.svc:8428
  vt_svc=http://vt-vt-single-server.$OBS_NS.svc:10428
  # A second release with its own (emptyDir) state and exporters, without a
  # NetworkPolicy: the main release keeps its dead trace backend.
  hc upgrade --install "$OTEL_REL" "$CHART" -n "$NS" \
    --set "tls.client.namespaces={$JOBS_NS}" \
    --set "otel.metricsEndpoint=$vm_svc/opentelemetry/v1/metrics" \
    --set "otel.tracesEndpoint=$vt_svc/insert/opentelemetry/v1/traces" \
    --set otel.metricsIntervalSeconds=5 \
    --wait --timeout 5m >"$LOGS/otel-install.log" 2>&1 || { fail "install $OTEL_REL"; return; }
  eventually 30 quiet kc -n "$JOBS_NS" get secret "$OTEL_REL-buildkit-client" || { fail "no client certificate for $OTEL_REL"; return; }
  mkdir -p "$LOGS/otel-client"
  for k in tls.crt tls.key ca.crt; do pem "$JOBS_NS" "$OTEL_REL-buildkit-client" "$k" >"$LOGS/otel-client/$k"; done
  kc -n "$JOBS_NS" cp "$LOGS/otel-client" client-build:/otel >/dev/null || { fail "copy the $OTEL_REL client certificate"; return; }
  # A CI job's trace context, as the CI component will set it.
  tid=$(openssl rand -hex 16); sid=$(openssl rand -hex 8)
  next_id
  kc -n "$JOBS_NS" exec client-build -- env "TRACEPARENT=00-$tid-$sid-01" \
    "OTEL_RESOURCE_ATTRIBUTES=ci.pipeline.id=e2e-pipeline,ci.job.id=e2e-job" \
    buildctl --addr "tcp://$OTEL_REL-buildkit.$NS.svc:1234" --tlsdir /otel --tlsservername "$OTEL_REL-buildkit.$NS.svc" \
    build --frontend dockerfile.v0 --local context=/f/cache --local dockerfile=/f/cache \
    --opt "build-arg:BUST=$ID" --progress plain >"$LOGS/otel-build.log" 2>&1
  check "a build on the exporting daemons succeeds" test $? = 0
  series_ok() {
    series=$(kc -n "$JOBS_NS" exec client-build -- wget -qO- \
      "$vm_svc/api/v1/series?match%5B%5D=%7B__name__%3D%22buildkit.builds%22%7D" 2>/dev/null)
    printf '%s' "$series" | grep -q "\"k8s.pod.name\":\"$OTEL_REL-buildkit-"
  }
  check "buildkit.builds arrives, labelled with the daemon pod" eventually 30 series_ok
  check "the series carries the node and namespace" \
    eval 'printf "%s" "$series" | grep -q "\"k8s.node.name\":\"" && printf "%s" "$series" | grep -q "\"k8s.namespace.name\":\"$NS\""'
  trace_ok() {
    trace=$(kc -n "$JOBS_NS" exec client-build -- wget -qO- "$vt_svc/select/jaeger/api/traces/$tid" 2>/dev/null)
    printf '%s' "$trace" | grep -q '"serviceName":"buildkitd"'
  }
  check "the build's trace holds daemon spans" eventually 30 trace_ok
  printf '%s' "$trace" >"$LOGS/otel-trace.json"
  check "buildctl spans hang under the CI job's span, with its attributes" \
    python3 - "$LOGS/otel-trace.json" "$sid" "$OTEL_REL-buildkit-" <<'PY'
import json, sys
path, parent, pod_prefix = sys.argv[1:4]
t = json.load(open(path))["data"][0]
procs = t["processes"]
def tags(p): return {x["key"]: x["value"] for x in procs[p].get("tags", [])}
# Spans whose parent is the CI job's span: buildctl's, with the job's attributes.
roots = [s for s in t["spans"] if any(r["spanID"] == parent for r in s.get("references", []))]
ok = bool(roots) and all(procs[s["processID"]]["serviceName"] == "buildctl"
                         and tags(s["processID"]).get("ci.job.id") == "e2e-job" for s in roots)
# Daemon spans name their pod.
daemon = [p for p in procs if procs[p]["serviceName"] == "buildkitd"]
ok = ok and bool(daemon) and all(tags(p).get("k8s.pod.name", "").startswith(pod_prefix) for p in daemon)
sys.exit(0 if ok else 1)
PY
  hc uninstall "$OTEL_REL" -n "$NS" --wait --timeout 5m >/dev/null 2>&1 || true
  kc -n "$CM_NS" delete secret -l "app.kubernetes.io/instance=$OTEL_REL,ci-accelerator.io/component=buildkit" --ignore-not-found >/dev/null 2>&1
}

# fake_gitlab <method> <path> [json]: call the stand-in GitLab from a client.
fake_gitlab() {
  if [ "$1" = POST ]; then
    kc -n "$JOBS_NS" exec client-build -- wget -qO- --header 'Content-Type: application/json' \
      --post-data "$3" "http://fake-gitlab.$NS.svc$2"
  else
    kc -n "$JOBS_NS" exec client-build -- wget -qO- "http://fake-gitlab.$NS.svc$2"
  fi
}

# run_job <name> [jobs namespace]: queue the e2e CI job, wait for it, keep its
# pod spec and trace in $LOGS/job-<name>.{pod.json,trace,state}.
run_job() {
  local name=$1 jobs_ns=${2:-$JOBS_NS} id state i pod_seen=0
  id=$(fake_gitlab POST /e2e/jobs "$JOB_SPEC" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])') \
    || { fail "queue job $name"; return 1; }
  for i in $(seq 1 120); do
    if [ "$pod_seen" = 0 ]; then
      kc -n "$jobs_ns" get pods -l ci-accelerator.io/component=ci-job -o json >"$LOGS/job-$name.pod.json" 2>/dev/null
      grep -q '"nodeName"' "$LOGS/job-$name.pod.json" && pod_seen=1
    fi
    fake_gitlab GET "/e2e/jobs/$id" >"$LOGS/job-$name.json" 2>/dev/null
    state=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["state"])' "$LOGS/job-$name.json" 2>/dev/null)
    case "$state" in success|failed) break ;; esac
    sleep 2
  done
  # The trace as plain lines: without colour codes, carriage returns and
  # the runner's "<timestamp> <stream><O|E>[+] " prefix.
  python3 -c 'import json,re,sys
t = re.sub(r"\x1b\[[0-9;]*[A-Za-z]|\r", "", json.load(open(sys.argv[1]))["trace"])
print("\n".join(re.sub(r"^\d{4}-\d\d-\d\dT[0-9:.]+Z [0-9]+[OE]\+? ?", "", l) for l in t.split("\n")))' \
    "$LOGS/job-$name.json" >"$LOGS/job-$name.trace" 2>/dev/null
  echo "${state:-unknown}" >"$LOGS/job-$name.state"
}

# job_printed <name> <regex>: a whole output line of the job. The runner also
# echoes every script line, so a plain grep would match the command itself.
job_printed() { grep -qE "^$2\$" "$LOGS/job-$1.trace"; }

# ensure_fake_gitlab <runner namespace>: the stand-in GitLab API in $NS and a
# runner token Secret in the runner's namespace.
ensure_fake_gitlab() {
  kc -n "$NS" create configmap fake-gitlab --from-file="$ROOT/test/e2e/fake-gitlab/server.py" \
    --dry-run=client -o yaml | kc apply -f - >/dev/null
  kc -n "$NS" apply -f - >/dev/null <<EOF_FAKE
apiVersion: v1
kind: Pod
metadata: {name: fake-gitlab, labels: {app: fake-gitlab}}
spec:
  tolerations: [{key: node-role.kubernetes.io/control-plane, operator: Exists, effect: NoSchedule}]
  containers:
    - name: api
      image: $PYTHON_IMAGE
      command: [python3, -u, /app/server.py]
      ports: [{containerPort: 8080}]
      volumeMounts: [{name: app, mountPath: /app}]
      readinessProbe: {httpGet: {path: /healthz, port: 8080}, periodSeconds: 2}
  volumes: [{name: app, configMap: {name: fake-gitlab}}]
---
apiVersion: v1
kind: Service
metadata: {name: fake-gitlab}
spec:
  selector: {app: fake-gitlab}
  ports: [{port: 80, targetPort: 8080}]
EOF_FAKE
  kc -n "$NS" wait --for=condition=Ready pod/fake-gitlab --timeout=120s >/dev/null || { fail "fake GitLab not ready"; return 1; }
  kc -n "$1" create secret generic gitlab-runner-token --from-literal=runner-token=glrt-e2e-not-a-real-token \
    --from-literal=runner-registration-token= --dry-run=client -o yaml | kc apply -f - >/dev/null
}

# The e2e CI job: what the CI component will do, plus evidence for the checks.
job_spec() {
  python3 - "$IMAGE" <<'PY'
import json, sys
print(json.dumps({"image": sys.argv[1], "script": [
    'env | grep "^BUILDKIT_" | sort',
    'ls "$BUILDKIT_TLS_DIR"',
    'if [ -e /var/run/secrets/kubernetes.io/serviceaccount/token ]; then echo SA_TOKEN_PRESENT; else echo NO_SA_TOKEN; fi',
    'mkdir -p ctx && echo "FROM alpine:3.22" > ctx/Dockerfile && echo "RUN sleep 2 && echo deps > /deps" >> ctx/Dockerfile',
    'echo "ARG BUST" >> ctx/Dockerfile && echo "RUN echo bust=\\$BUST" >> ctx/Dockerfile',
    'buildctl --addr "$BUILDKIT_REMOTE_ADDR" --tlsdir "$BUILDKIT_TLS_DIR" debug workers -v | grep worker.hostname',
    'buildctl --addr "$BUILDKIT_REMOTE_ADDR" --tlsdir "$BUILDKIT_TLS_DIR" build --frontend dockerfile.v0 --local context=ctx --local dockerfile=ctx --opt build-arg:BUST=$CI_JOB_ID --progress plain',
]}))
PY
}

s_runner() {
  step "runner: a real gitlab-runner with integrations/gitlab-runner sends CI jobs to the daemons"
  local chart frag pod node daemon_node arch
  ensure_fake_gitlab "$NS" || return
  # The fragment as a user would adapt it: the names marked ADJUST.
  frag=$LOGS/runner-fragment.yaml
  # e2e only: every job on one build node, so the second job meets the
  # daemon whose cache the first one filled (the cache is per daemon).
  arch=$(kc get node "$BUILD_NODE" -o jsonpath='{.metadata.labels.kubernetes\.io/arch}')
  sed -e "s|ci-accelerator-buildkit.ci-accelerator.svc|$SVC_HOST|" \
      -e "s|\"kubernetes.io/arch\" = \"amd64\"|\"kubernetes.io/arch\" = \"$arch\"|" \
      -e "s|ci-accelerator-buildkit-client|$CLIENT_SECRET|" \
      -e "s|^\( *\)\"ci-accelerator.io/node-role\" = \"build\"|&\\
\1\"kubernetes.io/hostname\" = \"$BUILD_NODE\"|" \
      "${E2E_RUNNER_FRAGMENT:-$ROOT/integrations/gitlab-runner/values.yaml}" >"$frag"
  check "the fragment's ADJUST names were all replaced" \
    eval '! grep -q "ci-accelerator-buildkit" "$frag"'
  chart=$(pull_chart gitlab-runner "$RUNNER_CHART_VERSION" "$RUNNER_CHART_SHA256" "$RUNNER_REPO") \
    || { fail "pull the gitlab-runner chart"; return; }
  hc upgrade --install gitlab-runner "$chart" -n "$NS" -f "$frag" -f "$ROOT/test/e2e/runner-values.yaml" \
    --set "gitlabUrl=http://fake-gitlab.$NS.svc/" --set runners.secret=gitlab-runner-token \
    --set "runners.jobNamespace=$JOBS_NS" --wait --timeout 5m >"$LOGS/runner-install.log" 2>&1 \
    || { fail "install gitlab-runner (see $LOGS/runner-install.log)"; return; }
  pass "the official chart with the fragment registers and gets ready"
  check "gitlab-runner lint accepts the generated config.toml" \
    quiet kc -n "$NS" exec deploy/gitlab-runner -- gitlab-runner lint -c /home/gitlab-runner/.gitlab-runner/config.toml
  JOB_SPEC=$(job_spec)
  run_job first
  check "the first job succeeds" test "$(cat "$LOGS/job-first.state")" = success
  pod=$LOGS/job-first.pod.json
  check "the job pod runs in $JOBS_NS (PSA baseline)" grep -q "\"namespace\": \"$JOBS_NS\"" "$pod"
  node=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["items"][0]["spec"]["nodeName"])' "$pod" 2>/dev/null)
  check "the job pod landed on a build node (${node:-none})" \
    eval 'case " $BUILD_NODES " in *" $node "*) true ;; *) false ;; esac'
  check "the job gets BUILDKIT_REMOTE_ADDR for the daemons" job_printed first "BUILDKIT_REMOTE_ADDR=tcp://$SVC_HOST:1234"
  check "the job gets BUILDKIT_TLS_DIR" job_printed first "BUILDKIT_TLS_DIR=/certs"
  check "the client certificate is mounted at BUILDKIT_TLS_DIR" \
    eval 'job_printed first ca.crt && job_printed first tls.crt && job_printed first tls.key'
  check "no Kubernetes API token in the job pod" job_printed first NO_SA_TOKEN
  check "the build container asks for the small tier (50m CPU)" \
    eval 'python3 -c "import json,sys; c=[x for x in json.load(open(sys.argv[1]))[\"items\"][0][\"spec\"][\"containers\"] if x[\"name\"]==\"build\"][0]; sys.exit(0 if c[\"resources\"][\"requests\"][\"cpu\"]==\"50m\" else 1)" "$pod"'
  daemon_node=$(kc -n "$NS" get pod "$(grep -o 'worker.hostname: *[^ ]*' "$LOGS/job-first.trace" | head -1 | awk '{print $2}')" \
    -o jsonpath='{.spec.nodeName}' 2>/dev/null)
  check "the daemon that answered runs on the job's node (${daemon_node:-?})" test -n "$node" -a "$daemon_node" = "$node"
  check "the first job's dependency step ran (cold baseline)" \
    eval '! grep -A1 "^#[0-9]* \[.*RUN sleep 2" "$LOGS/job-first.trace" | grep -q "CACHED"'
  run_job second
  check "the second job succeeds" test "$(cat "$LOGS/job-second.state")" = success
  check "the second job's dependency step is CACHED" \
    eval 'grep -A1 "^#[0-9]* \[.*RUN sleep 2" "$LOGS/job-second.trace" | grep -q "^#[0-9]* CACHED$"'
  # The stand-in GitLab stays for the box scenario; cleanup removes it.
  hc uninstall gitlab-runner -n "$NS" --wait --timeout 3m >/dev/null 2>&1 || true
}

s_box() {
  step "box: charts/ci-accelerator installs daemons, runner, jobs namespace and storage that work together"
  local node arch
  ensure_ns "$BOX_NS" privileged
  ensure_fake_gitlab "$BOX_NS" || return
  make -C "$ROOT" deps >"$LOGS/box-deps.log" 2>&1 || { fail "make deps (see $LOGS/box-deps.log)"; return; }
  arch=$(kc get node "$BUILD_NODE" -o jsonpath='{.metadata.labels.kubernetes\.io/arch}')
  local extra=()
  for f in ${E2E_BOX_EXTRA_VALUES:-}; do extra+=(-f "$f"); done
  hc upgrade --install "$BOX_REL" "$ROOT/charts/ci-accelerator" -n "$BOX_NS" -f "$ROOT/test/e2e/box-values.yaml" ${extra[@]+"${extra[@]}"} \
    --set "gitlab-runner.gitlabUrl=http://fake-gitlab.$NS.svc/" --set gitlab-runner.runners.secret=gitlab-runner-token \
    --set "global.jobs.namespace=$BOX_JOBS_NS" --set "gitlab-runner.runners.jobNamespace=$BOX_JOBS_NS" \
    --set "global.jobs.architecture=$arch" --wait --timeout 8m >"$LOGS/box-install.log" 2>&1 \
    || { fail "install the box (see $LOGS/box-install.log)"; return; }
  pass "the box installs with two required values and gets ready"
  # A token Secret without the empty registration key (seen on a first
  # install) is refused before anything is created.
  kc -n "$BOX_NS" delete secret bad-runner-token --ignore-not-found >/dev/null
  kc -n "$BOX_NS" create secret generic bad-runner-token --from-literal=runner-token=glrt-e2e-not-a-real-token >/dev/null
  if hc upgrade "$BOX_REL" "$ROOT/charts/ci-accelerator" -n "$BOX_NS" -f "$ROOT/test/e2e/box-values.yaml" --dry-run=server \
      --set "gitlab-runner.gitlabUrl=http://fake-gitlab.$NS.svc/" --set gitlab-runner.runners.secret=bad-runner-token \
      --set "global.jobs.namespace=$BOX_JOBS_NS" --set "gitlab-runner.runners.jobNamespace=$BOX_JOBS_NS" \
      --set "global.jobs.architecture=$arch" >"$LOGS/box-bad-secret.log" 2>&1; then
    fail "a runner token Secret without runner-registration-token was accepted"
  elif grep -q "has no runner-registration-token key" "$LOGS/box-bad-secret.log"; then
    pass "a runner token Secret without runner-registration-token is refused with the fix"
  else
    fail "the bad runner Secret failed for another reason (see $LOGS/box-bad-secret.log)"
  fi
  check "the jobs namespace enforces Pod Security baseline" \
    test "$(kc get namespace "$BOX_JOBS_NS" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}')" = baseline
  check "the client certificate is issued into the jobs namespace" \
    eventually 30 quiet kc -n "$BOX_JOBS_NS" get secret "$BOX_REL-buildkit-client"
  JOB_SPEC=$(job_spec)
  run_job box "$BOX_JOBS_NS"
  check "a CI job through the box's runner succeeds" test "$(cat "$LOGS/job-box.state")" = success
  check "the job talks to the box's daemons" job_printed box "BUILDKIT_REMOTE_ADDR=tcp://$BOX_REL-buildkit.$BOX_NS.svc:1234"
  check "no Kubernetes API token in the box's job pod" job_printed box NO_SA_TOKEN
  node=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["items"][0]["spec"]["nodeName"])' "$LOGS/job-box.pod.json" 2>/dev/null)
  check "the box's job pod landed on a build node (${node:-none})" \
    eval 'case " $BUILD_NODES " in *" $node "*) true ;; *) false ;; esac'
  # The daemons push to the box's own storage.
  box_metrics() {
    kc -n "$JOBS_NS" exec client-build -- wget -qO- \
      "http://$BOX_REL-victoria-metrics-single-server.$BOX_NS.svc:8428/api/v1/series?match%5B%5D=%7B__name__%3D%22buildkit.builds%22%7D" 2>/dev/null \
      | grep -q "\"k8s.namespace.name\":\"$BOX_NS\""
  }
  check "the box's VictoriaMetrics receives the daemons' build metrics" eventually 30 box_metrics
  box_traces() {
    kc -n "$JOBS_NS" exec client-build -- wget -qO- \
      "http://$BOX_REL-vt-single-server.$BOX_NS.svc:10428/select/jaeger/api/services" 2>/dev/null | grep -q '"buildkitd"'
  }
  check "the box's VictoriaTraces receives the daemons' spans" eventually 30 box_traces
  hc uninstall "$BOX_REL" -n "$BOX_NS" --wait --timeout 5m >/dev/null 2>&1 || fail "uninstall the box"
  check "uninstall removes the jobs namespace" \
    eventually 60 eval '! kc get namespace "$BOX_JOBS_NS" >/dev/null 2>&1'
  kc -n "$CM_NS" delete secret -l "app.kubernetes.io/instance=$BOX_REL,ci-accelerator.io/component=buildkit" --ignore-not-found >/dev/null 2>&1
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
  local restarts unhealthy alive probe_pod out rc probe_s plain_s
  check "the hostile OTEL env is deployed" \
    test -n "$(kc -n "$NS" get ds "$REL-buildkit" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="OTEL_TRACES_EXPORTER")].value}')"
  restarts=$(kc -n "$NS" get pods -l "app.kubernetes.io/instance=$REL" \
    -o jsonpath='{range .items[*]}{.metadata.name} {.status.containerStatuses[0].restartCount}{"\n"}{end}' \
    | awk -v k="$SIGKILLED_POD" '$1 != k {s += $2} END {print s + 0}')
  check "0 unexpected daemon restarts" test "$restarts" = 0
  # Probe failures of the daemons running now. Pods replaced on purpose
  # (rollouts, deletions) legitimately fail readiness while they drain: the
  # socket is gone after SIGTERM. The SIGKILLed daemon fails it while it
  # restarts.
  alive=" $(kc -n "$NS" get pods -l "app.kubernetes.io/instance=$REL" -o jsonpath='{.items[*].metadata.name}') "
  unhealthy=$(kc -n "$NS" get events --field-selector reason=Unhealthy \
    -o jsonpath='{range .items[*]}{.involvedObject.name}{"\t"}{.message}{"\n"}{end}' \
    | awk -F'\t' -v alive="$alive" -v k="$SIGKILLED_POD" \
        'index(alive, " " $1 " ") && $1 != k && $2 ~ /^(Readiness|Liveness|Startup) probe/' | wc -l | tr -d ' ')
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
