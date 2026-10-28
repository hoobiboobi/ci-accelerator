#!/bin/sh
# scripts/ci-changes.sh on lists of changed paths: documentation alone runs
# nothing, a Go change runs the control plane's jobs, a chart change the
# charts' and every e2e group, anything unknown everything.
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
ALL="$CP"',{"group":"tls-runner","scenarios":"preflight tls certmanager existing runner"},{"group":"daemons","scenarios":"netpol otel cache dedup routing local bump graceful persist sigkill badconfig gc debug probes"}'
EVERY="charts=true go=true e2e_matrix=[$ALL]"
CHARTS="charts=true go=false e2e_matrix=[$ALL]"
GO="charts=false go=true e2e_matrix=[$CP]"
NONE='charts=false go=false e2e_matrix=[]'
want "docs alone" "$NONE" README.md docs/adr/0048-logs.md CHANGELOG.md LICENSE artifacthub/buildkit.yml
want "go code" "$GO" internal/web/nodes.go cmd/ci-accelerator/main.go go.sum
want "the browser checks" "$GO" test/ui/ui.mjs
want "a chart" "$CHARTS" charts/buildkit/values.yaml
want "a chart README (generated)" "$CHARTS" charts/buildkit/README.md
want "the CI component's README (checked by its tests)" "$CHARTS" ci/gitlab/README.md
want "the e2e" "$CHARTS" test/e2e/run.sh
want "the quickstart" "$CHARTS" scripts/quickstart.sh test/quickstart/run.sh
want "go and docs" "$GO" docs/troubleshooting.md internal/store/write.go
want "the Makefile" "$EVERY" Makefile
want "a workflow" "$EVERY" .github/workflows/checks.yml
want "the image recipe" "$EVERY" cmd/ci-accelerator/Dockerfile
want "nothing known" "$EVERY"
want "go and a chart" "$EVERY" internal/web/nodes.go charts/control-plane/values.yaml
exit $fail
