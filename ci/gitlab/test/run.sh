#!/usr/bin/env bash
# Runs the job script of ci/gitlab/build.yml in its own image with a fake
# buildctl that records its arguments and environment, once per case. The
# CIA_* variables stand in for the inputs as GitLab passes them.
#   ci/gitlab/test/run.sh            (needs docker and ruby)
set -euo pipefail
cd "$(dirname "$0")/../../.."
COMPONENT=ci/gitlab/build.yml
IMAGE=$(ruby -ryaml -e 'puts YAML.load_stream(File.read(ARGV[0]))[0]["spec"]["inputs"]["buildkit-image"]["default"]' "$COMPONENT")
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
ruby -ryaml -e 'puts YAML.load_stream(File.read(ARGV[0]))[1].values[0]["script"].join("\n")' "$COMPONENT" >"$WORK/job.sh"

# The fake buildctl: argv and environment to files, plain progress and a
# metadata file like the real one, or a failure on demand.
cat >"$WORK/buildctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" >/out/argv
env >/out/env
[ -n "${DOCKER_CONFIG:-}" ] && cat "$DOCKER_CONFIG/config.json" >/out/config.json 2>/dev/null
case "${FAKE:-ok}" in
  fail) echo "error: failed to solve: process did not complete successfully" >&2; exit 1 ;;
  # Like buildctl: SIGTERM cancels the build and it exits 1; or a client
  # that does not stop on SIGTERM at all.
  timeout) trap 'echo "error: failed to solve: Canceled: context canceled" >&2; exit 1' TERM; sleep 600 >/dev/null 2>&1 & wait; exit 1 ;;
  hang) trap "" TERM; sleep 20 >/dev/null 2>&1; echo not killed >/out/hang; exit 1 ;;
esac
echo "#1 [internal] load build definition from Dockerfile"
echo "#1 DONE 0.0s"
echo "#5 [internal] load build context"
[ "${FAKE:-ok}" = nocontext ] || echo "#5 transferring context: 1.23MB 0.1s done"
echo "#4 [base 1/3] FROM docker.io/library/alpine:3.22"
echo "#4 DONE 0.0s"
echo "#6 [base 2/3] RUN apk add curl"
echo "#6 CACHED"
echo "#7 [3/3] COPY . ."
echo "#7 DONE 0.4s"
echo "#8 exporting to image"
echo "#8 DONE 1.2s"
# Like buildctl: the metadata file only when there is an exporter.
out=""; meta=""
while [ $# -gt 0 ]; do case "$1" in --metadata-file) meta=$2 ;; --output) out=$2 ;; esac; shift; done
[ -z "$out" ] || printf '{\n  "containerimage.digest": "sha256:%064d"\n}\n' 1 >"$meta"
EOF
chmod +x "$WORK/buildctl"
mkdir -p "$WORK/repo/app" && printf 'FROM alpine\n' >"$WORK/repo/Dockerfile" && printf 'FROM alpine\n' >"$WORK/repo/app/Containerfile"
# A File-type CI/CD variable: GitLab writes the value to <project dir>.tmp/
# and the variable holds that path.
mkdir -p "$WORK/repo/p.tmp" && printf '//npm.example.com/:_authToken=x\n' >"$WORK/repo/p.tmp/NPMRC"

# case <name> <expected exit 0|1> [VAR=value ...] -- checks via grep on /out files
# (UNSET="A B" removes those variables from the job's environment)
run_case() {
  local name=$1 want=$2; shift 2
  rm -rf "$WORK/out" && mkdir -p "$WORK/out"
  rm -f "$WORK/repo/build.env" "$WORK/repo/build-metadata.json"
  local env=(-e CIA_CONTEXT=. -e CIA_DOCKERFILE= -e CIA_TARGET= -e CIA_PLATFORM= -e CIA_BUILD_ARGS=
    -e CIA_SECRETS= -e "CIA_IMAGES=registry.example.com/g/p:abc123" -e CIA_PUSH=true -e CIA_REGISTRY_AUTH=job-token
    -e CIA_LABELS=true -e CIA_PROVENANCE=false -e CIA_TIMEOUT_MINUTES=60 -e CIA_BUILDCTL_ARGS=
    -e BUILDKIT_REMOTE_ADDR=tcp://box-buildkit:1234 -e BUILDKIT_TLS_DIR=/certs
    -e CI_REGISTRY=registry.example.com -e CI_REGISTRY_USER=gitlab-ci-token -e CI_REGISTRY_PASSWORD=job-token-value
    -e CI_PROJECT_URL=https://gitlab.example.com/g/p -e CI_COMMIT_SHA=0123abc
    -e 'DOCKER_AUTH_CONFIG={"auths":{"registry.example.com":{"auth":"cmVhZG9ubHk6eA=="}}}')
  while [ $# -gt 0 ] && [ "$1" != -- ]; do env+=(-e "$1"); shift; done
  shift
  local status=0
  docker run --rm --entrypoint sh "${env[@]}" -v "$WORK/job.sh:/job.sh:ro" -v "$WORK/buildctl:/usr/local/bin/buildctl:ro" \
    -v "$WORK/out:/out" -v "$WORK/repo:/repo" -w /repo "$IMAGE" -c 'PATH=/usr/local/bin:$PATH; [ -z "${UNSET:-}" ] || unset $UNSET UNSET; sh /job.sh' \
    >"$WORK/out/stdout" 2>&1 || status=$?
  [ "$status" = 0 ] && got=0 || got=1
  local ok=1
  [ "$got" = "$want" ] || { ok=0; echo "    exit $status, wanted $( [ "$want" = 0 ] && echo 0 || echo non-zero)"; }
  while [ $# -gt 0 ]; do
    local file=${1%%:*} pat=${1#*:}; shift
    if [ "${pat:0:1}" = "!" ]; then
      ! grep -qE -- "${pat:1}" "$WORK/out/$file" 2>/dev/null || { ok=0; echo "    $file has: ${pat:1}"; }
    else
      grep -qE -- "$pat" "$WORK/out/$file" 2>/dev/null || { ok=0; echo "    $file lacks: $pat"; }
    fi
  done
  if [ "$ok" = 1 ]; then pass=$((pass + 1)); echo "  ok    $name"; else failn=$((failn + 1)); echo "  FAIL  $name"; sed 's/^/      | /' "$WORK/out/stdout" | tail -n 8; fi
  cp "$WORK/repo/build.env" "$WORK/out/build.env" 2>/dev/null || true
}

echo "component job script, in $IMAGE"
# The README's table names every input, and the job's buildctl is the
# daemons' BuildKit release (tag and digest of charts/buildkit).
pass=0 failn=0
for input in $(ruby -ryaml -e 'puts YAML.load_stream(File.read(ARGV[0]))[0]["spec"]["inputs"].keys' "$COMPONENT"); do
  grep -q "^| \`$input\` |" ci/gitlab/README.md || { echo "  FAIL  ci/gitlab/README.md does not list the input $input"; failn=$((failn + 1)); }
done
[ "$failn" = 0 ] && echo "  ok    the README lists every input" && pass=$((pass + 1))
app=$(awk '/^appVersion:/ {gsub(/"/, "", $2); print $2; exit}' charts/buildkit/Chart.yaml)
digest=$(awk '/^  digest:/ {gsub(/"/, "", $2); print $2; exit}' charts/buildkit/values.yaml)
if [ "$IMAGE" = "moby/buildkit:$app@$digest" ]; then echo "  ok    buildkit-image is the daemons' release ($app)"; pass=$((pass + 1));
else echo "  FAIL  buildkit-image $IMAGE, the daemons run moby/buildkit:$app@$digest"; failn=$((failn + 1)); fi
# The box's install notes print the component's include with its
# integrity: the sha256 of this file.
want="sha256-$(openssl dgst -sha256 -binary "$COMPONENT" | base64)"
got=$(sed -n '/define "ci-accelerator.componentIntegrity"/{n;p;}' charts/ci-accelerator/templates/_helpers.tpl)
if [ "$got" = "$want" ]; then echo "  ok    the box's notes print the component's integrity"; pass=$((pass + 1));
else echo "  FAIL  charts/ci-accelerator/templates/_helpers.tpl componentIntegrity is $got, the component's is $want"; failn=$((failn + 1)); fi
run_case "defaults: context, Dockerfile, push with the job token" 0 -- \
  'argv:^context=\.$' 'argv:^dockerfile=\.$' 'argv:^filename=Dockerfile$' \
  'argv:^type=image,"name=registry\.example\.com/g/p:abc123",push=true$' \
  'argv:^label:org\.opencontainers\.image\.revision=0123abc$' 'argv:^--progress$' \
  'config.json:"registry\.example\.com":\{"auth":"Z2l0bGFiLWNpLXRva2VuOmpvYi10b2tlbi12YWx1ZQ=="\}' \
  'env:!^DOCKER_AUTH_CONFIG=' 'stdout:Dockerfile steps: 1 from cache, 1 run; context sent: 1\.23MB'
# Lines of build-args and secrets may be indented (a YAML block).
run_case "indented build-args and secrets lines" 0 "CIA_BUILD_ARGS=  VERSION=1.2 3" "CIA_SECRETS=  npmrc = NPM_TOKEN " NPM_TOKEN=s3cr3t -- \
  'argv:^build-arg:VERSION=1\.2 3$' 'argv:^id=npmrc,env=NPM_TOKEN$'
# A context BuildKit fetched itself (a git URL) prints no transfer line:
# the build passed, so does the job.
run_case "no context transfer in the log" 0 FAKE=nocontext -- \
  'stdout:context sent: unknown'
run_case "the digest goes to build.env" 0 -- \
  'stdout:digest sha256:0{63}1'
grep -q '^IMAGE_REF=registry.example.com/g/p@sha256:' "$WORK/out/build.env" && echo "  ok    build.env: IMAGE_REF without the tag" && pass=$((pass + 1)) \
  || { echo "  FAIL  build.env: $(cat "$WORK/out/build.env" 2>/dev/null)"; failn=$((failn + 1)); }
run_case "Dockerfile elsewhere, target, platforms, build args, two images" 0 \
  CIA_CONTEXT=app CIA_DOCKERFILE=app/Containerfile CIA_TARGET=runtime CIA_PLATFORM=linux/amd64,linux/arm64 \
  "CIA_BUILD_ARGS=$(printf 'NODE_VERSION=22\nGREETING=hello world\n')" \
  "CIA_IMAGES=$(printf 'r.example.com:5000/g/p:1.2\n  r.example.com:5000/g/p:latest\n')" -- \
  'argv:^context=app$' 'argv:^dockerfile=app$' 'argv:^filename=Containerfile$' 'argv:^target=runtime$' \
  'argv:^platform=linux/amd64,linux/arm64$' 'argv:^build-arg:NODE_VERSION=22$' 'argv:^build-arg:GREETING=hello world$' \
  'argv:^type=image,"name=r\.example\.com:5000/g/p:1\.2,r\.example\.com:5000/g/p:latest",push=true$'
grep -q '^IMAGE_REF=r.example.com:5000/g/p@sha256:' "$WORK/out/build.env" && echo "  ok    build.env: a registry port stays" && pass=$((pass + 1)) \
  || { echo "  FAIL  build.env with a port: $(cat "$WORK/out/build.env" 2>/dev/null)"; failn=$((failn + 1)); }
run_case "a secret from a CI/CD variable, by name only" 0 "CIA_SECRETS=npmrc=NPM_TOKEN" NPM_TOKEN=s3cr3t -- \
  'argv:^id=npmrc,env=NPM_TOKEN$' 'argv:!s3cr3t'
run_case "a secret from a File-type variable mounts the file, not its path" 0 "CIA_SECRETS=npmrc=NPMRC" \
  CI_PROJECT_DIR=/repo/p NPMRC=/repo/p.tmp/NPMRC -- 'argv:^id=npmrc,src=/repo/p\.tmp/NPMRC$' 'argv:!env=NPMRC'
run_case "a variable that only looks like a file path stays a value" 0 "CIA_SECRETS=npmrc=NPMRC" \
  CI_PROJECT_DIR=/repo/p NPMRC=/repo/p.tmp/MISSING -- 'argv:^id=npmrc,env=NPMRC$'
run_case "a secret whose variable is not set" 1 "CIA_SECRETS=npmrc=NOT_SET" -- \
  'stdout:the CI/CD variable NOT_SET is not set' 'argv:!.'
run_case "a secret name that is not a variable name is refused, not evaluated" 1 'CIA_SECRETS=x=A;touch /out/pwned' -- \
  'stdout:is not a variable name' 'argv:!.'
[ ! -e "$WORK/out/pwned" ] || { echo "  FAIL  the secret name was evaluated"; failn=$((failn + 1)); }
run_case "a build argument without =" 1 "CIA_BUILD_ARGS=JUSTKEY" -- 'stdout:is not KEY=VALUE'
run_case "push=false: no output, no metadata, still a summary" 0 CIA_PUSH=false -- 'argv:!^--output$' 'stdout:!pushing' 'stdout:Dockerfile steps: 1 from cache, 1 run' 'stdout:!digest'
run_case "docker-auth-config keeps DOCKER_AUTH_CONFIG" 0 CIA_REGISTRY_AUTH=docker-auth-config -- \
  'env:^DOCKER_AUTH_CONFIG=' 'config.json:!.'
run_case "labels and provenance switches" 0 CIA_LABELS=false CIA_PROVENANCE=true -- \
  'argv:!^label:' 'argv:^attest:provenance=mode=min$'
run_case "the job's trace names the build's job while it runs" 0 TRACEPARENT=00-01abcdef0000b33b2ec9000419350c4d-0000000419350c4d-01 -- \
  'argv:^ci-accelerator\.io/traceparent=00-01abcdef0000b33b2ec9000419350c4d-0000000419350c4d-01$'
run_case "without a trace, no option" 0 UNSET=TRACEPARENT -- 'argv:!traceparent'
run_case "extra buildctl arguments, one per line" 0 "CIA_BUILDCTL_ARGS=$(printf -- '--no-cache\n')" -- 'argv:^--no-cache$'
run_case "a failing build fails the job" 1 FAKE=fail -- 'stdout:buildctl exit status 1'
[ -f "$WORK/repo/build.env" ] && [ -f "$WORK/repo/build-metadata.json" ] && echo "  ok    the artifact files exist after a failure" && pass=$((pass + 1)) \
  || { echo "  FAIL  no artifact files after a failure"; failn=$((failn + 1)); }
run_case "a build cut by the timeout says so (buildctl exits 1 on SIGTERM)" 1 FAKE=timeout CIA_TIMEOUT_SECONDS=2 -- \
  'stdout:did not finish in 60 minutes' 'stdout:!buildctl exit status'
run_case "a buildctl that ignores SIGTERM is killed after the grace period" 1 FAKE=hang CIA_TIMEOUT_SECONDS=1 CIA_TIMEOUT_GRACE_SECONDS=2 -- \
  'stdout:did not finish in 60 minutes' 'hang:!.'
run_case "without a GitLab registry, push=false builds without credentials" 0 CIA_PUSH=false \
  UNSET="CI_REGISTRY CI_REGISTRY_USER CI_REGISTRY_PASSWORD" -- \
  'argv:!^--output$' 'config.json:!"auth"' 'stdout:no GitLab container registry' 'stdout:!parameter not set'
run_case "without a GitLab registry, push with the job token says what to do" 1 \
  UNSET="CI_REGISTRY CI_REGISTRY_USER CI_REGISTRY_PASSWORD" -- \
  'stdout:registry-auth' 'stdout:!parameter not set' 'argv:!.'
run_case "the Dependency Proxy gets the job's credentials too" 0 CI_DEPENDENCY_PROXY_SERVER=gitlab.example.com:443 \
  CI_DEPENDENCY_PROXY_USER=gitlab-ci-token CI_DEPENDENCY_PROXY_PASSWORD=dp-token -- \
  'config.json:"gitlab\.example\.com:443":\{"auth":"Z2l0bGFiLWNpLXRva2VuOmRwLXRva2Vu"\}' \
  'config.json:"registry\.example\.com":\{"auth":"Z2l0bGFiLWNpLXRva2VuOmpvYi10b2tlbi12YWx1ZQ=="\}'
run_case "a Dependency Proxy on the registry's host is one entry" 0 CI_DEPENDENCY_PROXY_SERVER=registry.example.com \
  CI_DEPENDENCY_PROXY_USER=gitlab-ci-token CI_DEPENDENCY_PROXY_PASSWORD=dp-token -- \
  'config.json:^\{"auths":\{"registry\.example\.com":\{"auth":"[^"]*"\}\}\}$'
run_case "not on a ci-accelerator runner" 1 BUILDKIT_REMOTE_ADDR= -- 'stdout:must run on a ci-accelerator runner' 'argv:!.'
run_case "a missing Dockerfile" 1 CIA_DOCKERFILE=nope/Dockerfile -- 'stdout:no Dockerfile at nope/Dockerfile'

echo "component: $pass passed, $failn failed"
[ "$failn" = 0 ]
