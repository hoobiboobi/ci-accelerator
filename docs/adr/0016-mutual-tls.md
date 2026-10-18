# 0016. Mutual TLS is mandatory

Date: 2026-10-18

## Context

RUN steps of builds share the daemon pod's network, so they reach the daemon on localhost. Without client certificates any build could drive its own daemon (verified on kind). buildkitd requires and verifies client certificates when a CA is configured, and reads its certificates only at start.

## Decision

- The chart refuses to render without a TLS source; there is no insecure mode.
- Sources: cert-manager (default) or an existing Secret (a script creates a CA and certificates).
- Leaf certificates last one year and are renewed after two thirds; the DaemonSet carries a Stakater Reloader annotation for a rolling restart when the Secret changes.
- The end-to-end run proves that plaintext clients, clients without or with foreign certificates, and a RUN step on the daemon are refused.

## Consequences

- Any certificate from the CA is accepted: the CA must be dedicated to the installation.
- Without Reloader, daemons must be restarted after a renewal.
