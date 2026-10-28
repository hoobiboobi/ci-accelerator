#!/usr/bin/env bash
# A real GitLab for the component's tests: GitLab CE in Docker, pinned by
# digest, at the oldest version the component supports (17.9, amd64 only)
# and at a current one (the runner's minor line).
#
#   ci/gitlab/test/gitlab.sh lint [min|current|all]
#       boots each GitLab, has it lint and simulate pipelines that include
#       the component (ci/gitlab/test/lint.rb), and removes it again.
#       COMPONENT_URL=<url> includes the component from there (the release
#       checks the published file at its tag) instead of from a project in
#       the booted GitLab that holds the working tree's file;
#       COMPONENT_FILE=<file> tests another file than ci/gitlab/build.yml
#       (an earlier release's, to see the tests fail on it).
#   ci/gitlab/test/gitlab.sh up <name> <min|current> <out dir> [--network <net>] [--registry]
#       boots one and leaves it running (the e2e's component scenario):
#       <out dir>/port (the API on 127.0.0.1), <out dir>/token (an admin
#       token, mode 0600; never printed).
#   ci/gitlab/test/gitlab.sh down <name>
#
# Needs docker, curl, openssl and ruby. A GitLab takes some minutes to
# boot (more for 17.9 on arm64, which Docker runs emulated).
set -euo pipefail
cd "$(dirname "$0")/../../.."

# 17.9.8 (2025-05-07), the last patch of the oldest minor the component
# claims (include:integrity is 17.9); amd64 only. 19.4.3 (2026-10-21), the
# minor of the runner the box installs (19.4), a security patch (an
# unauthenticated RCE in its regular expressions), multi-arch.
GITLAB_MIN=gitlab/gitlab-ce:17.9.8-ce.0@sha256:c34e039c9505ef945b97f5ffa61841f7e45abccc347680c87a250fb21261eb39
GITLAB_MIN_PLATFORM=linux/amd64
GITLAB_CURRENT=gitlab/gitlab-ce:19.4.3-ce.0@sha256:483fb4cc5c75237a7a6c7c8b8b2a1832ed790d86e0449ff55e8b6f6d62d2d5f5
GITLAB_CURRENT_PLATFORM=
BOOT_TIMEOUT=${GITLAB_BOOT_TIMEOUT:-1800}

say() { echo "$(date +%H:%M:%S) $*"; }

# The memory-constrained settings of GitLab's documentation ("Running GitLab
# in a memory-constrained environment"), no monitoring, no Let's Encrypt.
omnibus_config() {
  local name=$1 registry=$2
  printf '%s\n' \
    "external_url 'http://$name'" \
    "letsencrypt['enable'] = false" \
    "gitlab_rails['usage_ping_enabled'] = false" \
    "puma['worker_processes'] = 0" \
    "sidekiq['concurrency'] = 10" \
    "prometheus_monitoring['enable'] = false" \
    "gitlab_rails['env'] = { 'MALLOC_CONF' => 'dirty_decay_ms:1000,muzzy_decay_ms:1000' }" \
    "gitaly['env'] = { 'MALLOC_CONF' => 'dirty_decay_ms:1000,muzzy_decay_ms:1000', 'GITALY_COMMAND_SPAWN_MAX_PARALLEL' => '2' }" \
    "nginx['listen_addresses'] = ['*', '[::]']"
  # IPv6 too: on a dual-stack Docker network (kind's) the container's name
  # resolves to both, and GitLab fetching its own URL took the IPv6 one.
  if [ "$registry" = 1 ]; then
    # Plain HTTP: the daemons are told so (buildkitd.toml [registry."<name>:5050"] http = true).
    echo "registry_external_url 'http://$name:5050'"
    echo "registry_nginx['listen_addresses'] = ['*', '[::]']"
  fi
}

# up <name> <min|current> <out dir> [--network <net>] [--registry]
up() {
  local name=$1 which=$2 out=$3; shift 3
  local network="" registry=0
  while [ $# -gt 0 ]; do
    case "$1" in
      --network) network=$2; shift 2 ;;
      --registry) registry=1; shift ;;
      *) echo "up: unknown option $1" >&2; return 2 ;;
    esac
  done
  local image platform
  case "$which" in
    min) image=$GITLAB_MIN platform=$GITLAB_MIN_PLATFORM ;;
    current) image=$GITLAB_CURRENT platform=$GITLAB_CURRENT_PLATFORM ;;
    *) echo "up: min or current, not $which" >&2; return 2 ;;
  esac
  mkdir -p "$out" && chmod 700 "$out" || return 1
  docker rm -f "$name" >/dev/null 2>&1 || true
  local args=(-d --name "$name" --hostname "$name" --shm-size 256m -p 127.0.0.1::80)
  [ -z "$network" ] || args+=(--network "$network")
  [ -z "$platform" ] || args+=(--platform "$platform")
  [ "$registry" = 0 ] || args+=(-p 127.0.0.1::5050)
  # The root password only seeds the account; nothing logs in with it.
  local password
  password="$(openssl rand -hex 16)Aa1"
  say "booting $image as $name"
  GITLAB_ROOT_PASSWORD=$password GITLAB_OMNIBUS_CONFIG=$(omnibus_config "$name" "$registry") \
    docker run "${args[@]}" -e GITLAB_ROOT_PASSWORD -e GITLAB_OMNIBUS_CONFIG "$image" >/dev/null || return 1
  docker port "$name" 80/tcp | head -n 1 | sed 's/.*://' >"$out/port"
  grep -qE '^[0-9]+$' "$out/port" || { echo "no published port for $name" >&2; return 1; }
  local start=$SECONDS
  until docker exec "$name" curl -fsS -o /dev/null 'http://localhost/-/readiness?all=1' 2>/dev/null; do
    if ! docker ps -q --filter "name=^$name\$" | grep -q .; then
      echo "GitLab $name exited:" >&2; docker logs --tail 40 "$name" >&2; return 1
    fi
    if [ $((SECONDS - start)) -gt "$BOOT_TIMEOUT" ]; then
      echo "GitLab $name not ready after ${BOOT_TIMEOUT}s:" >&2; docker logs --tail 40 "$name" >&2; return 1
    fi
    sleep 10
  done
  say "$name ready after $((SECONDS - start))s"
  # Sidekiq runs pipelines; its own health check answers on 8092.
  until docker exec "$name" curl -fsS -o /dev/null http://localhost:8092/readiness 2>/dev/null; do
    [ $((SECONDS - start)) -le "$BOOT_TIMEOUT" ] || { echo "Sidekiq of $name not ready" >&2; return 1; }
    sleep 5
  done
  # An admin token, created by GitLab and written straight to a file.
  ( umask 077
    docker exec "$name" gitlab-rails runner \
      "t = User.find_by_username('root').personal_access_tokens.create!(scopes: ['api', 'create_runner', 'write_repository'], name: 'tests', expires_at: 2.days.from_now); print t.token" \
      >"$out/token" )
  grep -q '^glpat-' "$out/token" || { echo "no token from $name" >&2; return 1; }
  # The component is fetched from a URL in the same Docker network.
  api "$out" PUT /application/settings allow_local_requests_from_web_hooks_and_services=true >/dev/null || return 1
  say "$name: token and settings in place ($((SECONDS - start))s)"
}

down() { docker rm -f "$1" >/dev/null 2>&1 || true; }

# api <out dir> <method> <path> [key=value ...]: the GitLab's REST API as
# the admin token (read from its file, never on a command line).
api() {
  local out=$1 method=$2 path=$3; shift 3
  local args=()
  for kv in "$@"; do args+=(--data-urlencode "$kv"); done
  curl -fsS -X "$method" -H @<(printf 'PRIVATE-TOKEN: %s\n' "$(cat "$out/token")") \
    "http://127.0.0.1:$(cat "$out/port")/api/v4$path" ${args[@]+"${args[@]}"}
}

# What to remove when the script ends, however it ends.
LEFT_CONTAINERS=()
LEFT_DIRS=()
leave() {
  local x
  for x in ${LEFT_CONTAINERS[@]+"${LEFT_CONTAINERS[@]}"}; do down "$x"; done
  for x in ${LEFT_DIRS[@]+"${LEFT_DIRS[@]}"}; do rm -rf "$x"; done
}

# lint_one <min|current>: boots that GitLab, runs the lint suite against it,
# removes it (and its token) again whatever happened.
lint_one() {
  local which=$1 name="cia-gitlab-$1" out version rc=0
  out=$(mktemp -d)
  LEFT_CONTAINERS+=("$name")
  LEFT_DIRS+=("$out")
  if up "$name" "$which" "$out" &&
    version=$(api "$out" GET /version | ruby -rjson -e 'puts JSON.parse(STDIN.read)["version"]'); then
    ruby ci/gitlab/test/lint.rb "http://127.0.0.1:$(cat "$out/port")" "$out/token" "http://$name" "$version" "${COMPONENT_FILE:-ci/gitlab/build.yml}" || rc=$?
  else
    echo "GitLab $which did not come up" >&2; rc=1
  fi
  down "$name"; rm -rf "$out"
  return "$rc"
}

case "${1:-}" in
  lint)
    trap leave EXIT
    rc=0
    case "${2:-all}" in
      all) lint_one current || rc=1; lint_one min || rc=1 ;;
      min | current) lint_one "$2" || rc=1 ;;
      *) echo "lint: min, current or all" >&2; exit 2 ;;
    esac
    exit "$rc" ;;
  up) shift; up "$@" ;;
  down) down "${2:?name}" ;;
  *) sed -n '2,/^# boot/p' "$0" >&2; exit 2 ;;
esac
