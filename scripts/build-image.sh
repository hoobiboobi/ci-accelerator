#!/usr/bin/env bash
# Builds the ci-accelerator image (the control plane and the disk probe)
# for amd64 and arm64 the way a release does:
# Go cross-compiles (no emulation), BuildKit is the release the box's
# daemons run (pinned by digest), timestamps come from the commit.
#
#   scripts/build-image.sh <image:tag> push    # a release
#   scripts/build-image.sh <image:tag> check   # build only (CI)
#
# Prints <image>@<digest> after a push. The provenance attestation is
# mode=min: builder and materials, not the whole CI event payload. With SCAN_TAR=<file> set, check
# mode also writes the linux/amd64 image as a docker archive there (the
# same build, from the builder's cache) for a vulnerability scan.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
ref=${1:?usage: $0 <image:tag> push|check}
mode=${2:?usage: $0 <image:tag> push|check}
BUILDKIT_IMAGE=moby/buildkit:v0.33.1@sha256:cec9f139f45e93c5c69c60f8b07cfad9f43f4ef6b6a6cd917527fea5ff2e3dea

builder=ci-accelerator-image-$$
docker buildx create --name "$builder" --driver docker-container --driver-opt "image=$BUILDKIT_IMAGE" >/dev/null
trap 'docker buildx rm "$builder" >/dev/null 2>&1 || true' EXIT
SOURCE_DATE_EPOCH=$(git -C "$ROOT" log -1 --format=%ct)
export SOURCE_DATE_EPOCH
COMMIT=$(git -C "$ROOT" rev-parse --short=12 HEAD)
meta=$(mktemp)
case "$mode" in
  push) output="type=image,name=$ref,push=true,rewrite-timestamp=true" ;;
  check) output="type=image,name=$ref,push=false,rewrite-timestamp=true" ;;
  *) echo "mode must be push or check" >&2; exit 2 ;;
esac
docker buildx build --builder "$builder" --provenance=mode=min -f "$ROOT/cmd/ci-accelerator/Dockerfile" \
  --platform linux/amd64,linux/arm64 \
  --label "org.opencontainers.image.source=https://github.com/${GITHUB_REPOSITORY:-hoobiboobi/ci-accelerator}" \
  --label "org.opencontainers.image.version=${ref##*:}" \
  --build-arg "VERSION=${ref##*:}" --build-arg "COMMIT=$COMMIT" \
  --output "$output" --metadata-file "$meta" "$ROOT" >&2
digest=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["containerimage.digest"])' "$meta")
rm -f "$meta"
if [ "$mode" = push ]; then
  echo "${ref%:*}@$digest"
else
  echo "built ${ref%:*}@$digest (not pushed)" >&2
  if [ -n "${SCAN_TAR:-}" ]; then
    # No attestation here: the docker exporter cannot write a manifest list.
    docker buildx build --builder "$builder" --provenance=false -f "$ROOT/cmd/ci-accelerator/Dockerfile" --platform linux/amd64 \
      --build-arg "VERSION=${ref##*:}" --build-arg "COMMIT=$COMMIT" \
      --output "type=docker,dest=$SCAN_TAR,name=$ref" "$ROOT" >&2
    echo "wrote $SCAN_TAR (linux/amd64)" >&2
  fi
fi
