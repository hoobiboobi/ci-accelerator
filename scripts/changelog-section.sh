#!/usr/bin/env bash
# Prints the CHANGELOG.md entry of a version (without its heading) and fails
# when there is none or it has no "### Upgrade notes" section with text. A
# pre-release (0.2.0-rc.1) without its own entry uses the one of its version,
# then "Unreleased".
#
#   scripts/changelog-section.sh 0.1.0

set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
version=${1:?usage: $0 <version, e.g. 0.1.0>}
file=${CHANGELOG:-$ROOT/CHANGELOG.md}

# From "## <heading>" (alone or followed by " - <date>") to the next "## ".
entry() {
  awk -v v="$1" '
    $0 == "## " v || index($0, "## " v " ") == 1 { on = 1; next }
    on && /^## / { exit }
    on { print }
  ' "$file"
}
has_text() { [ -n "$(printf '%s' "$1" | tr -d '[:space:]')" ]; }

section=$(entry "$version")
case "$version" in
  *-*)
    has_text "$section" || section=$(entry "${version%%-*}")
    has_text "$section" || section=$(entry Unreleased)
    ;;
esac
has_text "$section" || { echo "CHANGELOG.md has no entry for $version" >&2; exit 1; }

notes=$(printf '%s\n' "$section" | awk '
  /^### Upgrade notes[[:space:]]*$/ { on = 1; next }
  on && /^### / { exit }
  on { print }
')
has_text "$notes" \
  || { echo "the CHANGELOG.md entry for $version has no \"### Upgrade notes\" (write \"None.\" if there are none)" >&2; exit 1; }

printf '%s\n' "$section" | sed -e '/./,$!d'
