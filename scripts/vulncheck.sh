#!/usr/bin/env bash
# Known vulnerabilities reachable from the console's code, for both release
# targets (linux/amd64, linux/arm64), with govulncheck in source mode. A
# vulnerability counts when a call path reaches an affected symbol; module-
# level matches are reported for information only.
#
# .govulncheck-allow lists ids that may stay for a while (one per line:
# "<GO-id> <expires YYYY-MM-DD> <why>"), e.g. a Go point release that is
# not two weeks old yet. An expired entry fails like a new finding.
#
#   make vulncheck                  # installs the pinned govulncheck first
#   GOVULNCHECK=<binary> scripts/vulncheck.sh
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
bin=${GOVULNCHECK:?set GOVULNCHECK to the govulncheck binary (make vulncheck does)}
allow=$ROOT/.govulncheck-allow
today=$(date -u +%Y-%m-%d)

status=0
json=$(mktemp)
trap 'rm -f "$json"' EXIT
for arch in amd64 arm64; do
  echo "==> $("$bin" -version 2>/dev/null | sed -n 's/^Scanner: //p'), linux/$arch"
  # GOOS/GOARCH select the release target; the binary itself is the host's.
  (cd "$ROOT" && GOOS=linux GOARCH=$arch "$bin" -format json ./...) >"$json"
  python3 - "$allow" "$today" "$json" <<'EOF' || status=1
import json, sys
allow_file, today, report = sys.argv[1], sys.argv[2], sys.argv[3]
allowed = {}
try:
    for line in open(allow_file):
        line = line.split("#", 1)[0].strip()
        if line:
            osv_id, expires, *why = line.split(None, 2)
            allowed[osv_id] = (expires, why[0] if why else "")
except FileNotFoundError:
    pass
dec, buf, i = json.JSONDecoder(), open(report).read(), 0
osvs, reached, seen = {}, {}, set()
while i < len(buf):
    while i < len(buf) and buf[i].isspace():
        i += 1
    if i >= len(buf):
        break
    obj, i = dec.raw_decode(buf, i)
    if "osv" in obj:
        osvs[obj["osv"]["id"]] = obj["osv"]
    if "finding" in obj:
        f = obj["finding"]
        seen.add(f["osv"])
        trace = f.get("trace") or []
        if trace and trace[0].get("function"):
            reached.setdefault(f["osv"], trace[0])
rc = 0
for osv_id in sorted(reached):
    o, t = osvs.get(osv_id, {}), reached[osv_id]
    where = f'{t.get("module", "")} {t.get("package", "")}.{t.get("function", "")}'.strip()
    summary = o.get("summary", "")
    if osv_id in allowed:
        expires, why = allowed[osv_id]
        if today > expires:
            print(f"  FAIL  {osv_id} allowed until {expires}, expired: {summary} ({where})"); rc = 1
        else:
            print(f"  allow {osv_id} until {expires} ({why}): {summary}")
    else:
        print(f"  FAIL  {osv_id}: {summary} - reached at {where}"); rc = 1
for osv_id in sorted(seen - set(reached)):
    print(f"  info  {osv_id}: {osvs.get(osv_id, {}).get('summary', '')} (module match only, no call path)")
for osv_id in sorted(set(allowed) - set(reached)):
    print(f"  note  {osv_id} is in .govulncheck-allow but not reported any more: drop the line")
if rc == 0:
    print(f"  ok    {len(reached)} reachable, {len(seen - set(reached))} module-level only")
sys.exit(rc)
EOF
done
exit $status
