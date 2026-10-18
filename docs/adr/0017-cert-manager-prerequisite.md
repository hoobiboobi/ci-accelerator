# 0017. cert-manager is a prerequisite, never a subchart

Date: 2026-10-18

## Context

cert-manager is cluster-wide and must be installed exactly once. Installing it as a dependency of our chart would put its CRDs and our Certificates in one release (Helm fails before the CRDs exist), race its webhook, and remove it from the cluster on `helm uninstall`. Generating a CA in templates would produce a new CA on every GitOps render.

## Decision

- Without the `cert-manager.io/v1` API the chart fails with the command that installs cert-manager.
- Without an issuer of the user's own, the chart creates a CA dedicated to the release (self-signed issuer, CA Certificate that keeps its key on renewal, ClusterIssuer) in cert-manager's namespace.

## Consequences

- A bare cluster needs two commands; a one-command installer is planned.
- Offline rendering needs `--api-versions cert-manager.io/v1`.
- The CA key stays in cert-manager's namespace after uninstall.
