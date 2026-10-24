# 0041. Security patches skip the two-weeks wait

Date: 2026-10-24

## Context

Every pin in this project is at least two weeks old when it is adopted:
fresh releases regress, and two weeks let the ecosystem find out. The
rule has no exception, so when Go 1.27.3 (crypto/x509, reachable from the
console), Node 24.22.1 and cert-manager v1.21.3 (security releases) came
out in the days before 0.2.0, the release would have waited for them with
a known, reachable vulnerability documented as a grace period.

## Decision

A pin may be younger than two weeks when all of this holds:

1. it is a patch release of the minor line already pinned (1.27.2 to
   1.27.3), not a new minor or major - a minor waits the two weeks even
   when it carries security fixes;
2. it fixes a vulnerability that affects the project: govulncheck, trivy
   or Dependabot report it, or the advisory describes the project's use;
3. the full CI passes on the bump, and the changelog names
   the exception, so a regression can be traced to it.

## Consequences

- A security patch reaches a release within days instead of weeks; grace
  entries in `.govulncheck-allow` and `.trivyignore.yaml` are for the rare
  case where no patch exists yet.
- The first use: 0.2.0 ships Go 1.27.3, Node 24.22.1 and cert-manager
  v1.21.3 four days after their releases.
