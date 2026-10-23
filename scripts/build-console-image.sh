#!/usr/bin/env bash
# Builds the console's image for amd64 and arm64 the way a release does:
# Go cross-compiles (no emulation), BuildKit is the release the box's
# daemons run (pinned by digest), timestamps come from the commit.
#
#   scripts/build-console-image.sh <image:tag> push    # a release
#   scripts/build-console-image.sh <image:tag> check   # build only (CI)
#
# Prints <image>@<digest> after a push.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
ref=${1:?usage: $0 <image:tag> push|check}
mode=${2:?usage: $0 <image:tag> push|check}
BUILDKIT_IMAGE=moby/buildkit:v0.33.1@sha256:cec9f139f45e93c5c69c60f8b07cfad9f43f4ef6b6a6cd917527fea5ff2e3dea

builder=console-image-$$
docker buildx create --name "$builder" --driver docker-container --driver-opt "image=$BUILDKIT_IMAGE" >/dev/null
trap 'docker buildx rm "$builder" >/dev/null 2>&1 || true' EXIT
SOURCE_DATE_EPOCH=$(git -C "$ROOT" log -1 --format=%ct)
export SOURCE_DATE_EPOCH
meta=$(mktemp)
case "$mode" in
  push) output="type=image,name=$ref,push=true,rewrite-timestamp=true" ;;
  check) output="type=image,name=$ref,push=false,rewrite-timestamp=true" ;;
  *) echo "mode must be push or check" >&2; exit 2 ;;
esac
docker buildx build --builder "$builder" -f "$ROOT/cmd/console/Dockerfile" \
  --platform linux/amd64,linux/arm64 \
  --label "org.opencontainers.image.source=https://github.com/${GITHUB_REPOSITORY:-hoobiboobi/ci-accelerator}" \
  --label "org.opencontainers.image.version=${ref##*:}" \
  --output "$output" --metadata-file "$meta" "$ROOT" >&2
digest=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["containerimage.digest"])' "$meta")
rm -f "$meta"
if [ "$mode" = push ]; then
  echo "${ref%:*}@$digest"
else
  echo "built ${ref%:*}@$digest (not pushed)" >&2
fi
