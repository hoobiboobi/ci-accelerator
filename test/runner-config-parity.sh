#!/usr/bin/env bash
# The runner configuration exists twice: integrations/gitlab-runner/values.yaml
# for an existing runner, and the umbrella chart's gitlab-runner block, which
# derives names from its release. Rendered for the release and namespace the
# fragment's defaults assume ("ci-accelerator"), both must produce the same
# config.toml template, comments aside.
#
#   test/runner-config-parity.sh <helm> <gitlab-runner chart archive>

set -euo pipefail
HELM=$1
RUNNER_CHART=$2
ROOT=$(cd "$(dirname "$0")/.." && pwd)

# The config.template.toml value of the rendered ConfigMap, without comments
# and blank lines.
toml() {
  awk '/^  config.template.toml:/ {on = 1; next} on && /^  [a-z]/ {exit} on' \
    | sed -e 's/^    //' -e 's/[[:space:]]*#.*$//' -e '/^[[:space:]]*$/d'
}

fragment=$("$HELM" template ci-accelerator "$RUNNER_CHART" -n ci-accelerator \
  -f "$ROOT/integrations/gitlab-runner/values.yaml" | toml)
umbrella=$("$HELM" template ci-accelerator "$ROOT/charts/ci-accelerator" -n ci-accelerator \
  --api-versions cert-manager.io/v1 \
  --set gitlab-runner.gitlabUrl=https://gitlab.example.com/ --set gitlab-runner.runners.secret=gitlab-runner-token \
  --show-only charts/gitlab-runner/templates/configmap.yaml | toml)

[ -n "$fragment" ] || { echo "no runner config rendered from the fragment" >&2; exit 1; }
if [ "$fragment" != "$umbrella" ]; then
  echo "runner config differs between integrations/gitlab-runner and charts/ci-accelerator:" >&2
  diff <(printf '%s\n' "$fragment") <(printf '%s\n' "$umbrella") >&2 || true
  exit 1
fi
echo "runner config: integrations/gitlab-runner and charts/ci-accelerator agree"
