#!/usr/bin/env bash
# Checks that a pushed ci-accelerator image carries an SPDX SBOM for each
# platform it is built for (scripts/build-image.sh attaches them), as a
# user reads it:
#
#   scripts/check-sbom.sh ghcr.io/<owner>/ci-accelerator@sha256:...
#
# Fails when a platform has none, or one without the binary's Go modules
# (the Go standard library and the modules every role links: a build that
# lost its build info would still list the base image's packages).
# SBOM_JSON=<file> checks a saved `imagetools inspect --format '{{json .SBOM}}'`
# instead of the registry.
set -euo pipefail
ref=${1:?usage: $0 <image@digest>}
PLATFORMS=${PLATFORMS:-linux/amd64 linux/arm64}
out=$(mktemp)
trap 'rm -f "$out"' EXIT
if [ -n "${SBOM_JSON:-}" ]; then cp "$SBOM_JSON" "$out"; else
  docker buildx imagetools inspect "$ref" --format '{{json .SBOM}}' >"$out"
fi
python3 - "$out" $PLATFORMS <<'PY'
import json, sys
raw = open(sys.argv[1]).read().strip()
sbom = json.loads(raw) if raw and raw != "null" else {}
# The Go modules the binary must list (pkg:golang purls): the standard
# library and modules each role links.
WANT = ["stdlib", "github.com/moby/buildkit", "modernc.org/sqlite"]
failed = False
for platform in sys.argv[2:]:
    doc = (sbom.get(platform) or {}).get("SPDX") or {}
    packages = doc.get("packages") or []
    golang = set()
    for p in packages:
        for r in p.get("externalRefs") or []:
            loc = r.get("referenceLocator") or ""
            if r.get("referenceType") == "purl" and loc.startswith("pkg:golang/"):
                golang.add(loc[len("pkg:golang/"):].split("@")[0].split("?")[0])
    missing = [m for m in WANT if m not in golang]
    if not doc.get("spdxVersion") or not packages:
        print(f"{platform}: no SPDX SBOM", file=sys.stderr)
        failed = True
    elif missing:
        print(f"{platform}: the SBOM lists no Go module {', '.join(missing)} ({len(golang)} Go modules)", file=sys.stderr)
        failed = True
    else:
        print(f"{platform}: {doc['spdxVersion']}, {len(packages)} packages, {len(golang)} of them Go modules")
sys.exit(1 if failed else 0)
PY
