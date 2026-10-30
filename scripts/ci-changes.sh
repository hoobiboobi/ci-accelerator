#!/bin/sh
# Which CI jobs a change needs, from the paths it changed (one per line on
# stdin). Prints key=value lines for $GITHUB_OUTPUT:
#
#   charts=true|false   lint, unit tests, schema validation, the CI
#                       component's tests, the quickstart, every e2e group
#   go=true|false       go test, the image build and scan, the browser
#                       checks, the e2e groups that run the control plane
#                       (box, upgrade)
#   component=true|false
#                       the GitLab CI component in a real GitLab (CE booted
#                       in Docker: lint at two versions, the e2e group
#                       component); only for a change under ci/gitlab/ (or
#                       no list at all), since each GitLab takes minutes
#   e2e_matrix=[...]    the e2e groups to run with their scenarios, as the
#                       JSON of the e2e workflow's matrix (empty: none)
#
# A change the lists below do not know, an empty list or no list at all
# runs everything: skipping is only for what is certainly untouched.
# Documentation alone (Markdown outside the charts and the CI component,
# docs/, Artifact Hub metadata) runs nothing. A real GitLab
# runs only for the component's own files.
set -eu

charts=false
go=false
component=false
all=false
any=false
while IFS= read -r f; do
  [ -n "$f" ] || continue
  any=true
  case "$f" in ci/gitlab/*) component=true ;; esac
  case "$f" in
    # Generated from values (checked by lint) or checked by the component's
    # tests: before the Markdown rule.
    # The license too: lint compares every chart's copy with it.
    charts/*/README.md | ci/gitlab/* | LICENSE) charts=true ;;
    # The image's own recipe (.dockerignore decides what its build sees):
    # the image job, the e2e and the quickstart build it.
    cmd/ci-accelerator/Dockerfile | scripts/build-image.sh | .dockerignore) all=true ;;
    # .gitlab-ci.yml is the mirror's pipeline at gitlab.com: GitLab is its
    # check.
    docs/* | *.md | artifacthub/* | .github/ISSUE_TEMPLATE/* | .gitignore | .editorconfig | .gitlab-ci.yml) ;;
    # The Go job runs the vulnerability gate, lint shellchecks it.
    scripts/vulncheck.sh) charts=true; go=true ;;
    cmd/* | internal/* | go.mod | go.sum | test/ui/* | .govulncheck-allow | .trivyignore.yaml) go=true ;;
    charts/* | integrations/* | examples/* | scripts/*.sh | test/e2e/* | test/quickstart/* | test/*.sh) charts=true ;;
    *) all=true ;;
  esac
done
if [ "$any" = false ] || [ "$all" = true ]; then
  charts=true
  go=true
fi
[ "$any" = true ] || component=true
echo "charts=$charts"
echo "go=$go"
echo "component=$component"
# The e2e groups: scenario times from a full local run, upgrade 401 s,
# box 350 s, existing 181 s, runner 101 s, the rest about 360 s together;
# component (a real GitLab boots first) on its own.
# The first two run the control plane; test/e2e-matrix.sh checks that
# every scenario is in exactly one group.
cp_groups='{"group":"upgrade","scenarios":"upgrade"},{"group":"box","scenarios":"box"}'
component_group='{"group":"component","scenarios":"component"}'
chart_groups='{"group":"tls-runner","scenarios":"preflight tls certmanager existing runner"},{"group":"daemons","scenarios":"netpol otel cache dedup routing local bump graceful persist sigkill badconfig gc debug probes"}'
groups=""
if [ "$charts" = true ]; then
  groups="$cp_groups,$chart_groups"
elif [ "$go" = true ]; then
  groups=$cp_groups
fi
[ "$component" = false ] || groups="${groups:+$groups,}$component_group"
echo "e2e_matrix=[$groups]"
