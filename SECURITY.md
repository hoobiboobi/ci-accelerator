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

| Version | Fixes |
|---|---|
| 0.3.x | yes |
| 0.2.x and older | no |

The latest 0.x minor receives fixes; older minors do not. A release
upgrades in place from the previous one, with one exception: 0.2.x does
not upgrade to 0.3.0 (the console became the control plane and its objects
have new names), so move to 0.3 by installing again as the
[changelog's upgrade notes](CHANGELOG.md) describe. See also
[docs/releasing.md](docs/releasing.md).

## What is in scope

- The Helm charts (`charts/*`), the `ci-accelerator` binary and its roles
  (the controller, the web, the build router and the disk probe: `cmd/`,
  `internal/`), the runner integration (`integrations/`), the GitLab CI
  component (`ci/gitlab/`) and the scripts (`scripts/quickstart.sh`,
  `scripts/buildkit-certs.sh`).
- Published artifacts: the charts and the ci-accelerator image on `ghcr.io`,
  signed with cosign (the release notes show how to verify them).

What the design relies on, so that a way around it is a vulnerability:
- The daemons accept only clients with a certificate from the
  installation's CA. The build router passes the jobs' TLS through to them
  and holds no key; it reads a job's identity from its pod's address only
  to choose a node, never to grant access.
- In the box, with the router on, the daemons' NetworkPolicy admits only
  the routers and the controller.
- The controller's API answers only the web and the routers: each proves
  who it is with a projected ServiceAccount token (audience
  `ci-accelerator-controller`) that the controller checks with a
  TokenReview. Its certificate is for server authentication only, so it
  cannot be used to reach the daemons.
- The web keeps nothing: sessions, GitLab tokens (encrypted at rest with
  the session key when sign-in is on) and the store live in the controller.
  With sign-in, a person sees only the projects whose jobs GitLab lets them
  read.
- No role logs tokens, cookies, query strings, build arguments or step
  output, and anything shaped like a GitLab token is masked.

Out of scope: BuildKit, cert-manager, the GitLab runner and Kubernetes
themselves (report to their projects), and the deliberate limits listed
under *Limitations* in the [README](README.md): a build daemon is one trust
domain (builds share its cache mounts), RUN steps share its network
identity, any certificate from the installation's CA reaches the daemons,
the disk probe's numbers are readable by builds, and the console without
sign-in shows everything to whoever reaches it.

## What the project does itself

- Every dependency, image, tool and GitHub Action is pinned by version and
  checksum or digest; pins are at least two weeks old when adopted, except
  a security patch of the same minor line, which is adopted at once.
- Every pull request that touches the Go code or the image's build runs
  `govulncheck` on the Go code for both release targets and `trivy` on the
  built image; a weekly run repeats both against `main` and the latest
  published image, so an advisory published after a merge still turns
  something red.
- GitHub's dependency graph, Dependabot alerts and security updates,
  CodeQL and secret push protection are on.
