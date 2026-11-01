#!/bin/sh
# The third-party images the charts in a tree (default: this one) run by
# default, one reference per line: the daemons' BuildKit (by digest) and
# the box's gitlab-runner. make third-party-scan scans them.
#
#   scripts/third-party-images.sh [<tree>]
set -eu
root=${1:-$(cd "$(dirname "$0")/.." && pwd)}
bk_tag=$(awk '/^appVersion:/ {gsub(/"/, "", $2); print $2; exit}' "$root/charts/buildkit/Chart.yaml")
bk_digest=$(sed -n 's/^  digest: "\(sha256:[0-9a-f]\{64\}\)".*/\1/p' "$root/charts/buildkit/values.yaml" | head -n 1)
runner_tag=$(awk '/depName=registry.gitlab.com\/gitlab-org\/gitlab-runner/ {getline; print $2; exit}' "$root/charts/ci-accelerator/values.yaml")
[ -n "$bk_tag" ] && [ -n "$bk_digest" ] || { echo "no BuildKit image in $root/charts/buildkit" >&2; exit 1; }
[ -n "$runner_tag" ] || { echo "no gitlab-runner image tag in $root/charts/ci-accelerator/values.yaml" >&2; exit 1; }
echo "moby/buildkit:$bk_tag@$bk_digest"
echo "registry.gitlab.com/gitlab-org/gitlab-runner:$runner_tag"
