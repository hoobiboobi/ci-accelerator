#!/bin/sh
# scripts/ci-changes.sh on lists of changed paths: documentation alone runs
# nothing, a Go change runs the control plane's jobs, a chart change the
# charts' and every e2e group but the component's, a change of the GitLab CI
# component a real GitLab too, anything unknown everything (a real GitLab
# only without a list at all); a rename lists the path it left.
set -eu
cd "$(dirname "$0")/.."
fail=0
want() {
  desc=$1
  expect=$2
  shift 2
  got=$(printf '%s\n' "$@" | scripts/ci-changes.sh | tr '\n' ' ' | sed 's/ $//')
  if [ "$got" = "$expect" ]; then
    echo "  ok   $desc"
  else
    echo "  FAIL $desc: got '$got', want '$expect'"
    fail=1
  fi
}
CP='{"group":"upgrade","scenarios":"upgrade"},{"group":"box","scenarios":"box"}'
CHART_GROUPS='{"group":"tls-runner","scenarios":"preflight tls certmanager existing runner"},{"group":"daemons","scenarios":"netpol otel cache dedup routing local bump graceful persist sigkill badconfig gc debug probes"}'
COMP='{"group":"component","scenarios":"component"}'
ALL="$CP,$CHART_GROUPS"
EVERY="charts=true go=true component=false renovate=true e2e_matrix=[$ALL]"
CHARTS="charts=true go=false component=false renovate=false e2e_matrix=[$ALL]"
COMPONENT="charts=true go=false component=true renovate=false e2e_matrix=[$ALL,$COMP]"
GO="charts=false go=true component=false renovate=false e2e_matrix=[$CP]"
NONE='charts=false go=false component=false renovate=false e2e_matrix=[]'
want "docs alone" "$NONE" README.md docs/adr/0048-logs.md CHANGELOG.md artifacthub/buildkit.yml
want "the license (lint compares the charts' copies with it)" "$CHARTS" LICENSE
want "go code" "$GO" internal/web/nodes.go cmd/ci-accelerator/main.go go.sum
want "the browser checks" "$GO" test/ui/ui.mjs
want "a chart" "$CHARTS" charts/buildkit/values.yaml
want "a chart README (generated)" "$CHARTS" charts/buildkit/README.md
want "the CI component: a real GitLab too" "$COMPONENT" ci/gitlab/build.yml
want "the CI component's README (checked by its tests)" "$COMPONENT" ci/gitlab/README.md
want "the component's GitLab tests" "$COMPONENT" ci/gitlab/test/lint.rb
want "the e2e (no GitLab unless the component changed)" "$CHARTS" test/e2e/run.sh
want "the quickstart" "$CHARTS" scripts/quickstart.sh test/quickstart/run.sh
want "go and docs" "$GO" docs/troubleshooting.md internal/store/write.go
want "the Makefile (everything but a real GitLab)" "$EVERY" Makefile
want "a workflow" "$EVERY" .github/workflows/checks.yml
want "the mirror's pipeline (GitLab runs it)" "$NONE" .gitlab-ci.yml ci/showcase/test.Dockerfile ci/showcase/test.Dockerfile.dockerignore
want "the image recipe" "$EVERY" cmd/ci-accelerator/Dockerfile
want "the image build's context" "$EVERY" .dockerignore
want "the vulnerability gate (run by the Go job, shellchecked by lint)" "charts=true go=true component=false renovate=false e2e_matrix=[$ALL]" scripts/vulncheck.sh
want "the release gate (lint runs it on copies of the tree)" "$CHARTS" scripts/check-release-version.sh scripts/changelog-section.sh
want "nothing known: everything, a real GitLab too" "charts=true go=true component=true renovate=true e2e_matrix=[$ALL,$COMP]"
want "go and a chart" "charts=true go=true component=false renovate=false e2e_matrix=[$ALL]" internal/web/nodes.go charts/control-plane/values.yaml
want "Renovate's config (its check alone)" "charts=false go=false component=false renovate=true e2e_matrix=[]" renovate.json5 .github/renovate-global.js test/renovate-deps.txt
want "Renovate's workflow (its check alone, not every job)" "charts=false go=false component=false renovate=true e2e_matrix=[]" .github/workflows/renovate.yml
want "the pins script (shellchecked by lint, run by the Renovate check)" "charts=true go=false component=false renovate=true e2e_matrix=[$ALL]" scripts/update-pins.sh test/renovate-check.sh
want "go and the component" "charts=true go=true component=true renovate=false e2e_matrix=[$ALL,$COMP]" internal/web/nodes.go ci/gitlab/build.yml

# The list a pull request gets (scripts/ci-changed-paths.sh, as the changes
# workflow runs it): moving the component's README into docs/ must still
# run what checks it.
repo=$(mktemp -d)
root=$(pwd)
(
  cd "$repo"
  g() { git -c user.name=test -c user.email=test@example.invalid -c commit.gpgsign=false "$@"; }
  g init -q
  mkdir -p ci/gitlab docs
  echo component >ci/gitlab/README.md
  g add . && g commit -qm base
  g mv ci/gitlab/README.md docs/gitlab-component.md
  g commit -qm move
  "$root/scripts/ci-changed-paths.sh" HEAD~1
) >"$repo.paths"
got=$(scripts/ci-changes.sh <"$repo.paths" | tr '\n' ' ' | sed 's/ $//')
rm -rf "$repo" "$repo.paths"
if [ "$got" = "$COMPONENT" ]; then
  echo "  ok   a file moved out of the component (git's list of a rename)"
else
  echo "  FAIL a file moved out of the component (git's list of a rename): got '$got', want '$COMPONENT'"
  fail=1
fi
exit $fail
