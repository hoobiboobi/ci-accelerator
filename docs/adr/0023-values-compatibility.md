# 0023. Values compatibility from 0.1.0 on

Date: 2026-10-18

## Context

`helm upgrade --reuse-values` keeps the old values tree, so a key added in a later release is missing and a direct reference fails.

## Decision

From 0.1.0 on, every new key is read with defaults in templates, or the release notes require `--reset-then-reuse-values`. The docs recommend `--reset-then-reuse-values`.

## Consequences

New options cost a little more care in templates.
