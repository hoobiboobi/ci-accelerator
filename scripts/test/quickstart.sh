#!/usr/bin/env bash
# scripts/quickstart.sh in a fake root (a copy whose system paths point
# into a temporary directory) with stand-ins for curl, k3s, its installer,
# Helm, df and systemctl: first-run slips and recoveries, without root,
# Docker or a cluster. test/quickstart/offline.sh covers the cache path
# and upgrades in a container; test/quickstart/run.sh the real install.
#
#   scripts/test/quickstart.sh
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
REPO=$PWD
pass=0 failn=0
check() { # <name> <command...>
  local name=$1; shift
  if "$@"; then pass=$((pass + 1)); echo "  ok    $name"
  else failn=$((failn + 1)); echo "  FAIL  $name"; sed 's/^/      | /' "$R/out" | tail -n 8; fi
}
ROOTS=()
trap 'rm -rf "${ROOTS[@]}"' EXIT

# newroot: a fresh fake machine in $R.
newroot() {
  R=$(mktemp -d "${TMPDIR:-/tmp}/quickstart-test.XXXXXX")
  ROOTS+=("$R")
  mkdir -p "$R/stub" "$R/usr/local/bin" "$R/run/systemd/system" "$R/proc" "$R/tmp" "$R/log" "$R/home"
  : >"$R/cgroup.controllers"
  echo "MemTotal:       16000000 kB" >"$R/proc/meminfo"
  sed -e "s#/usr/local/bin#$R/usr/local/bin#g" -e "s#/usr/local/lib/ci-accelerator#$R/usr/local/lib/ci-accelerator#g" \
    -e "s#^STATE=/var/lib#STATE=$R/var/lib#" -e "s#/etc/rancher#$R/etc/rancher#g" -e "s#/etc/kubernetes#$R/etc/kubernetes#g" \
    -e "s#/run/systemd/system#$R/run/systemd/system#g" -e "s#/sys/fs/cgroup/cgroup.controllers#$R/cgroup.controllers#g" \
    -e "s#/proc/meminfo#$R/proc/meminfo#g" -e "s#-o root -g root##" -e "s#seq 1 90#seq 1 2#" \
    "$REPO/scripts/quickstart.sh" >"$R/quickstart.sh"
  st() { printf '#!/bin/sh\nR=%s\n%s\n' "$R" "$2" >"$R/stub/$1"; chmod +x "$R/stub/$1"; }
  st id 'echo 0'
  st uname 'case "$1" in -s) echo Linux;; -m) echo x86_64;; esac'
  st nproc 'echo 8'
  st systemctl 'exit 3'
  st df 'echo "Filesystem 1024-blocks Used Available Capacity Mounted on"; echo "stub 209715200 1 ${STUB_FREE_KB:-104857600} 50% /"'
  st hostname 'echo box.example'
  st logname 'echo alice'
  st sha256sum 'cat >/dev/null; exit 0'
  st k3s.bin 'echo "k3s $*" >>$R/log/calls
[ "$1" != --version ] || { echo "k3s version v1.36.5+k3s1 (stub)"; exit 0; }
[ "$1" = kubectl ] && shift
[ ! -e $R/k3s-down ] || { echo "The connection to the server 127.0.0.1:6443 was refused" >&2; exit 1; }
case "$*" in
  "get nodes"*) echo node/stub ;;
  *"create secret"*) for a in "$@"; do case "$a" in --from-file=*) kv=${a#--from-file=}; cp "${kv#*=}" "$R/log/secret.${kv%%=*}";; esac; done ;;
  *"apply -f -"*) cat >/dev/null ;;
esac
exit 0'
  # k3s's installer: fails while $R/install-fails exists; else writes its
  # uninstall script (which removes what it installed).
  st install.sh.src 'echo "install.sh" >>$R/log/calls
[ ! -e $R/install-fails ] || { echo "[ERROR] systemd start failed (stand-in)" >&2; exit 1; }
ln -sf k3s $R/usr/local/bin/kubectl
printf "#!/bin/sh\nrm -rf %s/usr/local/bin/k3s %s/usr/local/bin/kubectl %s/usr/local/bin/k3s-uninstall.sh %s/etc/rancher/k3s\n" $R $R $R $R >$R/usr/local/bin/k3s-uninstall.sh
chmod +x $R/usr/local/bin/k3s-uninstall.sh'
  mkdir -p "$R/stub/helmpkg/linux-amd64"
  printf '#!/bin/sh\nR=%s\n%s\n' "$R" 'echo "helm $*" >>$R/log/calls
case "$1 $2" in
  "pull "*) d=; v=; n=; while [ $# -gt 0 ]; do case "$1" in -d) d=$2;; --version) v=$2;; oci:*) n=${1##*/};; esac; shift; done
     echo chart >"$d/$n-$v.tgz" ;;
  "get notes") printf "%s\\n" "(the notes)" "  kubectl -n ci get ds b" "  (the log prints it: kubectl -n ci logs deploy/c)" ;;
esac
exit 0' >"$R/stub/helmpkg/linux-amd64/helm"
  chmod +x "$R/stub/helmpkg/linux-amd64/helm"
  tar -czf "$R/stub/helm.tgz" -C "$R/stub/helmpkg" linux-amd64
  st curl 'echo "curl $*" >>$R/log/calls
url=; while [ $# -gt 0 ]; do case "$1" in -o) out=$2; shift;; http*) url=$1;; esac; shift; done
case "$url" in */install.sh) cp $R/stub/install.sh.src "$out";; */k3s) cp $R/stub/k3s.bin "$out";; *helm-*) cp $R/stub/helm.tgz "$out";; */SHA256SUMS) v=${url%/SHA256SUMS}; v=${v##*/v}; echo "0000  ci-accelerator-$v.tgz" >"$out";; esac'
  echo glrt-abc >"$R/tok"
}
# qs <args>: one run (output in $R/out, status in $rc); QS_ENV adds variables.
# shellcheck disable=SC2034 # rc is read by the checks, through eval
qs() {
  rc=0
  # shellcheck disable=SC2086 # QS_ENV is a list of words
  env -i PATH="$R/stub:/usr/bin:/bin" HOME="$R/home" TMPDIR="$R/tmp" ${QS_ENV:-} \
    bash "$R/quickstart.sh" "$@" >"$R/out" 2>&1 </dev/null || rc=$?
}
G=(--gitlab-url https://gitlab.example.com/ --yes)
S() { echo "$R/var/lib/ci-accelerator"; }
runs() { grep -c "^install.sh$" "$R/log/calls"; }

echo "quickstart.sh in a fake root"

# k3s's installer fails after the binary is in place: a second run runs it
# again; --uninstall removes what was placed; a later install goes on.
newroot
touch "$R/install-fails"
qs "${G[@]}" --runner-token-file "$R/tok"
check "k3s's installer fails: the run stops" eval '[ "$rc" != 0 ] && [ -e "$R/usr/local/bin/k3s" ]'
qs "${G[@]}" --runner-token-file "$R/tok"
check "a second run runs k3s's installer again" eval '[ "$(runs)" = 2 ]'
qs --uninstall --yes
check "--uninstall removes the k3s the script placed" \
  eval '[ "$rc" = 0 ] && [ ! -e "$R/usr/local/bin/k3s" ] && [ ! -e "$R/etc/rancher/k3s" ] && [ ! -e "$(S)" ]'
rm "$R/install-fails"
qs "${G[@]}" --runner-token-file "$R/tok"
check "after that, an install goes on" eval '[ "$rc" = 0 ] && [ -x "$R/usr/local/bin/k3s-uninstall.sh" ]'
touch "$R/k3s-down"
qs "${G[@]}" --runner-token-file "$R/tok"
check "k3s that does not come up: a hint, not kubectl's error alone" eval '[ "$rc" != 0 ] && grep -q "journalctl -u k3s" "$R/out"'
rm "$R/k3s-down"

# A GitLab token added on a second run.
newroot
qs "${G[@]}" --runner-token-file "$R/tok"
echo glpat-xyz >"$R/gtok"
qs "${G[@]}" --runner-token-file "$R/tok" --gitlab-token-file "$R/gtok"
check "a token added on a second run: the closing note says it is read" \
  eval '[ "$rc" = 0 ] && grep -q "reads GitLab with the token you gave" "$R/out" && ! grep -q "cannot read yet" "$R/out"'
check "... and the printed helm upgrade keeps it" \
  eval 'grep "upgrade ci-accelerator" "$R/out" | grep -q -- "--set control-plane.gitlab.token.existingSecret=gitlab-api-token"'

# --build-dir is checked before anything is installed.
newroot
for d in relcache "/data/my cache" /; do
  (cd "$R/home" && qs "${G[@]}" --runner-token-file "$R/tok" --build-dir "$d")
  check "--build-dir '$d' is refused before anything changes" \
    eval 'grep -q -- "--build-dir" "$R/out" && [ ! -e "$(S)" ] && [ ! -e "$R/home/relcache" ] && [ ! -e "$R/usr/local/bin/k3s" ]'
done

# The closing line's --uninstall command works as printed.
newroot
(cd "$R" && env -i PATH="$R/stub:/usr/bin:/bin" HOME="$R/home" TMPDIR="$R/tmp" \
  bash quickstart.sh "${G[@]}" --runner-token-file "$R/tok" >"$R/out" 2>&1 </dev/null)
check "the closing line names the script by its path, run with bash" \
  eval 'tail -n 1 "$R/out" | grep -qx "Remove everything: sudo bash $(cd "$R" && pwd -P)/quickstart.sh --uninstall"'
check "the chart's notes name kubectl as this machine runs it" \
  eval 'grep -qx "  sudo k3s kubectl -n ci get ds b" "$R/out" && grep -qx "  (the log prints it: sudo k3s kubectl -n ci logs deploy/c)" "$R/out" && ! sed "s/k3s kubectl //g" "$R/out" | grep -q "kubectl "'

# A stop at the checks leaves nothing; --uninstall then talks of no k3s.
newroot
QS_ENV="STUB_FREE_KB=10485760" qs "${G[@]}" --runner-token-file "$R/tok"
check "a stop at the disk check leaves no state directory" eval '[ "$rc" != 0 ] && [ ! -e "$(S)" ]'
mkdir -p "$(S)"
qs --uninstall --yes
check "--uninstall with nothing installed says so, and of no k3s" \
  eval 'grep -q "nothing to uninstall" "$R/out" && ! grep -q "k3s" "$R/out" && [ ! -e "$(S)" ]'

# Options and tokens.
newroot
qs --gitlab-url
check "an option without its value names the option" eval '[ "$rc" = 2 ] && grep -q -- "--gitlab-url needs a value" "$R/out"'
: >"$R/empty"
qs "${G[@]}" --runner-token-file "$R/empty"
check "an empty token file is named" eval '[ "$rc" != 0 ] && grep -q "$R/empty: the file is empty" "$R/out"'
printf '\xef\xbb\xbf glrt-abc \r\n' >"$R/tok2"
qs "${G[@]}" --runner-token-file "$R/tok2"
check "a byte order mark and blanks around the token are dropped" \
  eval '[ "$rc" = 0 ] && [ "$(cat "$R/log/secret.runner-token")" = glrt-abc ] && ! grep -q "does not start with glrt-" "$R/out"'
printf 'glrt-a bc\n' >"$R/tok3"
qs "${G[@]}" --runner-token-file "$R/tok3"
check "a token with a space inside is refused" eval '[ "$rc" != 0 ] && grep -q "spaces or line breaks inside" "$R/out"'
QS_ENV="RUNNER_TOKEN=glrt-fromenv" qs "${G[@]}" --runner-token-file "$R/tok"
check "--runner-token-file wins over RUNNER_TOKEN, and says so" \
  eval '[ "$rc" = 0 ] && [ "$(cat "$R/log/secret.runner-token")" = glrt-abc ] && grep -q "not from RUNNER_TOKEN" "$R/out"'
newroot
qs "${G[@]}" --runner-token-file "$R/tok" --version v0.6.0
check "--version v0.6.0 means release 0.6.0" \
  eval 'grep -q "/v0.6.0/SHA256SUMS" "$R/log/calls" && ! grep -q "/vv0" "$R/log/calls" && grep -q "pull.* --version 0.6.0 " "$R/log/calls"'

# A cache outside the state directory that the install did not record.
newroot
qs "${G[@]}" --runner-token-file "$R/tok" --build-dir "$R/data/cache"
rm -f "$(S)/build-dir-made"
qs --uninstall --yes
check "a cache directory not recorded is left with a neutral note and the command" \
  eval '[ "$rc" = 0 ] && [ -d "$R/data/cache" ] && grep -q "not known to be made by this script" "$R/out" && grep -q "sudo rm -rf $R/data/cache" "$R/out"'

echo "quickstart.sh in a fake root: $pass passed, $failn failed"
[ "$failn" = 0 ]
