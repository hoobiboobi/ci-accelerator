#!/usr/bin/env bash
# Checks that a pushed ci-accelerator image carries an SPDX SBOM for each
# platform it is built for (scripts/build-image.sh attaches them), as a
# user reads it:
#
#   scripts/check-sbom.sh ghcr.io/<owner>/ci-accelerator@sha256:...
#
# Fails when a platform has none, or one without packages.
set -euo pipefail
ref=${1:?usage: $0 <image@digest>}
PLATFORMS=${PLATFORMS:-linux/amd64 linux/arm64}
out=$(mktemp)
trap 'rm -f "$out"' EXIT
docker buildx imagetools inspect "$ref" --format '{{json .SBOM}}' >"$out"
python3 - "$out" $PLATFORMS <<'PY'
import json, sys
raw = open(sys.argv[1]).read().strip()
sbom = json.loads(raw) if raw and raw != "null" else {}
failed = False
for platform in sys.argv[2:]:
    doc = (sbom.get(platform) or {}).get("SPDX") or {}
    packages = len(doc.get("packages") or [])
    if not doc.get("spdxVersion") or packages == 0:
        print(f"{platform}: no SPDX SBOM", file=sys.stderr)
        failed = True
    else:
        print(f"{platform}: {doc['spdxVersion']}, {packages} packages")
sys.exit(1 if failed else 0)
PY
