# 0002. Helm 3 and 4, schemas, GitOps-friendly

Date: 2026-10-17

## Context

Helm 4 was released in November 2025; Helm 3 gets security fixes until November 2026. Many installations are managed by Argo CD or Flux, which render charts offline.

## Decision

- Charts use `apiVersion: v2` and work on Helm 3 and Helm 4; every check runs on both.
- Every chart has a `values.schema.json`; unknown keys are rejected.
- No hooks the chart depends on, no randomness in templates (no generated passwords or CAs), an existing-Secret option for every credential.
- Checks that need the live cluster use `lookup` and are skipped when rendering offline.

## Consequences

- Values that cannot be generated (the runner token) must be created by the user.
- Some install-time checks do not run under GitOps tools; NOTES and docs say so.
