#!/bin/sh
# The paths changed between a base commit and HEAD, one per line, for
# scripts/ci-changes.sh. Deleted paths are listed, and so are both sides of
# a rename (--no-renames): git's rename detection would list only the new
# path, and a checked file moved into docs/ would run no check of the
# place it left.
#
#   scripts/ci-changed-paths.sh <base commit>
set -eu
git diff --name-only --no-renames "${1:?usage: $0 <base commit>}" HEAD
