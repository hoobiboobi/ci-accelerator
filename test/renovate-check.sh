#!/usr/bin/env bash
# Renovate's view of this tree, from its own image (docker, no token): the
# two configs validate, and the pins it finds are the ones
# test/renovate-deps.txt lists (manager, file, dependency, digest or not;
# no versions: they change with every bump). A pin that lost its
# annotation, or a new one without a line here, fails the check.
#
#   test/renovate-check.sh <renovate image>            check
#   test/renovate-check.sh <renovate image> --write    rewrite the list
#                                                      (after looking at
#                                                      the difference)
set -euo pipefail
cd "$(dirname "$0")/.."
IMAGE=${1:?the Renovate image (make renovate-check passes the pinned one)}
WRITE=${2:-}
LIST=test/renovate-deps.txt
WORK=$(mktemp -d "${TMPDIR:-/tmp}/renovate-check.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
run() { docker run --rm -v "$PWD":/usr/src/app -w /usr/src/app "$@"; }

echo "==> renovate-config-validator"
run --entrypoint renovate-config-validator "$IMAGE" --strict renovate.json5 >"$WORK/validate.log" 2>&1 \
  || { cat "$WORK/validate.log"; exit 1; }
run --entrypoint renovate-config-validator "$IMAGE" --strict .github/renovate-global.js >"$WORK/validate.log" 2>&1 \
  || { cat "$WORK/validate.log"; exit 1; }

echo "==> renovate --platform=local --dry-run=extract"
run -e LOG_LEVEL=debug -e LOG_FORMAT=json "$IMAGE" --platform=local --dry-run=extract >"$WORK/extract.jsonl" 2>&1 \
  || { tail -n 30 "$WORK/extract.jsonl"; exit 1; }
# The image's node reads its log: the record carrying packageFiles is the
# extraction. Go modules are left out (go.mod is their own list); for
# GitHub Actions the action's name is enough.
docker run --rm -i --entrypoint node "$IMAGE" -e '
let s = ""; process.stdin.on("data", d => { s += d; }).on("end", () => {
  let pf = null;
  for (const line of s.split("\n")) { let j; try { j = JSON.parse(line); } catch { continue; } if (j.packageFiles) pf = j.packageFiles; }
  if (!pf) { console.error("no packageFiles in the log"); process.exit(1); }
  const out = new Set();
  for (const [mgr, files] of Object.entries(pf)) {
    if (mgr === "gomod") continue;
    for (const f of files) for (const d of f.deps || []) {
      // Without a token github.com lookups are skipped, not the pins: keep those.
      if (d.skipReason && !["disabled", "github-token-required"].includes(d.skipReason)) continue;
      out.add([mgr, f.packageFile, d.depName || d.packageName, d.currentDigest ? "digest" : "-"].join("\t"));
    }
  }
  console.log([...out].sort().join("\n"));
});' <"$WORK/extract.jsonl" >"$WORK/got"

if [ "$WRITE" = --write ]; then
  cp "$WORK/got" "$LIST"
  echo "$LIST written: $(wc -l <"$LIST" | tr -d ' ') pins"
  exit 0
fi
if diff -u "$LIST" "$WORK/got" >"$WORK/diff"; then
  echo "every pin found: $(wc -l <"$WORK/got" | tr -d ' ') (as $LIST lists them)"
else
  cat "$WORK/diff"
  echo "Renovate sees other pins than $LIST lists: a pin lost its annotation (-), or a new one has no line yet (+)." >&2
  echo "After looking: test/renovate-check.sh $IMAGE --write" >&2
  exit 1
fi
