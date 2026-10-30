#!/bin/sh
# scripts/check-release-version.sh on copies of this tree, so a change to
# the release gate is run before a tag depends on it: it passes for the
# charts' version and fails on each kind of drift. Between releases the
# disk probe's tag may already name the next release; the copies pin it to
# the charts' version, a case of its own checks that rule.
set -eu
cd "$(dirname "$0")/.."
version=$(awk '/^version:/ {print $2; exit}' charts/buildkit/Chart.yaml)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail=0
fresh() {
  rm -rf "$work/tree" && mkdir "$work/tree"
  cp -R README.md CHANGELOG.md charts ci docs integrations scripts "$work/tree/"
  sed -i.bak "s/default \"v[0-9][^\"]*\" \$img\.tag/default \"v$version\" \$img.tag/" "$work/tree/charts/buildkit/templates/_helpers.tpl"
}
# expect <description> pass|fail [<sed script> <file>]: runs the check on a
# fresh copy, edited by the sed script when given.
expect() {
  desc=$1 want=$2
  fresh
  if [ $# -gt 2 ]; then
    cp "$work/tree/$4" "$work/before"
    sed -i.bak "$3" "$work/tree/$4"
    cmp -s "$work/before" "$work/tree/$4" && { echo "  FAIL $desc: the edit changed nothing in $4"; fail=1; return; }
  fi
  if "$work/tree/scripts/check-release-version.sh" "v$version" >"$work/out" 2>&1; then got=pass; else got=fail; fi
  if [ "$got" = "$want" ]; then
    echo "  ok   $desc"
  else
    echo "  FAIL $desc: the check would $got, want $want"
    sed 's/^/       /' "$work/out"
    fail=1
  fi
}
expect "this tree, v$version" pass
expect "a chart of another version" fail 's/^version: .*/version: 9.9.9/' charts/control-plane/Chart.yaml
expect "the umbrella depends on another buildkit" fail "/^  - name: buildkit\$/,/^    version:/s/^    version: .*/    version: 9.9.9/" charts/ci-accelerator/Chart.yaml
expect "the README installs another version" fail "s/charts\/ci-accelerator --version $version /charts\/ci-accelerator --version 9.9.9 /" README.md
expect "the runner integration installs another version" fail "s/charts\/ci-accelerator --version $version /charts\/ci-accelerator --version 0.0.1 /" integrations/gitlab-runner/README.md
expect "a guide installs another chart version" fail "1s/^/helm install buildkit oci:\/\/ghcr.io\/hoobiboobi\/charts\/buildkit --version 0.0.1 \\\\\\
/" docs/troubleshooting.md
expect "the disk probe runs an unreleased image" fail 's/default "v[0-9][^"]*" \$img\.tag/default "v9.9.9" $img.tag/' charts/buildkit/templates/_helpers.tpl
expect "the changelog entry has no upgrade notes" fail '/^### Upgrade notes/d' CHANGELOG.md
exit $fail
