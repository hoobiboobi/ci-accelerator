# Security

## Reporting a vulnerability

Use GitHub's private vulnerability reporting for this repository
([Security → Report a vulnerability](https://github.com/hoobiboobi/ci-accelerator/security/advisories/new)).
Do not open a public issue for something exploitable. You get an answer
within a week; a fix or a workaround follows as the next patch release of
the current minor, with the advisory published once it is out. This is a
one-person project: there is no bounty and no embargo coordination beyond
GitHub's advisory workflow.

## Supported versions

The latest 0.x minor receives fixes; older minors do not. Every release
upgrades in place from the previous one (see
[docs/releasing.md](docs/releasing.md)).

## What is in scope

- The Helm charts (`charts/*`), the console (`cmd/`, `internal/`), the
  runner integration and `scripts/quickstart.sh`.
- Published artifacts: the charts and the console image on `ghcr.io`,
  signed with cosign (the release notes show how to verify them).

Out of scope: BuildKit, cert-manager, the GitLab runner and Kubernetes
themselves (report to their projects), and the deliberate limits listed
under *Limitations* in the [README](README.md): a build daemon is one trust
domain, RUN steps share its network identity, and the console without
sign-in shows everything to whoever reaches it.

## What the project does itself

- Every dependency, image, tool and GitHub Action is pinned by version and
  checksum or digest; pins are at least two weeks old when adopted.
- Every pull request runs `govulncheck` on the console's code for both
  release targets and `trivy` on the built image; a weekly run repeats
  both against `main` and the latest published image.
- GitHub's dependency graph, Dependabot alerts and security updates,
  CodeQL and secret push protection are on.
