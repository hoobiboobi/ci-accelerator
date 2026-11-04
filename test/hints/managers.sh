#!/usr/bin/env bash
# Checks the console's cache-mount hints against real builds, one small
# project per package manager (test/hints/cases/<name>):
#   1. the manager's own command reports where its cache is (stage probe);
#   2. the install step is built without a cache mount (stage install,
#      the Dockerfile's last line), and its record's provenance is saved
#      as a fixture for internal/hints;
#   3. the hints read that provenance (test/hints/fragment): the cache
#      folders they place must be the ones the manager reported;
#   4. the suggested fragment replaces the last line, and the step is run
#      again with and without the mount: the times and the files in the
#      mount are reported.
#
#   make kind-up && test/hints/managers.sh [case...]
#
# Installs charts/buildkit as release "hint" (one daemon), prunes the
# daemon's cache after every case, uninstalls at the end. Refuses anything
# but a local kind cluster.

set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
CTX=${KUBE_CONTEXT:-kind-buildkit}
REL=hint NS=hint JOBS_NS=hint-jobs
OUT=${OUT:-$ROOT/internal/hints/testdata/records}
RESULTS=${RESULTS:-$OUT/../results.txt}
IMAGE=moby/buildkit:$(awk -F'"' '/^appVersion:/ {print $2}' "$ROOT/charts/buildkit/Chart.yaml")

case "$CTX" in kind-*) ;; *) echo "refusing: '$CTX' is not a kind context" >&2; exit 2 ;; esac
kc() { kubectl --context "$CTX" "$@"; }
hc() { helm --kube-context "$CTX" "$@"; }

TMP=$(mktemp -d "${TMPDIR:-/tmp}/hints.XXXXXX")
cleanup() {
  [ -n "${KEEP:-}" ] && return
  hc uninstall "$REL" -n "$NS" --wait --timeout 5m >/dev/null 2>&1 || true
  kc delete namespace "$NS" "$JOBS_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  rm -rf "$TMP"
}
trap cleanup EXIT

NODE=$(kc get nodes -l ci-accelerator.io/node-role=build -o jsonpath='{.items[0].metadata.name}')
if ! kc -n "$JOBS_NS" get pod client >/dev/null 2>&1; then
  for ns in "$NS" "$JOBS_NS"; do kc create namespace "$ns" >/dev/null 2>&1 || true; done
  kc label namespace "$NS" pod-security.kubernetes.io/enforce=privileged --overwrite >/dev/null
  "$ROOT/scripts/buildkit-certs.sh" --name "$REL-buildkit" --namespace "$NS" --out "$TMP/certs" >/dev/null
  kc -n "$NS" create secret generic "$REL-tls" --from-file="$TMP/certs/server/tls.crt" \
    --from-file="$TMP/certs/server/tls.key" --from-file="$TMP/certs/server/ca.crt" >/dev/null
  kc -n "$JOBS_NS" create secret generic "$REL-client" --from-file="$TMP/certs/client/tls.crt" \
    --from-file="$TMP/certs/client/tls.key" --from-file="$TMP/certs/client/ca.crt" >/dev/null
  hc upgrade --install "$REL" "$ROOT/charts/buildkit" -n "$NS" \
    --set tls.source=existingSecret --set "tls.existingSecret=$REL-tls" \
    --set-json "global.buildNodes.selector={\"ci-accelerator.io/node-role\":\"build\",\"kubernetes.io/hostname\":\"$NODE\"}" \
    --wait --timeout 5m >/dev/null
  kc -n "$JOBS_NS" run client --image="$IMAGE" --restart=Never --overrides="{\"spec\":{\"nodeName\":\"$NODE\",
    \"containers\":[{\"name\":\"client\",\"image\":\"$IMAGE\",\"command\":[\"sleep\",\"86400\"],
    \"volumeMounts\":[{\"name\":\"certs\",\"mountPath\":\"/certs\",\"readOnly\":true}]}],
    \"volumes\":[{\"name\":\"certs\",\"secret\":{\"secretName\":\"$REL-client\"}}]}}" >/dev/null
  kc -n "$JOBS_NS" wait --for=condition=Ready pod/client --timeout=120s >/dev/null
fi
ADDR=tcp://$REL-buildkit.$NS.svc:1234
TLS=(--tlsdir /certs --tlsservername "$REL-buildkit.$NS.svc")
cx() { kc -n "$JOBS_NS" exec client -- "$@"; }
bk() { cx buildctl --addr "$ADDR" "${TLS[@]}" "$@"; }
# build <case dir in the pod> <target> <bust> <log>: plain progress into log.
build() {
  bk build --frontend dockerfile.v0 --local context="$1" --local dockerfile="$1" \
    --opt target="$2" --opt build-arg:BUST="$3" --progress plain >"$4" 2>&1
}
# stepTime <log>: seconds the install stage's RUN step took.
stepTime() {
  local n
  n=$(sed -n 's/^#\([0-9]*\) \[install [0-9]*\/[0-9]*\] RUN.*/\1/p' "$1" | tail -1)
  [ -n "$n" ] && sed -n "s/^#$n DONE \([0-9.]*\)s\$/\1/p" "$1" | tail -1
}
freeGB() { docker exec "$NODE" df -BG --output=avail /var | tail -1 | tr -dc 0-9; }

CASES=("$@")
if [ ${#CASES[@]} -eq 0 ]; then
  while IFS= read -r c; do CASES+=("$c"); done < <(cd "$ROOT/test/hints/cases" && ls)
fi
mkdir -p "$OUT"
for c in "${CASES[@]}"; do
  echo "== $c (node has $(freeGB) GB free)"
  dir=/cases/$c
  cx rm -rf "$dir"; cx mkdir -p /cases
  kc cp "$ROOT/test/hints/cases/$c" "$JOBS_NS/client:$dir" >/dev/null
  if ! bk build --frontend dockerfile.v0 --local context="$dir" --local dockerfile="$dir" --opt target=probe --opt no-cache= --progress plain >"$TMP/probe.log" 2>&1; then
    echo "   probe failed:"; tail -5 "$TMP/probe.log"; continue
  fi
  reported=$(sed -n 's/^#[0-9]* [0-9.]* CACHEDIR=\(.*\)$/\1/p' "$TMP/probe.log" | sed 's:/*$::' | tr '\n' ' ')
  grep -E '^#[0-9]+ [0-9.]+ (HOME|USER|MAMBA|DENO|Package cache|  *package cache)' "$TMP/probe.log" | sed 's/^/   probe: /' || true
  if ! build "$dir" install 1 "$TMP/a1.log"; then
    echo "   install failed:"; tail -8 "$TMP/a1.log"; continue
  fi
  ref=$(bk debug histories --format '{{json .Record}}' | python3 -c '
import json, sys
rs = [json.loads(l) for l in sys.stdin if l.strip()]
def at(r):
    t = r.get("CreatedAt") or {}
    return (t.get("seconds", 0), t.get("nanos", 0)) if isinstance(t, dict) else (str(t), 0)
rs.sort(key=at)
print(rs[-1]["Ref"], ((rs[-1].get("Result") or {}).get("Attestations") or [{}])[0].get("digest", "-"))')
  set -- $ref
  mkdir -p "$OUT/$c"
  bk debug get "$2" >"$OUT/$c/provenance.json"
  printf '%s\n' $reported >"$OUT/$c/reported.txt"
  (cd "$ROOT" && go run ./test/hints/fragment "$OUT/$c/provenance.json") >"$TMP/frag.out"
  computed=$(sed -n 's/^PATH [^ ]* //p' "$TMP/frag.out" | tr '\n' ' ')
  sed -n 's/^PROBLEM /   problem: /p' "$TMP/frag.out"
  fragment=$(sed -n '/^FRAGMENT$/,$p' "$TMP/frag.out" | tail -n +2)
  match=no
  [ "$(printf '%s\n' $reported | sort)" = "$(printf '%s\n' $computed | sort)" ] && match=yes
  echo "   reported: $reported"
  echo "   computed: $computed (match: $match)"
  printf '   fragment:\n%s\n' "$fragment" | sed 's/^/     /'
  nomount=- warm=- files=-
  if [ -n "$fragment" ] && ! printf '%s' "$fragment" | grep -q '<'; then
    sed '$d' "$ROOT/test/hints/cases/$c/Dockerfile" >"$TMP/Dockerfile"
    printf '%s\n' "$fragment" >>"$TMP/Dockerfile"
    cx rm -rf "$dir-fixed"
    kc cp "$ROOT/test/hints/cases/$c" "$JOBS_NS/client:$dir-fixed" >/dev/null
    kc cp "$TMP/Dockerfile" "$JOBS_NS/client:$dir-fixed/Dockerfile" >/dev/null
    build "$dir" install 2 "$TMP/a2.log" && nomount=$(stepTime "$TMP/a2.log")
    if build "$dir-fixed" install 3 "$TMP/b1.log" && build "$dir-fixed" install 4 "$TMP/b2.log"; then
      warm=$(stepTime "$TMP/b2.log")
      # The fragment's own mount options (uid, mode and sharing make a
      # mount of their own), over every folder it mounts; counted with a
      # changed argument, not with no-cache: a build with no-cache gets
      # its cache mounts empty.
      flags=$(printf '%s\n' "$fragment" | grep -o -- '--mount=type=cache,[^ ]*' | tr '\n' ' ')
      targets=$(printf '%s\n' $flags | sed 's/.*target=\([^,]*\).*/\1/' | tr '\n' ' ')
      printf 'FROM busybox:1.37\nARG AT\nRUN %s find %s -type f | wc -l | sed "s/^/FILES=/"\n' "$flags" "$targets" >"$TMP/Dockerfile.count"
      cx mkdir -p /count; kc cp "$TMP/Dockerfile.count" "$JOBS_NS/client:/count/Dockerfile" >/dev/null
      bk build --frontend dockerfile.v0 --local context=/count --local dockerfile=/count --opt build-arg:AT="$c-$(date +%s)" --progress plain >"$TMP/count.log" 2>&1 || true
      files=$(sed -n 's/^#[0-9]* [0-9.]* FILES=\(.*\)$/\1/p' "$TMP/count.log" | tail -1)
    else
      echo "   fixed build failed:"; tail -8 "$TMP/b1.log" "$TMP/b2.log" 2>/dev/null | sed 's/^/     /'
      warm=failed
    fi
  fi
  echo "   rerun without mount: ${nomount}s, with the fragment (warm): ${warm}s, files in mount: $files"
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$c" "$match" "$reported" "$computed" "$nomount" "$warm" "$files" >>"$RESULTS"
  bk prune --all >/dev/null 2>&1 || true
done
