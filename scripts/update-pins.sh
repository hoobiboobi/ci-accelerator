#!/usr/bin/env bash
# The checksums next to the pinned versions (Makefile, test/e2e/run.sh,
# scripts/quickstart.sh), recomputed from where each project publishes
# them, so a version bump (Renovate's, or by hand) never carries a sum
# typed in. Sources, per dependency:
#
#   helm-unittest   the release's signed .prov file
#   kubeconform     the release's CHECKSUMS file
#   ShellCheck      none published: the sum of the downloaded archive, and
#                   the script says so (trust on first use)
#   gitlab-runner   the chart archive on gitlab-charts.s3.amazonaws.com
#   helm            get.helm.sh's .sha256sum files
#   kind            the release's .sha256sum files
#   kubectl         dl.k8s.io's .sha256 files
#   oras, trivy     the release's checksums.txt
#   go              https://go.dev/dl/?mode=json
#   node            nodejs.org's SHASUMS256.txt
#   k3s             the release's sha256sum-<arch>.txt, and install.sh at the tag
#   cert-manager    the chart layer's digest in the OCI manifest at quay.io
#   victoria-*      the chart archive the Helm repository's index names
#   buildkit        the CI component's integrity (its default image changed),
#                   and the previous default digest kept by the chart (given,
#                   or the one HEAD has when the tree's differs: Renovate
#                   runs this before it commits)
#   component       the CI component's integrity alone
#
#   scripts/update-pins.sh <dep>... | all          rewrite the sums in place
#   scripts/update-pins.sh --check <dep>... | all  only say which differ (exit 1)
#   scripts/update-pins.sh buildkit [sha256:<previous default digest>]
#
# Renovate runs it after each version bump (postUpgradeTasks); the checks
# run --check on the tree. Needs curl and sha256sum (and openssl or node for
# the component's integrity).
set -euo pipefail
cd "$(dirname "$0")/.."

CHECK=0
if [ "${1:-}" = --check ]; then CHECK=1; shift; fi
[ $# -gt 0 ] || { sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

ALL="helm-unittest kubeconform shellcheck gitlab-runner helm kind kubectl oras trivy go node k3s cert-manager victoria-metrics-single victoria-traces-single component"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/update-pins.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
DIFFER=0
say() { echo "update-pins: $*" >&2; }
die() { say "$*"; exit 1; }

fetch() { # <url> <file>
  curl -fsSL --retry 3 --retry-delay 2 --retry-all-errors -o "$2" "$1" || die "download failed: $1"
}
sum_of() { sha256sum "$1" | cut -c1-64; }
# The value of NAME in a Makefile (NAME := v, NAME ?= v) or a shell file (NAME=v).
var() { # <file> <name>
  local v
  v=$(sed -n "s/^$2[[:space:]]*[:?]\{0,1\}=[[:space:]]*//p" "$1" | sed -n 1p)
  [ -n "$v" ] || die "$2 not found in $1"
  printf '%s' "$v"
}
# Sets the 64-hex sum at the end of NAME's line in FILE; with --check only
# compares. The rest of the line (spacing, the assignment) stays.
set_sum() { # <file> <name> <sum>
  local file=$1 name=$2 sum=$3 cur
  printf '%s' "$sum" | grep -Eq '^[0-9a-f]{64}$' || die "$name: not a sha256: '$sum'"
  cur=$(var "$file" "$name")
  if [ "$cur" = "$sum" ]; then return 0; fi
  if [ "$CHECK" = 1 ]; then
    say "$file: $name is $cur, upstream says $sum"
    DIFFER=1
    return 0
  fi
  awk -v n="$name" -v s="$sum" '
    !done && $0 ~ "^" n "[[:space:]]*[:?]?=" { sub(/[0-9a-f]{64}[[:space:]]*$/, s); done = 1 }
    { print }' "$file" >"$WORK/edit" && cat "$WORK/edit" >"$file"
  say "$file: $name -> $sum"
}
# The sum of <name> in a checksum file of "<sum>  <name>" lines.
sum_in() { # <checksums-file> <name>
  local s
  s=$(awk -v n="$2" '$2 == n || $2 == "*" n { print $1; exit }' "$1")
  [ -n "$s" ] || die "no checksum for $2 in $(head -c 200 "$1" | tr '\n' ' ')"
  printf '%s' "$s"
}
# A unit test's expectation that follows a pin: sed <expr> on <file>, or
# with --check say so when it would change anything.
follow() { # <file> <sed expression> <what>
  sed "$2" "$1" >"$WORK/follow"
  if cmp -s "$WORK/follow" "$1"; then return 0; fi
  if [ "$CHECK" = 1 ]; then
    say "$1: does not expect $3 yet"
    DIFFER=1
    return 0
  fi
  cat "$WORK/follow" >"$1"
  say "$1: expects $3"
}
# A version that changed since HEAD (the base Renovate branches from, or
# the last commit before a bump by hand), as "old new"; nothing when it did
# not change or there is no git.
moved() { # <file> <name>
  local old new
  new=$(var "$1" "$2")
  git rev-parse --verify -q HEAD >/dev/null 2>&1 || return 0
  old=$(git show "HEAD:$1" 2>/dev/null | sed -n "s/^$2[[:space:]]*[:?]\{0,1\}=[[:space:]]*//p" | sed -n 1p)
  [ -n "$old" ] && [ "$old" != "$new" ] && printf '%s %s' "$old" "$new"
  return 0
}
re() { printf '%s' "$1" | sed 's/[].[*^$/#\\]/\\&/g'; }
# The text that names a version (READMEs, an error message, its test):
# <before><old><after> becomes <before><new><after> in <file>; with
# --check, <before><current><after> must be there.
retext() { # <file> <before> <after> <old> <new>
  local f=$1 b=$2 a=$3 old=$4 new=$5
  if [ "$CHECK" = 1 ] || [ -z "$old" ]; then
    grep -qF -- "$b$new$a" "$f" || { say "$f: does not name ${b}${new}${a}"; DIFFER=1; }
    return 0
  fi
  follow "$f" "s#$(re "$b$old$a")#$(re "$b$new$a")#g" "$b$new$a"
}
# The versions of one dependency named in several files must agree: Renovate
# bumps them in one branch, a hand edit must too.
same() { # <what> <value> <value>
  [ "$2" = "$3" ] || die "$1 differs: $2 and $3 (bump them together)"
}

dep_helm_unittest() {
  local v; v=$(var Makefile HELM_UNITTEST_VERSION)
  fetch "https://github.com/helm-unittest/helm-unittest/releases/download/v$v/unittest-$v.tgz.prov" "$WORK/prov"
  set_sum Makefile HELM_UNITTEST_SHA256 "$(sed -n "s/^  unittest-$v.tgz: sha256://p" "$WORK/prov" | sed -n 1p)"
}
dep_kubeconform() {
  local v os arch; v=$(var Makefile KUBECONFORM_VERSION)
  fetch "https://github.com/yannh/kubeconform/releases/download/v$v/CHECKSUMS" "$WORK/sums"
  for os in darwin linux; do for arch in arm64 amd64; do
    set_sum Makefile "KUBECONFORM_SHA256_${os}_${arch}" "$(sum_in "$WORK/sums" "kubeconform-$os-$arch.tar.gz")"
  done; done
}
dep_shellcheck() {
  local v os arch sc; v=$(var Makefile SHELLCHECK_VERSION)
  say "shellcheck publishes no checksums: the sums are those of the archives downloaded now"
  for os in darwin linux; do for arch in arm64 amd64; do
    case $arch in arm64) sc=aarch64 ;; *) sc=x86_64 ;; esac
    fetch "https://github.com/koalaman/shellcheck/releases/download/v$v/shellcheck-v$v.$os.$sc.tar.gz" "$WORK/sc.tar.gz"
    set_sum Makefile "SHELLCHECK_SHA256_${os}_${arch}" "$(sum_of "$WORK/sc.tar.gz")"
  done; done
}
dep_gitlab_runner() {
  local v s tag test=charts/ci-accelerator/tests/runner_cache_tiers_test.yaml
  v=$(var Makefile RUNNER_CHART_VERSION)
  same "gitlab-runner chart version" "$v" "$(var test/e2e/run.sh RUNNER_CHART_VERSION)"
  fetch "https://gitlab-charts.s3.amazonaws.com/gitlab-runner-$v.tgz" "$WORK/runner.tgz"
  s=$(sum_of "$WORK/runner.tgz")
  set_sum Makefile RUNNER_CHART_SHA256 "$s"
  set_sum test/e2e/run.sh RUNNER_CHART_SHA256 "$s"
  # The box's unit test expects the runner image the box runs.
  tag=$(sed -n 's/^    tag: \(alpine-v[0-9.]*\)$/\1/p' charts/ci-accelerator/values.yaml | sed -n 1p)
  [ -n "$tag" ] || die "charts/ci-accelerator/values.yaml: no gitlab-runner image tag"
  follow "$test" 's#gitlab-runner:alpine-v[0-9.]*"#gitlab-runner:'"$tag"'"#g' "the runner image $tag"
  # The box's README (helm-docs output, checked by lint) names both.
  v=$(awk '/^  - name: gitlab-runner$/ { f = 1; next } f && /^    version: / { print $2; exit }' charts/ci-accelerator/Chart.yaml)
  [ -n "$v" ] || die "charts/ci-accelerator/Chart.yaml: no gitlab-runner dependency version"
  follow charts/ci-accelerator/README.md 's#^| https://charts.gitlab.io | gitlab-runner | [0-9.]* |$#| https://charts.gitlab.io | gitlab-runner | '"$v"' |#; s#^| gitlab-runner.image.tag | string | `"alpine-v[0-9.]*"` |#| gitlab-runner.image.tag | string | `"'"$tag"'"` |#' "gitlab-runner $v with $tag"
}
# The Makefile keys Helm's sums by version (two Helm lines run in CI): the
# v4 lines are named after the version, so a bump renames them.
dep_helm() {
  local v arch s width old; v=$(var Makefile CI_HELM_VERSION)
  same "Helm version" "$v" "$(var scripts/quickstart.sh HELM_VERSION)"
  case $v in v4.*) ;; *) die "CI_HELM_VERSION $v: the Makefile keeps the v4 line here (Helm 3 is pinned by hand)" ;; esac
  read -r old _ <<<"$(moved scripts/quickstart.sh HELM_VERSION)"
  retext README.md "/usr/local/lib/ci-accelerator/helm-" "/helm upgrade" "$old" "$v"
  retext README.md "- Helm " " and cert-manager" "$old" "$v"
  for arch in amd64 arm64; do
    fetch "https://get.helm.sh/helm-$v-linux-$arch.tar.gz.sha256sum" "$WORK/helm.sum"
    s=$(cut -d' ' -f1 "$WORK/helm.sum")
    set_sum scripts/quickstart.sh "HELM_SHA256_$arch" "$s"
    if grep -q "^HELM_SHA256_${v}_linux_$arch " Makefile; then
      set_sum Makefile "HELM_SHA256_${v}_linux_$arch" "$s"
    elif [ "$CHECK" = 1 ]; then
      say "Makefile: no HELM_SHA256_${v}_linux_$arch (the v4 lines are named after another version)"
      DIFFER=1
    else
      # Rename the v4 line of this arch, keeping the names aligned.
      width=$(awk '/^HELM_SHA256_/ { if (length($1) > w) w = length($1) } END { print w }' Makefile)
      awk -v n="HELM_SHA256_${v}_linux_$arch" -v s="$s" -v arch="$arch" -v w="$width" '
        !done && $0 ~ "^HELM_SHA256_v4[^ ]*_linux_" arch " " { printf "%-" (w > length(n) ? w : length(n)) "s := %s\n", n, s; done = 1; next }
        { print }' Makefile >"$WORK/edit" && cat "$WORK/edit" >Makefile
      say "Makefile: HELM_SHA256_${v}_linux_$arch -> $s"
    fi
  done
}
dep_kind() {
  local v arch; v=$(var Makefile KIND_VERSION)
  for arch in amd64 arm64; do
    fetch "https://github.com/kubernetes-sigs/kind/releases/download/$v/kind-linux-$arch.sha256sum" "$WORK/kind.sum"
    set_sum Makefile "KIND_SHA256_linux_$arch" "$(cut -d' ' -f1 "$WORK/kind.sum")"
  done
}
dep_kubectl() {
  local v arch; v=$(var Makefile KUBECTL_VERSION)
  for arch in amd64 arm64; do
    fetch "https://dl.k8s.io/release/$v/bin/linux/$arch/kubectl.sha256" "$WORK/kubectl.sum"
    set_sum Makefile "KUBECTL_SHA256_linux_$arch" "$(tr -d '\n' <"$WORK/kubectl.sum")"
  done
}
dep_oras() {
  local v p; v=$(var Makefile ORAS_VERSION)
  fetch "https://github.com/oras-project/oras/releases/download/v$v/oras_${v}_checksums.txt" "$WORK/sums"
  for p in linux_amd64 linux_arm64 darwin_arm64; do
    set_sum Makefile "ORAS_SHA256_$p" "$(sum_in "$WORK/sums" "oras_${v}_$p.tar.gz")"
  done
}
dep_trivy() {
  local v; v=$(var Makefile TRIVY_VERSION)
  fetch "https://github.com/aquasecurity/trivy/releases/download/v$v/trivy_${v}_checksums.txt" "$WORK/sums"
  set_sum Makefile TRIVY_SHA256_linux_amd64 "$(sum_in "$WORK/sums" "trivy_${v}_Linux-64bit.tar.gz")"
  set_sum Makefile TRIVY_SHA256_linux_arm64 "$(sum_in "$WORK/sums" "trivy_${v}_Linux-ARM64.tar.gz")"
  set_sum Makefile TRIVY_SHA256_darwin_arm64 "$(sum_in "$WORK/sums" "trivy_${v}_macOS-ARM64.tar.gz")"
}
dep_go() {
  local v arch f s; v=$(var Makefile GO_VERSION)
  fetch "https://go.dev/dl/?mode=json&include=all" "$WORK/go.json"
  for arch in amd64 arm64; do
    f="go$v.linux-$arch.tar.gz"
    s=$(tr -d '\n ' <"$WORK/go.json" | grep -oE "\"filename\":\"$f\"[^}]*\"sha256\":\"[0-9a-f]{64}\"" | sed -n '1s/.*"sha256":"\([0-9a-f]\{64\}\)"$/\1/p' || true)
    [ -n "$s" ] || die "go.dev lists no $f"
    set_sum Makefile "GO_SHA256_linux_$arch" "$s"
  done
}
dep_node() {
  local v; v=$(var Makefile NODE_VERSION)
  fetch "https://nodejs.org/dist/v$v/SHASUMS256.txt" "$WORK/sums"
  set_sum Makefile NODE_SHA256_linux_amd64 "$(sum_in "$WORK/sums" "node-v$v-linux-x64.tar.xz")"
  set_sum Makefile NODE_SHA256_linux_arm64 "$(sum_in "$WORK/sums" "node-v$v-linux-arm64.tar.xz")"
  set_sum Makefile NODE_SHA256_darwin_arm64 "$(sum_in "$WORK/sums" "node-v$v-darwin-arm64.tar.xz")"
  set_sum Makefile NODE_SHA256_darwin_amd64 "$(sum_in "$WORK/sums" "node-v$v-darwin-x64.tar.xz")"
}
dep_k3s() {
  local v tag old; v=$(var scripts/quickstart.sh K3S_VERSION)
  tag=${v//+/%2B}
  read -r old _ <<<"$(moved scripts/quickstart.sh K3S_VERSION)"
  retext README.md "- k3s " ", with the machine" "$old" "$v"
  fetch "https://github.com/k3s-io/k3s/releases/download/$tag/sha256sum-amd64.txt" "$WORK/sums"
  set_sum scripts/quickstart.sh K3S_SHA256_amd64 "$(sum_in "$WORK/sums" k3s)"
  fetch "https://github.com/k3s-io/k3s/releases/download/$tag/sha256sum-arm64.txt" "$WORK/sums"
  set_sum scripts/quickstart.sh K3S_SHA256_arm64 "$(sum_in "$WORK/sums" k3s-arm64)"
  fetch "https://raw.githubusercontent.com/k3s-io/k3s/$tag/install.sh" "$WORK/install.sh"
  set_sum scripts/quickstart.sh K3S_INSTALL_SHA256 "$(sum_of "$WORK/install.sh")"
}
# The chart's archive is the one layer of its OCI manifest: its digest is
# the archive's sha256, no pull needed. (The images' digests are updated
# by Renovate itself from the tags written next to them.)
dep_cert_manager() {
  local v tok s old f; v=$(var test/e2e/run.sh CERT_MANAGER_VERSION)
  same "cert-manager version" "$v" "$(var scripts/quickstart.sh CERT_MANAGER_VERSION)"
  # The install command the chart prints, its test, the README and the
  # GitOps guide; the e2e's values name it in a comment.
  read -r old _ <<<"$(moved test/e2e/run.sh CERT_MANAGER_VERSION)"
  for f in charts/buildkit/templates/_validate.tpl charts/buildkit/tests/validate_test.yaml README.md; do
    retext "$f" "cert-manager --version " "" "$old" "$v"
  done
  retext docs/gitops.md "--version " " --namespace cert-manager" "$old" "$v"
  retext README.md "- Helm $(var scripts/quickstart.sh HELM_VERSION) and cert-manager " ";" "$old" "$v"
  retext test/e2e/cert-manager-values.yaml "pinned to the " " index digests" "$old" "$v"
  tok=$(curl -fsSL "https://quay.io/v2/auth?service=quay.io&scope=repository:jetstack/charts/cert-manager:pull" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
  [ -n "$tok" ] || die "no pull token from quay.io"
  curl -fsSL -H "Authorization: Bearer $tok" -H "Accept: application/vnd.oci.image.manifest.v1+json" \
    "https://quay.io/v2/jetstack/charts/cert-manager/manifests/$v" >"$WORK/manifest" || die "no manifest for cert-manager $v at quay.io"
  s=$(tr -d '\n ' <"$WORK/manifest" | grep -oE '"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar\+gzip","digest":"sha256:[0-9a-f]{64}"' | grep -oE '[0-9a-f]{64}' || true)
  [ -n "$s" ] || die "no chart layer in the cert-manager $v manifest"
  set_sum test/e2e/run.sh CERT_MANAGER_SHA256 "$s"
  set_sum scripts/quickstart.sh CERT_MANAGER_SHA256 "$s"
}
# A chart of the VictoriaMetrics Helm repository: the archive the index
# names for that version.
dep_victoria() { # <chart> <version var> <sum var> <values file>
  local v url old; v=$(var test/e2e/run.sh "$2")
  read -r old _ <<<"$(moved test/e2e/run.sh "$2")"
  retext "$4" "(chart " ", checksum in" "$old" "$v"
  [ -f "$WORK/victoria.yaml" ] || fetch "https://victoriametrics.github.io/helm-charts/index.yaml" "$WORK/victoria.yaml"
  url=$(awk -v c="$1" -v v="$v" '
    /^  [a-z0-9-]+:$/ { chart = substr($1, 1, length($1) - 1); next }
    chart == c && /^  - / { ver = ""; u = "" }
    chart == c && /^    version: / { ver = $2 }
    chart == c && /^    - https?:\/\// { u = $2 }
    chart == c && ver == v && u != "" { print u; exit }' "$WORK/victoria.yaml")
  [ -n "$url" ] || die "the VictoriaMetrics index lists no $1 $v"
  fetch "$url" "$WORK/$1.tgz"
  set_sum test/e2e/run.sh "$3" "$(sum_of "$WORK/$1.tgz")"
}
# The CI component's integrity, as include:remote checks it (the sha256 of
# the file, base64): in the box's notes.
dep_component() {
  local hex b64 cur
  hex=$(sum_of ci/gitlab/build.yml)
  if command -v openssl >/dev/null 2>&1; then
    b64=$(printf '%s' "$hex" | xxd -r -p 2>/dev/null | openssl base64 -A) || b64=$(openssl dgst -sha256 -binary ci/gitlab/build.yml | openssl base64 -A)
  else
    b64=$(node -e 'process.stdout.write(Buffer.from(process.argv[1], "hex").toString("base64"))' "$hex")
  fi
  cur=$(sed -n '/define "ci-accelerator.componentIntegrity"/{n;p;}' charts/ci-accelerator/templates/_helpers.tpl)
  if [ "$cur" = "sha256-$b64" ]; then return 0; fi
  if [ "$CHECK" = 1 ]; then
    say "charts/ci-accelerator/templates/_helpers.tpl: the component's integrity is $cur, the file's is sha256-$b64"
    DIFFER=1
    return 0
  fi
  awk -v s="sha256-$b64" '
    hit { print s; hit = 0; next }
    /define "ci-accelerator.componentIntegrity"/ { hit = 1 }
    { print }' charts/ci-accelerator/templates/_helpers.tpl >"$WORK/edit" && cat "$WORK/edit" >charts/ci-accelerator/templates/_helpers.tpl
  say "charts/ci-accelerator/templates/_helpers.tpl: component integrity -> sha256-$b64"
}
# After a BuildKit bump: the component's integrity (its default image names
# the daemons' BuildKit), and the previous default digest joins the chart's
# list of earlier defaults (helm upgrade --reuse-values keeps the old
# default digest in the release's values: under the new chart's tag it
# would run the old BuildKit by digest).
dep_buildkit() { # [previous default digest]
  local prev=${1:-} helpers=charts/buildkit/templates/_helpers.tpl values=charts/buildkit/values.yaml was now tag oldtag t
  dep_component
  if [ -z "$prev" ] && git rev-parse --verify -q HEAD >/dev/null 2>&1; then
    was=$(git show "HEAD:$values" 2>/dev/null | sed -n 's/^  digest: "\(sha256:[0-9a-f]\{64\}\)".*/\1/p' | sed -n 1p)
    now=$(sed -n 's/^  digest: "\(sha256:[0-9a-f]\{64\}\)".*/\1/p' "$values" | sed -n 1p)
    [ "$was" = "$now" ] || prev=$was
  fi
  # The helper names the chart's default digest itself (what an earlier
  # default is mapped to): it follows values.yaml.
  now=$(sed -n 's/^  digest: "\(sha256:[0-9a-f]\{64\}\)".*/\1/p' "$values" | sed -n 1p)
  [ -n "$now" ] || die "$values: no default digest"
  if ! grep -q "{{- \$digest = \"$now\" }}" "$helpers"; then
    if [ "$CHECK" = 1 ]; then
      say "$helpers: the default digest it maps earlier defaults to is not $now (values.yaml's)"
      DIFFER=1
    else
      awk -v d="$now" '
        /\{\{- \$digest = "sha256:[0-9a-f]+" \}\}/ { sub(/"sha256:[0-9a-f]+"/, "\"" d "\"") }
        { print }' "$helpers" >"$WORK/edit" && cat "$WORK/edit" >"$helpers"
      grep -q "{{- \$digest = \"$now\" }}" "$helpers" || die "$helpers: the default digest line was not found"
      say "$helpers: default digest -> $now"
    fi
  fi
  # The render-time check that refuses another tag with the default digest
  # names the default digest too.
  if ! grep -q "(eq (toString \$v.image.digest) \"$now\")" charts/buildkit/templates/_validate.tpl; then
    follow charts/buildkit/templates/_validate.tpl 's#(eq (toString $v.image.digest) "sha256:[0-9a-f]\{64\}")#(eq (toString $v.image.digest) "'"$now"'")#' "the default digest $now"
  fi
  # The chart's unit tests expect the default image (tag and digest) and
  # its version label: they follow the bump (tests of an earlier BuildKit
  # with its own tag keep theirs).
  tag=$(sed -n 's/^appVersion: "\(v[^"]*\)"$/\1/p' charts/buildkit/Chart.yaml)
  [ -n "$tag" ] || die "charts/buildkit/Chart.yaml: no appVersion"
  if [ -n "$prev" ] && git rev-parse --verify -q HEAD >/dev/null 2>&1; then
    oldtag=$(git show HEAD:charts/buildkit/Chart.yaml | sed -n 's/^appVersion: "\(v[^"]*\)"$/\1/p')
    for t in charts/buildkit/tests/*.yaml; do
      follow "$t" "s#moby/buildkit:$oldtag@$prev#moby/buildkit:$tag@$now#g; s#still the digest of $oldtag'#still the digest of $tag'#g" "moby/buildkit:$tag@$now"
    done
    follow charts/buildkit/tests/daemonset_test.yaml '/app.kubernetes.io\/version"\]$/{n;s#value: '"$oldtag"'$#value: '"$tag"'#;}' "the version label $tag"
    # The chart's README (helm-docs output, checked by lint): its
    # AppVersion badge, the version its description names, the default digest.
    follow charts/buildkit/README.md "s#AppVersion: $oldtag](https://img.shields.io/badge/AppVersion-$oldtag-#AppVersion: $tag](https://img.shields.io/badge/AppVersion-$tag-#; s#It runs BuildKit $oldtag (#It runs BuildKit $tag (#; s#\"$prev\"#\"$now\"#" "BuildKit $tag"
  fi
  [ -n "$prev" ] || return 0
  printf '%s' "$prev" | grep -Eq '^sha256:[0-9a-f]{64}$' || die "buildkit: not a digest: '$prev'"
  if grep -Eq "has \\\$digest \(list [^)]*\"$prev\"" "$helpers"; then return 0; fi
  if [ "$CHECK" = 1 ]; then
    say "$helpers: the earlier default digest $prev is not in the list"
    DIFFER=1
    return 0
  fi
  awk -v d="$prev" '
    !done && /has \$digest \(list / { sub(/\(list /, "(list \"" d "\" "); done = 1 }
    { print }' "$helpers" >"$WORK/edit" && cat "$WORK/edit" >"$helpers"
  grep -Eq "has \\\$digest \(list [^)]*\"$prev\"" "$helpers" || die "$helpers: the earlier-defaults list was not found"
  say "$helpers: earlier default digest $prev added"
}

while [ $# -gt 0 ]; do
  case $1 in
    all) set -- $ALL "${@:2}"; continue ;;
    helm-unittest) dep_helm_unittest ;;
    kubeconform) dep_kubeconform ;;
    shellcheck) dep_shellcheck ;;
    gitlab-runner) dep_gitlab_runner ;;
    helm) dep_helm ;;
    kind) dep_kind ;;
    kubectl) dep_kubectl ;;
    oras) dep_oras ;;
    trivy) dep_trivy ;;
    go) dep_go ;;
    node) dep_node ;;
    k3s) dep_k3s ;;
    cert-manager) dep_cert_manager ;;
    victoria-metrics-single) dep_victoria victoria-metrics-single VM_CHART_VERSION VM_CHART_SHA256 test/e2e/victoria-metrics-values.yaml ;;
    victoria-traces-single) dep_victoria victoria-traces-single VT_CHART_VERSION VT_CHART_SHA256 test/e2e/victoria-traces-values.yaml ;;
    component) dep_component ;;
    buildkit)
      case ${2:-} in sha256:*) dep_buildkit "$2"; shift ;; *) dep_buildkit ;; esac ;;
    *) die "unknown dependency: $1 (one of: $ALL buildkit)" ;;
  esac
  shift
done
if [ "$DIFFER" = 1 ]; then
  say "the sums above differ from upstream's: run scripts/update-pins.sh <dep> (no --check)"
  exit 1
fi
[ "$CHECK" = 0 ] || say "every sum checked matches upstream's"
