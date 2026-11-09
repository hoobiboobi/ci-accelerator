# 0059. An OpenAPI contract for the console API

Date: 2026-11-09

## Context

The console answers `GET /api/v1/builds` with JSON: every build, why it
was rebuilt and its cache-mount hints. The README described it in a
sentence; nothing said which fields there are, which may be missing, or
what may change between releases, and nothing stopped a release from
renaming a field that a script reads.

## Decision

- `docs/api/openapi.yaml` describes the API by hand, in OpenAPI 3.0.3.
  The tools that read it (oasdiff, kin-openapi) label their 3.1 support
  beta, and the API needs nothing from 3.1. One endpoint does not need a
  language that generates the description (TypeSpec).
- Within v1 no field is removed or renamed, no type changes and no
  parameter gets stricter; new fields and values may appear.
- Every object in it refuses properties it does not list.
  `internal/api`'s test validates real responses against it, with every
  field of the contract filled: a field added in Go without the contract,
  or a contract field the test never fills, fails it.
- `make api-check` compares the contract with the one of the newest
  release that has it, with oasdiff. A removed field, a changed type or a
  stricter parameter fails; a new field or value does not
  (`docs/api/oasdiff-levels.txt` sets the two levels where oasdiff's
  defaults differ). The Go job of the checks runs it.

## Consequences

- A script written against one release keeps working with the next ones
  of v1, and the project's CI, not a reader, notices when it would not.
- A breaking change needs `/api/v2`, with v1 kept for a release.
- Removing a value from an enum passes the check: a client that knows the
  value is not broken when the console stops sending it.
