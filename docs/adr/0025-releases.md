# 0025. Releases as OCI charts on ghcr.io

Date: 2026-10-18

## Context

Installing from a clone needs `make deps`. Users, and GitOps tools in particular, want a versioned, immutable chart they can pin. GitHub's container registry hosts OCI artifacts next to the code at no cost, and Helm 3.8+ and Helm 4 pull charts from OCI registries directly.

## Decision

- A version tag (`v0.1.0`) publishes both charts to `oci://ghcr.io/<owner>/charts`. The tag must match the charts' version and the umbrella's dependency on buildkit; one version and one tag cover the repository for now.
- The release runs the same checks as every push, packages with Helm 4 reproducibly (fixed timestamps), pushes with the workflow's own token, and creates a GitHub Release with the archives, their checksums and the registry digests. Pre-release tags (`-rc.N`) become pre-releases.
- Charts are signed with cosign keyless signing, only while the repository is public: the signing certificate records the repository in Sigstore's public transparency log.

## Consequences

- Users install with `helm install ... oci://ghcr.io/<owner>/charts/ci-accelerator --version <v>`, without cloning; the umbrella package contains its dependencies.
- Packages from a private repository are private; they are made public by hand after the repository is.
- A release can be rehearsed with an `-rc` tag before the real one.
- Anyone can rebuild a release byte for byte from its tag with the same Helm version (4.3.0 for now): `make deps`, then `helm package` with `SOURCE_DATE_EPOCH` set to the tag's commit time. Other Helm versions may order the vendored subcharts differently and produce a different archive (seen with 4.3.1).
