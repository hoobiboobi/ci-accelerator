#!/usr/bin/env bash
# Checks scripts/quickstart.sh on a clean Linux machine (the GitHub runner,
# or a throwaway VM): installs this tree with it against the fake GitLab of
# the e2e, runs two CI jobs that build an image through the real runner
# (the second must find the first one's cache), asks the console about the
# build, and uninstalls. Run as root, from the repository root, after
# `make deps` and with the console image saved to a tar:
#
#   sudo test/quickstart/run.sh /tmp/console.tar
#
# or a published release as a user gets it: quickstart.sh from the GitHub
# release, checked against its SHA256SUMS, the charts and the console image
# from the registry:
#
#   sudo test/quickstart/run.sh --release 0.2.0-rc.1
#
# Destroys whatever k3s it installed. Never run it on a machine you need.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
IMAGE_TAR="" RELEASE=""
case "${1:?usage: $0 <console image tar> | --release <version>}" in
  --release) RELEASE=${2:?usage: $0 --release <version>} ;;
  *) IMAGE_TAR=$1 ;;
esac
SCRIPT=$ROOT/scripts/quickstart.sh
LOGS=${QUICKSTART_LOGS:-$(mktemp -d)}
mkdir -p "$LOGS"
NS=ci-accelerator
BUILD_IMAGE=moby/buildkit:v0.33.1
PASSED=0 FAILED=0

pass() { PASSED=$((PASSED + 1)); echo "  PASS  $*"; }
fail() { FAILED=$((FAILED + 1)); echo "  FAIL  $*"; }
check() { local name=$1; shift; if "$@"; then pass "$name"; else fail "$name"; fi; }
kc() { /usr/local/bin/k3s kubectl "$@"; }

host_ip=$(hostname -I | awk '{print $1}')
gitlab=http://$host_ip:8080/
python3 "$ROOT/test/e2e/fake-gitlab/server.py" >"$LOGS/fake-gitlab.log" 2>&1 &
fake=$!
trap 'kill "$fake" 2>/dev/null || true' EXIT
for _ in $(seq 1 20); do curl -fs "${gitlab}healthz" >/dev/null && break; sleep 0.5; done

overrides=()
if [ -n "$RELEASE" ]; then
  # As the release notes say: download, check, run.
  dl=https://github.com/hoobiboobi/ci-accelerator/releases/download/v$RELEASE
  SCRIPT=$LOGS/quickstart.sh
  curl -fsSL -o "$SCRIPT" "$dl/quickstart.sh"
  curl -fsSL "$dl/SHA256SUMS" | grep ' quickstart.sh$' | (cd "$LOGS" && sha256sum -c --quiet -) \
    || { fail "quickstart.sh of v$RELEASE matches the release's SHA256SUMS"; exit 1; }
  pass "quickstart.sh of v$RELEASE matches the release's SHA256SUMS"
else
  overrides=(CI_ACCELERATOR_CHART="$ROOT/charts/ci-accelerator" CI_ACCELERATOR_CONSOLE_IMAGE_TAR="$IMAGE_TAR")
fi
echo "== install with ${RELEASE:+v$RELEASE }quickstart.sh (GitLab $gitlab)"
t0=$(date +%s)
if env RUNNER_TOKEN=glrt-quickstart-check ${overrides[@]+"${overrides[@]}"} \
    bash "$SCRIPT" --gitlab-url "$gitlab" --yes >"$LOGS/install.log" 2>&1; then
  pass "quickstart.sh installs in $(( $(date +%s) - t0 ))s"
else
  fail "quickstart.sh installs (see $LOGS/install.log)"; tail -30 "$LOGS/install.log"; exit 1
fi
check "it prints how to reach the console" grep -q 'ssh -N -L 8080:localhost:8080' "$LOGS/install.log"
check "the node is the build node" eval 'kc get nodes -l ci-accelerator.io/node-role=build -o name | grep -q node/'
check "every pod of the box runs" eval '! kc -n "$NS" get pods --no-headers | grep -v -E "Running|Completed"'

# job <pipeline id>: queues a job that builds an image, waits for it,
# writes its trace to $LOGS/job-<id>.trace and prints its state.
job() {
  local spec id state=""
  spec=$(python3 - "$BUILD_IMAGE" "$1" <<'PY'
import json, sys
print(json.dumps({"image": sys.argv[1], "pipeline_id": int(sys.argv[2]), "script": [
    'mkdir -p ctx && printf "FROM alpine:3.22\\nRUN sleep 2 && echo deps > /deps\\n" > ctx/Dockerfile',
    'buildctl --addr "$BUILDKIT_REMOTE_ADDR" --tlsdir "$BUILDKIT_TLS_DIR" build --frontend dockerfile.v0 --local context=ctx --local dockerfile=ctx --progress plain',
]}))
PY
)
  id=$(curl -fs -X POST -d "$spec" "${gitlab}e2e/jobs" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
  for _ in $(seq 1 150); do
    curl -fs "${gitlab}e2e/jobs/$id" >"$LOGS/job-$1.json" || true
    state=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["state"])' "$LOGS/job-$1.json" 2>/dev/null || true)
    case "$state" in success|failed) break ;; esac
    sleep 2
  done
  python3 -c 'import json,re,sys
t = re.sub(r"\x1b\[[0-9;]*[A-Za-z]|\r", "", json.load(open(sys.argv[1]))["trace"])
print("\n".join(re.sub(r"^\d{4}-\d\d-\d\dT[0-9:.]+Z [0-9]+[OE]\+? ?", "", l) for l in t.split("\n")))' \
    "$LOGS/job-$1.json" >"$LOGS/job-$1.trace" 2>/dev/null || true
  echo "${state:-unknown}"
}

echo "== a CI job builds an image on the box"
check "a CI job builds an image" test "$(job 1)" = success
check "the second build finds the first one's cache" eval '[ "$(job 2)" = success ] && grep -A1 "RUN sleep 2" "$LOGS/job-2.trace" | grep -q "^#[0-9]* CACHED$"'

echo "== the console saw the builds"
kc -n "$NS" port-forward svc/ci-accelerator-console 18080:8080 >"$LOGS/port-forward.log" 2>&1 &
pf=$!
console_builds() {
  curl -fs "http://localhost:18080/api/v1/builds?limit=10" >"$LOGS/console-builds.json" 2>/dev/null \
    && python3 -c 'import json,sys; sys.exit(0 if len(json.load(open(sys.argv[1]))["builds"]) >= 2 else 1)' "$LOGS/console-builds.json"
}
ok=1
for _ in $(seq 1 30); do console_builds && { ok=0; break; }; sleep 2; done
check "the console lists both builds" test "$ok" = 0
check "the console's pages answer" eval 'curl -fs http://localhost:18080/pipelines >/dev/null'

echo "== a second run keeps values.yaml and the daemons"
printf '# kept-by-the-user\n' >> /var/lib/ci-accelerator/values.yaml
uid0=$(kc -n "$NS" get pods -l app.kubernetes.io/name=buildkit -o jsonpath='{.items[*].metadata.uid}')
# The same script and chart source as the first run: in release mode the
# user's re-run pulls the chart from the registry again; forcing the source
# tree's chart here fails without make deps (nothing is vendored).
if env RUNNER_TOKEN=glrt-quickstart-check ${overrides[@]+"${overrides[@]}"} \
    bash "$SCRIPT" --gitlab-url "$gitlab" --yes >"$LOGS/install-again.log" 2>&1; then
  pass "quickstart.sh runs again"
else
  fail "quickstart.sh runs again (see $LOGS/install-again.log)"; tail -20 "$LOGS/install-again.log"
fi
check "it keeps the existing values.yaml" eval 'grep -q "Keeping the existing" "$LOGS/install-again.log" && grep -q "^# kept-by-the-user" /var/lib/ci-accelerator/values.yaml'
check "and does not restart the daemons" test "$uid0" = "$(kc -n "$NS" get pods -l app.kubernetes.io/name=buildkit -o jsonpath='{.items[*].metadata.uid}')"
kill "$pf" 2>/dev/null || true

if [ "$FAILED" -gt 0 ]; then
  kc get pods -A -o wide >"$LOGS/pods.txt" 2>&1 || true
  kc -n "$NS" describe pods >"$LOGS/describe.txt" 2>&1 || true
  kc get events -A --sort-by=.lastTimestamp >"$LOGS/events.txt" 2>&1 || true
fi

echo "== uninstall"
if bash "$SCRIPT" --uninstall --yes >"$LOGS/uninstall.log" 2>&1; then
  pass "quickstart.sh --uninstall"
else
  fail "quickstart.sh --uninstall (see $LOGS/uninstall.log)"
fi
check "k3s and the build cache are gone" eval '[ ! -e /usr/local/bin/k3s ] && [ ! -e /var/lib/ci-accelerator ]'

echo "== result: $PASSED passed, $FAILED failed (logs: $LOGS)"
[ "$FAILED" = 0 ]
