#!/usr/bin/env bash
# Generates BuildKit build-history fixtures for the console's history model
# (internal/history/testdata): real records of CI-like builds, as the
# collector reads them from a daemon.
#
#   make kind-up && test/history/generate.sh
#
# Installs charts/buildkit as release "hfix" (one daemon, certificates from
# scripts/buildkit-certs.sh), runs the builds from one client pod with the
# runner hook's trace context, saves for every record its JSON, the raw
# progress blob, the provenance and the trace, and uninstalls. Refuses
# anything but a local kind cluster. Needs bash, kubectl, helm, kind, python3.

set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
CTX=${KUBE_CONTEXT:-kind-buildkit}
REL=hfix NS=hfix JOBS_NS=hfix-jobs
OUT=${OUT:-$ROOT/internal/history/testdata/records}
GITLAB=https://gitlab.example.com/
IMAGE=moby/buildkit:$(awk -F'"' '/^appVersion:/ {print $2}' "$ROOT/charts/buildkit/Chart.yaml")

case "$CTX" in kind-*) ;; *) echo "refusing: '$CTX' is not a kind context" >&2; exit 2 ;; esac
kc() { kubectl --context "$CTX" "$@"; }
sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum; else shasum -a 256; fi; }
hc() { helm --kube-context "$CTX" "$@"; }

TMP=$(mktemp -d "${TMPDIR:-/tmp}/history-fixtures.XXXXXX")
cleanup() {
  hc uninstall "$REL" -n "$NS" --wait --timeout 5m >/dev/null 2>&1 || true
  kc delete namespace "$NS" "$JOBS_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  rm -rf "$TMP"
}
trap cleanup EXIT

NODE=$(kc get nodes -l ci-accelerator.io/node-role=build -o jsonpath='{.items[0].metadata.name}')
for ns in "$NS" "$JOBS_NS"; do kc create namespace "$ns" >/dev/null 2>&1 || true; done
kc label namespace "$NS" pod-security.kubernetes.io/enforce=privileged --overwrite >/dev/null
"$ROOT/scripts/buildkit-certs.sh" --name "$REL-buildkit" --namespace "$NS" --out "$TMP/certs" >/dev/null
kc -n "$NS" create secret generic "$REL-tls" --from-file="$TMP/certs/server/tls.crt" \
  --from-file="$TMP/certs/server/tls.key" --from-file="$TMP/certs/server/ca.crt" >/dev/null
kc -n "$JOBS_NS" create secret generic "$REL-client" --from-file="$TMP/certs/client/tls.crt" \
  --from-file="$TMP/certs/client/tls.key" --from-file="$TMP/certs/client/ca.crt" >/dev/null
# One daemon, so every build shares one history and one cache.
hc upgrade --install "$REL" "$ROOT/charts/buildkit" -n "$NS" \
  --set tls.source=existingSecret --set "tls.existingSecret=$REL-tls" \
  --set-json "global.buildNodes.selector={\"ci-accelerator.io/node-role\":\"build\",\"kubernetes.io/hostname\":\"$NODE\"}" \
  --wait --timeout 5m >/dev/null
ADDR=tcp://$REL-buildkit.$NS.svc:1234
TLS=(--tlsdir /certs --tlsservername "$REL-buildkit.$NS.svc")
kc -n "$JOBS_NS" run client --image="$IMAGE" --restart=Never --overrides="{\"spec\":{\"nodeName\":\"$NODE\",
  \"containers\":[{\"name\":\"client\",\"image\":\"$IMAGE\",\"command\":[\"sleep\",\"3600\"],
  \"volumeMounts\":[{\"name\":\"certs\",\"mountPath\":\"/certs\",\"readOnly\":true}]}],
  \"volumes\":[{\"name\":\"certs\",\"secret\":{\"secretName\":\"$REL-client\"}}]}}" >/dev/null
kc -n "$JOBS_NS" wait --for=condition=Ready pod/client --timeout=120s >/dev/null
cx() { kc -n "$JOBS_NS" exec client -- "$@"; }

# The project: three images from one Dockerfile, sharing a "deps" stage.
app() { # app <deps> <api source> [extra Dockerfile line]
  cx sh -c "rm -rf /app && mkdir -p /app/api /app/worker && cd /app &&
    printf '%s\n' '$1' > deps.txt && printf '%s\n' '$2' > api/main.txt && printf 'worker v1\n' > worker/main.txt &&
    printf '.git\n' > .dockerignore &&
    cat > Dockerfile <<'EOF'
ARG ALPINE=3.22
FROM alpine:\${ALPINE} AS deps
WORKDIR /app
COPY deps.txt .
RUN --mount=type=cache,target=/cache sleep 3 && cp deps.txt /cache/ && echo deps installed

FROM deps AS api
COPY api/ ./api
ARG VERSION=dev
RUN echo \"api \$VERSION\" > /version${3:-}

FROM deps AS worker
COPY worker/ ./worker
RUN echo worker > /version

FROM deps AS test
COPY . .
RUN cat api/main.txt worker/main.txt > /tested

FROM deps AS broken
RUN echo failing step && exit 3
EOF"
}
# build <pipeline> <job> <target> [buildctl args...]: a CI job's build, with
# the trace context the runner hook exports (one trace per job).
build() {
  local p=$1 j=$2 t=$3 tp; shift 3
  tp=$(printf '00-01%s%012x%012x-%016x-01' "$(printf '%s' "$GITLAB" | sha256 | cut -c1-6)" "$p" "$j" "$j")
  cx env TRACEPARENT="$tp" OTEL_RESOURCE_ATTRIBUTES="ci.pipeline.id=$p,ci.job.id=$j,ci.project.path=example/app" \
    buildctl --addr "$ADDR" "${TLS[@]}" build --frontend dockerfile.v0 --local context=/app --local dockerfile=/app \
    --opt target="$t" --progress plain "$@" >/dev/null 2>&1 || true
}

app 'lib 1' 'api v1'
build 100 1001 api                                  # cold
build 101 1002 api --opt build-arg:VERSION=2        # a build argument changed
app 'lib 1' 'api v2'; build 102 1003 api            # files it reads changed
app 'lib 2' 'api v2'; build 103 1004 api            # dependencies changed
app 'lib 3' 'api v2'                                # three jobs at once share "deps"
build 104 1005 api & sleep 0.3; build 104 1006 worker & build 104 1007 test; wait
app 'lib 3' 'api v2' ' && echo changed'; build 105 1008 api   # a step changed
build 106 1009 api --opt build-arg:ALPINE=3.21      # the base image changed
build 107 1010 broken                               # a failing build
cx buildctl --addr "$ADDR" "${TLS[@]}" build --frontend dockerfile.v0 --local context=/app \
  --local dockerfile=/app --opt target=worker >/dev/null 2>&1 || true   # no trace context
build 108 1011 api; build 108 1011 test             # two builds in one job

# The trace is attached up to 3 s after a build: wait for every record.
sleep 10
rm -rf "$OUT"; mkdir -p "$OUT"
cx buildctl --addr "$ADDR" "${TLS[@]}" debug histories --format '{{json .Record}}' >"$TMP/records.jsonl"
python3 - "$TMP/records.jsonl" >"$TMP/blobs.txt" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    if not line.strip():
        continue
    r = json.loads(line)
    att = ((r.get("Result") or {}).get("Attestations") or [{}])[0]
    print(r["Ref"], (r.get("logs") or {}).get("digest", "-"), att.get("digest", "-"), (r.get("trace") or {}).get("digest", "-"))
PY
while read -r ref logs prov trace; do
  mkdir -p "$OUT/$ref"
  grep "\"Ref\":\"$ref\"" "$TMP/records.jsonl" >"$OUT/$ref/record.json"
  [ "$logs" = - ] || cx buildctl --addr "$ADDR" "${TLS[@]}" debug get "$logs" >"$OUT/$ref/logs.bin"
  [ "$prov" = - ] || cx buildctl --addr "$ADDR" "${TLS[@]}" debug get "$prov" >"$OUT/$ref/provenance.json"
  [ "$trace" = - ] || cx buildctl --addr "$ADDR" "${TLS[@]}" debug get "$trace" >"$OUT/$ref/trace.json"
done <"$TMP/blobs.txt"
echo "$(wc -l <"$TMP/blobs.txt" | tr -d ' ') records in $OUT"
