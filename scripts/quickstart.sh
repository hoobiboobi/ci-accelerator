#!/usr/bin/env bash
# Try ci-accelerator on one clean Linux machine: k3s (one node, also the
# build node), Helm, cert-manager and the box with its console. k3s, Helm,
# cert-manager and the chart are pinned and checked against a sha256 (the
# box's images come by tag, the daemons' by digest). For a first look
# before a real deployment, not for production: one node, no high
# availability, the console reachable only through an SSH tunnel.
#
#   sudo ./quickstart.sh --gitlab-url https://gitlab.example.com/   # asks for the runner's token
#   sudo ./quickstart.sh --gitlab-url ... --runner-token-file ./runner-token
#   sudo ./quickstart.sh --uninstall
#
# Options:
#   --gitlab-url URL         your GitLab (required to install)
#   --runner-token-file F    a file with the runner's token (glrt-...); without
#                            it the script asks, or reads RUNNER_TOKEN from the
#                            environment (not `sudo RUNNER_TOKEN=...`: that is
#                            visible in ps and kept in the shell history)
#   --gitlab-token-file F    optional: a GitLab token with read_api, so the
#                            console shows how long jobs waited for a runner
#                            and decides "files or node" for rebuilds
#   --version V              the release to install (default: this script's)
#   --build-dir DIR          the build cache (default /var/lib/ci-accelerator/buildkit)
#   --allow-docker           install next to Docker (its iptables rules can
#                            break pod networking)
#   --yes                    do not ask before installing
#   --uninstall              remove k3s, everything in it, and the build cache
#
# Running it again (a new release with --version, a token added) keeps
# /var/lib/ci-accelerator/values.yaml and what you put there.
#
# Needs: Ubuntu 24.04 or Debian 13 (or similar, systemd and cgroup v2),
# amd64 or arm64, 4 CPUs, 8 GB of memory and 40 GB of free disk.
set -euo pipefail

VERSION=0.3.2
CHARTS=oci://ghcr.io/hoobiboobi/charts
RELEASES=https://github.com/hoobiboobi/ci-accelerator/releases/download

# Pinned components (bump together with their checksums).
K3S_VERSION=v1.36.5+k3s1
K3S_SHA256_amd64=d73847bcd3c5fccef0115b372e2f9a91f3032dc84bbf71518a4617565294d313
K3S_SHA256_arm64=135e34cb9e8a1cfae3cb55577789e93501efe7edc080a8b2630458a85b07721e
K3S_INSTALL_SHA256=46177d4c99440b4c0311b67233823a8e8a2fc09693f6c89af1a7161e152fbfad
HELM_VERSION=v4.3.1
HELM_SHA256_amd64=0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b
HELM_SHA256_arm64=8cf8f66ecbc252ac8f7f06cca68aba88508ddfde3a6a34feb1678ad1694b4f88
CERT_MANAGER_VERSION=v1.21.3
CERT_MANAGER_SHA256=9ec67938b2981d8b333af61b3fcf53e26b4f6f5deb7ccda3de4ea116bb373a17
CERT_MANAGER_DIGESTS=(
  image.digest=sha256:9f4f96a33c7774cef24466fcd85031997531a03a8d117f151126baa8d1290dcc
  webhook.image.digest=sha256:055ac8e9de6a0ba6763794838fe38944acc650f68ebe1b01dd8be079e580f458
  cainjector.image.digest=sha256:083d0e906f8eb44e255fe4ff817ae54e49f9ed82e22f1011bb4e9e4f4b8a641f
  startupapicheck.image.digest=sha256:1c519d4afca1413d1e4ccd78bdc756184418ab55a34569a3b3de39d32f9974e1
)

NS=ci-accelerator
REL=ci-accelerator
STATE=/var/lib/ci-accelerator
TOOLS=/usr/local/lib/ci-accelerator
BUILD_DIR=$STATE/buildkit
KUBECONFIG=/etc/rancher/k3s/k3s.yaml
export KUBECONFIG
# For testing this script before a release: a chart directory instead of
# the registry, and a ci-accelerator image to load into k3s.
CHART=${CI_ACCELERATOR_CHART:-}
IMAGE_TAR=${CI_ACCELERATOR_IMAGE_TAR:-}

gitlab_url="" runner_token_file="" gitlab_token_file="" allow_docker=0 yes=0 uninstall=0
while [ $# -gt 0 ]; do
  case "$1" in
    --gitlab-url) gitlab_url=${2:?}; shift 2 ;;
    --runner-token-file) runner_token_file=${2:?}; shift 2 ;;
    --gitlab-token-file) gitlab_token_file=${2:?}; shift 2 ;;
    --version) VERSION=${2:?}; shift 2 ;;
    --build-dir) BUILD_DIR=${2:?}; shift 2 ;;
    --allow-docker) allow_docker=1; shift ;;
    --yes) yes=1; shift ;;
    --uninstall) uninstall=1; shift ;;
    -h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option $1 (see --help)" >&2; exit 2 ;;
  esac
done

say() { printf '\n==> %s\n' "$*"; }
die() { printf '\nERROR: %s\n' "$*" >&2; exit 1; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }

[ "$(id -u)" = 0 ] || die "run it as root (sudo)"
[ "$(uname -s)" = Linux ] || die "this is for Linux"
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m): amd64 or arm64 only" ;;
esac

# --- uninstall ---------------------------------------------------------------

if [ "$uninstall" = 1 ]; then
  [ -x /usr/local/bin/k3s-uninstall.sh ] || [ -d "$STATE" ] || die "nothing to uninstall"
  # The build cache may live outside $STATE (--build-dir); the install
  # recorded where. Only an absolute path other than / is removed.
  cache=$(cat "$STATE/build-dir" 2>/dev/null || echo "$BUILD_DIR")
  case "$cache" in /?*) ;; *) cache=$STATE/buildkit ;; esac
  if [ "$yes" = 0 ]; then
    [ -t 0 ] || die "not a terminal: pass --yes to remove without asking"
    read -r -p "Remove k3s, everything running in it, $STATE and the build cache in $cache? [y/N] " a
    case "$a" in y|Y|yes) ;; *) exit 1 ;; esac
  fi
  if [ -x /usr/local/bin/k3s-uninstall.sh ]; then
    say "Removing k3s"
    /usr/local/bin/k3s-uninstall.sh
  fi
  rm -rf "$STATE" "$TOOLS" "$cache"
  say "Removed."
  exit 0
fi

# --- checks ------------------------------------------------------------------

[ -n "$gitlab_url" ] || die "--gitlab-url is required (your GitLab, e.g. https://gitlab.example.com/)"
case "$gitlab_url" in http://*|https://*) ;; *) die "--gitlab-url must start with https:// (or http://)" ;; esac
case "$gitlab_url" in */) ;; *) gitlab_url=$gitlab_url/ ;; esac

for cmd in curl sha256sum tar systemctl; do
  command -v "$cmd" >/dev/null || die "$cmd is missing"
done
[ -d /run/systemd/system ] || die "systemd is needed (k3s runs as a systemd service)"
[ -f /sys/fs/cgroup/cgroup.controllers ] || die "cgroup v2 is needed (unified hierarchy)"

ours=0
[ -f "$STATE/.quickstart" ] && ours=1
if [ "$ours" = 0 ]; then
  if [ -e /usr/local/bin/k3s ] || [ -d /etc/rancher/k3s ]; then
    die "k3s is already installed here and not by this script: use a clean machine"
  fi
  if [ -e /etc/kubernetes/admin.conf ]; then
    die "this machine already runs Kubernetes (kubeadm): use a clean machine"
  fi
fi
if command -v docker >/dev/null && systemctl is-active --quiet docker 2>/dev/null; then
  [ "$allow_docker" = 1 ] || die "Docker runs here: its iptables rules can break the pods' network. Use a clean machine, or --allow-docker"
  warn "Docker runs here (--allow-docker)"
fi
mem_kb=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)
[ "$mem_kb" -ge $((3500 * 1024)) ] || die "at least 4 GB of memory is needed, 8 GB recommended"
[ "$mem_kb" -ge $((7500 * 1024)) ] || warn "less than 8 GB of memory: builds may be slow or fail"
[ "$(nproc)" -ge 2 ] || die "at least 2 CPUs are needed, 4 recommended"
mkdir -p "$(dirname "$BUILD_DIR")"
free_gb=$(df -Pk "$(dirname "$BUILD_DIR")" | awk 'NR == 2 {print int($4 / 1048576)}')
[ "$free_gb" -ge 20 ] || die "at least 20 GB of free disk is needed under $(dirname "$BUILD_DIR"), 40 GB recommended"
[ "$free_gb" -ge 40 ] || warn "less than 40 GB of free disk: the build cache has little room"
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q '^Status: active'; then
  warn "ufw is active: allow the pod and service networks, or pods cannot reach the API:"
  warn "  ufw allow from 10.42.0.0/16 to any && ufw allow from 10.43.0.0/16 to any"
fi

# The runner's token: from the environment, a file, or asked for. Never on
# the command line, where other users and the shell history see it.
runner_token=${RUNNER_TOKEN:-}
if [ -z "$runner_token" ] && [ -n "$runner_token_file" ]; then
  runner_token=$(tr -d '\r\n' <"$runner_token_file")
fi
if [ -z "$runner_token" ] && [ -t 0 ]; then
  read -r -s -p "The runner's token (glrt-...; create the runner in GitLab first): " runner_token
  echo
fi
[ -n "$runner_token" ] || die "no runner token: pass --runner-token-file, or run in a terminal to be asked"
case "$runner_token" in glrt-*) ;; *) warn "the runner token does not start with glrt-: is it a runner authentication token?" ;; esac
gitlab_token=""
if [ -n "$gitlab_token_file" ]; then
  gitlab_token=$(tr -d '\r\n' <"$gitlab_token_file")
fi

cat <<EOF

This installs, on this machine:
  - k3s $K3S_VERSION (Kubernetes, one node, also the build node)
  - Helm $HELM_VERSION (in $TOOLS)
  - cert-manager $CERT_MANAGER_VERSION
  - ci-accelerator $VERSION: build daemons, a GitLab runner for $gitlab_url
    and the build console; the build cache in $BUILD_DIR
For trying it out, not for production. Remove it all with --uninstall.
EOF
if [ "$yes" = 0 ]; then
  [ -t 0 ] || die "not a terminal: pass --yes to install without asking"
  read -r -p "Go on? [y/N] " a
  case "$a" in y|Y|yes) ;; *) exit 1 ;; esac
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# fetch <url> <file> <sha256>
fetch() {
  curl -fsSL --retry 3 --retry-delay 2 -o "$2" "$1" || die "download failed: $1"
  echo "$3  $2" | sha256sum -c --quiet - || die "checksum mismatch: $1"
}

mkdir -p "$STATE"
touch "$STATE/.quickstart"

# --- k3s ---------------------------------------------------------------------

if [ ! -x /usr/local/bin/k3s ]; then
  say "Installing k3s $K3S_VERSION"
  tag=${K3S_VERSION//+/%2B}
  case "$ARCH" in
    amd64) bin=k3s sum=$K3S_SHA256_amd64 ;;
    arm64) bin=k3s-arm64 sum=$K3S_SHA256_arm64 ;;
  esac
  fetch "https://github.com/k3s-io/k3s/releases/download/$tag/$bin" "$WORK/k3s" "$sum"
  fetch "https://raw.githubusercontent.com/k3s-io/k3s/$tag/install.sh" "$WORK/install.sh" "$K3S_INSTALL_SHA256"
  install -m 0755 -o root -g root "$WORK/k3s" /usr/local/bin/k3s
  mkdir -p /etc/rancher/k3s
  # One node, so no taint: cert-manager, the runner and the console run on
  # the build node too. host-gw: no VXLAN device on a single node.
  cat >/etc/rancher/k3s/config.yaml <<EOF
write-kubeconfig-mode: "0600"
disable: [traefik, servicelb]
node-label: ["ci-accelerator.io/node-role=build"]
flannel-backend: host-gw
EOF
  INSTALL_K3S_SKIP_DOWNLOAD=true INSTALL_K3S_VERSION="$K3S_VERSION" sh "$WORK/install.sh"
fi
kubectl() { /usr/local/bin/k3s kubectl "$@"; }
say "Waiting for Kubernetes"
# The API server answers before the node has registered, and `kubectl wait`
# fails at once when nothing matches: wait for the node itself first.
for _ in $(seq 1 90); do [ -n "$(kubectl get nodes -o name 2>/dev/null)" ] && break; sleep 2; done
kubectl wait node --all --for=condition=Ready --timeout=5m >/dev/null
for d in coredns local-path-provisioner; do
  for _ in $(seq 1 60); do kubectl -n kube-system get deploy "$d" >/dev/null 2>&1 && break; sleep 2; done
  kubectl -n kube-system rollout status deploy "$d" --timeout=5m >/dev/null
done

# --- Helm --------------------------------------------------------------------

HELM=$TOOLS/helm-$HELM_VERSION/helm
if [ ! -x "$HELM" ]; then
  say "Installing Helm $HELM_VERSION"
  case "$ARCH" in
    amd64) sum=$HELM_SHA256_amd64 ;;
    arm64) sum=$HELM_SHA256_arm64 ;;
  esac
  fetch "https://get.helm.sh/helm-$HELM_VERSION-linux-$ARCH.tar.gz" "$WORK/helm.tgz" "$sum"
  mkdir -p "$(dirname "$HELM")"
  tar -xzf "$WORK/helm.tgz" -C "$(dirname "$HELM")" --strip-components=1 "linux-$ARCH/helm"
fi
helm() { "$HELM" "$@"; }

# --- cert-manager ------------------------------------------------------------

say "Installing cert-manager $CERT_MANAGER_VERSION"
helm pull oci://quay.io/jetstack/charts/cert-manager --version "$CERT_MANAGER_VERSION" -d "$WORK" >/dev/null
echo "$CERT_MANAGER_SHA256  $WORK/cert-manager-$CERT_MANAGER_VERSION.tgz" | sha256sum -c --quiet - \
  || die "checksum mismatch: the cert-manager chart"
cm_set=(--set crds.enabled=true)
for d in "${CERT_MANAGER_DIGESTS[@]}"; do cm_set+=(--set "$d"); done
helm upgrade --install cert-manager "$WORK/cert-manager-$CERT_MANAGER_VERSION.tgz" -n cert-manager --create-namespace \
  "${cm_set[@]}" --wait --timeout 5m >/dev/null

# --- the box -----------------------------------------------------------------

say "Installing ci-accelerator $VERSION"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl label namespace "$NS" --overwrite pod-security.kubernetes.io/enforce=privileged >/dev/null
# Secrets from files, so the tokens never appear in a command line.
umask 077
printf '%s' "$runner_token" >"$WORK/runner-token"
: >"$WORK/empty"
kubectl -n "$NS" create secret generic gitlab-runner-token \
  --from-file=runner-token="$WORK/runner-token" --from-file=runner-registration-token="$WORK/empty" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
if [ -n "$gitlab_token" ]; then
  printf '%s' "$gitlab_token" >"$WORK/gitlab-token"
  kubectl -n "$NS" create secret generic gitlab-api-token --from-file=token="$WORK/gitlab-token" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
fi
mkdir -p "$BUILD_DIR" "$STATE"
printf '%s\n' "$BUILD_DIR" >"$STATE/build-dir"

if [ -z "$CHART" ]; then
  helm pull "$CHARTS/ci-accelerator" --version "$VERSION" -d "$WORK" >/dev/null
  # The release's checksums, from where this script itself comes from.
  curl -fsSL --retry 3 -o "$WORK/SHA256SUMS" "$RELEASES/v$VERSION/SHA256SUMS" || die "download failed: SHA256SUMS of v$VERSION"
  (cd "$WORK" && grep " ci-accelerator-$VERSION.tgz\$" SHA256SUMS | sha256sum -c --quiet -) \
    || die "the chart does not match the release's SHA256SUMS"
  CHART=$WORK/ci-accelerator-$VERSION.tgz
fi

# A second run keeps the values file: it is where sign-in, a different
# cache path or anything else the user added live. Flags that change values
# are applied with --set and printed, to be copied into the file.
value_set=()
if [ -f "$STATE/values.yaml" ] && grep -q '^console:' "$STATE/values.yaml"; then
  # 0.3.0 renamed the console's objects and values: no in-place upgrade.
  die "this machine runs ci-accelerator 0.2.x, which $VERSION cannot upgrade in place (the console became the control plane, with new names). Remove the release, keeping k3s and the build cache:
  sudo KUBECONFIG=$KUBECONFIG $HELM uninstall $REL -n $NS --wait
  sudo k3s kubectl -n $NS delete secret $REL-buildkit-tls
(the daemons' certificate outlives the release; the new one names the build router), then in $STATE/values.yaml rename the top-level console: to control-plane: or delete the file to start over, and run this script again (with --gitlab-token-file to keep reading GitLab with a token). The old console's history stays on the PersistentVolumeClaim $REL-console-data until you delete it."
fi
if [ -f "$STATE/values.yaml" ]; then
  say "Keeping the existing $STATE/values.yaml (delete it to start over)"
  grep -q "gitlabUrl: $gitlab_url\$" "$STATE/values.yaml" || warn "$STATE/values.yaml names another GitLab than --gitlab-url: the file wins"
  if [ -n "$gitlab_token" ] && ! grep -q "existingSecret: gitlab-api-token" "$STATE/values.yaml"; then
    value_set+=(--set control-plane.gitlab.token.existingSecret=gitlab-api-token)
    warn "the GitLab token is applied with --set; add control-plane.gitlab.token.existingSecret: gitlab-api-token to $STATE/values.yaml to keep it"
  fi
else
# The daemon on this one node: half the CPUs and a quarter of the memory
# requested, three quarters of the memory at most; the rest is for k3s, the
# runner, the control plane and the CI jobs.
cpus=$(nproc)
mem_mi=$(awk '/^MemTotal:/ {print int($2 / 1024)}' /proc/meminfo)
req_cpu=$(( cpus / 2 > 1 ? cpus / 2 : 1 ))
req_mem=$(( mem_mi / 4 ))
lim_mem=$(( mem_mi * 3 / 4 ))
cat >"$STATE/values.yaml" <<EOF
# Written by quickstart.sh on the first install and left alone after that:
# edit it and apply with helm upgrade -f (the install prints the command).
global:
  jobs:
    architecture: $ARCH
  console:
    # Through the SSH tunnel the install prints: every job links here.
    url: http://localhost:8080
gitlab-runner:
  gitlabUrl: $gitlab_url
  runners:
    secret: gitlab-runner-token
buildkit:
  storage:
    type: hostPath
    hostPath:
      path: $BUILD_DIR
  # Sized for this node ($cpus CPUs, ${mem_mi}Mi) at the first install.
  resources:
    requests: {cpu: "$req_cpu", memory: ${req_mem}Mi}
    limits: {memory: ${lim_mem}Mi}
control-plane:
  enabled: true
EOF
if [ -n "$gitlab_token" ]; then
  cat >>"$STATE/values.yaml" <<EOF
  gitlab:
    token:
      existingSecret: gitlab-api-token
EOF
fi
fi
image_set=()
if [ -n "$IMAGE_TAR" ]; then
  /usr/local/bin/k3s ctr images import "$IMAGE_TAR" >/dev/null
  ref=$(tar -xOf "$IMAGE_TAR" manifest.json | sed -n 's/.*"RepoTags":\["\([^"]*\)".*/\1/p')
  image_set=(--set "control-plane.image.repository=${ref%:*}" --set "control-plane.image.tag=${ref##*:}" --set control-plane.image.pullPolicy=Never
    --set "buildkit.diskProbe.image.repository=${ref%:*}" --set "buildkit.diskProbe.image.tag=${ref##*:}" --set buildkit.diskProbe.image.pullPolicy=Never)
fi
helm upgrade --install "$REL" "$CHART" -n "$NS" -f "$STATE/values.yaml" ${image_set[@]+"${image_set[@]}"} ${value_set[@]+"${value_set[@]}"} \
  --wait --timeout 10m >"$STATE/install.log" 2>&1 || { cat "$STATE/install.log" >&2; die "the install failed (log: $STATE/install.log)"; }

# --- done --------------------------------------------------------------------

host=$(hostname -f 2>/dev/null || hostname)
cat <<EOF

Done. ci-accelerator $VERSION runs on this machine.

$(helm get notes "$REL" -n "$NS")

The console (no sign-in: reachable only through a tunnel). On this machine:
  sudo k3s kubectl -n $NS port-forward svc/$REL-web 8080:8080
and from your computer:
  ssh -N -L 8080:localhost:8080 $(logname 2>/dev/null || echo you)@$host
then open http://localhost:8080

The console shows image builds, their steps and cache verdicts; job results
and runner waits come from GitLab, which it cannot read yet. To add them,
run this script again with --gitlab-token-file FILE (a token with read_api).
For sign-in through GitLab, follow "The console" in the README; the values
are in $STATE/values.yaml, apply changes with
  sudo KUBECONFIG=$KUBECONFIG $HELM upgrade $REL $CHARTS/ci-accelerator --version $VERSION -n $NS -f $STATE/values.yaml --wait

Values: $STATE/values.yaml   Install log: $STATE/install.log
Kubernetes: sudo k3s kubectl get pods -A
Remove everything: sudo $0 --uninstall
EOF
