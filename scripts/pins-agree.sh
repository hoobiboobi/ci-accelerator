#!/bin/sh
# Pins that name the same thing in two places must agree (make lint):
#
#   - the BuildKit that builds the image (scripts/build-image.sh) is the
#     daemons' (charts/buildkit: appVersion and default digest);
#   - the mirror's checks (ci/showcase/test.Dockerfile) start from the
#     image's Go base (cmd/ci-accelerator/Dockerfile), so their first steps
#     are shared;
#   - the runner fragment (integrations/gitlab-runner) runs the box's runner
#     image and names the chart version the checks render it with
#     (RUNNER_CHART_VERSION in the Makefile).
#
#   scripts/pins-agree.sh [<repository root>]
set -eu
cd "${1:-$(dirname "$0")/..}"
fail=0
differ() { echo "pins-agree: $*" >&2; fail=1; }

app=$(sed -n 's/^appVersion: "\(v[^"]*\)"$/\1/p' charts/buildkit/Chart.yaml)
digest=$(sed -n 's/^  digest: "\(sha256:[0-9a-f]\{64\}\)".*/\1/p' charts/buildkit/values.yaml | sed -n 1p)
built=$(sed -n 's/^BUILDKIT_IMAGE=//p' scripts/build-image.sh)
[ "$built" = "moby/buildkit:$app@$digest" ] ||
  differ "scripts/build-image.sh builds with $built, the daemons run moby/buildkit:$app@$digest (charts/buildkit)"

image=$(grep -m1 '^FROM ' cmd/ci-accelerator/Dockerfile | sed 's/ AS .*//')
showcase=$(grep -m1 '^FROM ' ci/showcase/test.Dockerfile | sed 's/ AS .*//')
[ "$image" = "$showcase" ] ||
  differ "ci/showcase/test.Dockerfile starts '$showcase', cmd/ci-accelerator/Dockerfile '$image': bump them together"

tag() { sed -n 's/^ *tag: \(alpine-v[0-9.]*\)$/\1/p' "$1" | sed -n 1p; }
box=$(tag charts/ci-accelerator/values.yaml)
own=$(tag integrations/gitlab-runner/values.yaml)
[ -n "$box" ] && [ "$box" = "$own" ] ||
  differ "integrations/gitlab-runner/values.yaml runs gitlab-runner ${own:-?}, the box ${box:-?}"
chart=$(sed -n 's/^RUNNER_CHART_VERSION *:= *//p' Makefile)
for f in integrations/gitlab-runner/values.yaml integrations/gitlab-runner/README.md; do
  grep -q "charts.gitlab.io --version $chart \\\\\$" "$f" ||
    differ "$f: its install command does not name the gitlab-runner chart $chart (RUNNER_CHART_VERSION)"
  grep -q "Tested with chart $chart and" "$f" ||
    differ "$f: it does not say 'Tested with chart $chart' (RUNNER_CHART_VERSION)"
done
[ "$fail" = 0 ] && echo "pins-agree: the image's BuildKit and Go base, and the runner fragment, match"
exit "$fail"
