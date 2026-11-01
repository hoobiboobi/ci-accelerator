# 0056. "Build farm" only when it is proven

Date: 2026-11-01

## Context

The console's reason for a rebuild decides whether a team looks at its
code or at the build farm. An audit of 0.3.10 found common CI patterns in
which the console said "build farm" without proof:

- [ADR-0038](0038-files-or-node-from-gitlab.md) read "GitLab shows no
  change in the files the step copies" (or "the previous build built the
  same commit") as "the files did not change". A build context also holds
  what is not in the repository: CI artifacts copied into the image, files
  generated in the job, a `.dockerignore` edit.
- A witness from the same pipeline (the same step found in the cache on
  another node) proved nothing when the two builds read different context
  folders: one Dockerfile used for several services, each with its own
  folder as the context, gives their steps the same identities.
- A FilesChanged that the pipeline settling made itself served as the
  witness for the next settling, so two builds settled each other.
- A build with `--no-cache`, and a step behind a download without a
  checksum (`ADD https://...`, a git branch), read as the farm's.

## Decision

- "Build farm" stays for what BuildKit's records prove: a step that reads
  no files and did not change, and a witness of the same pipeline that
  read the same context folder (the CI component passes it as
  `--opt ci-accelerator.io/context=<folder>`, which the Dockerfile frontend
  ignores; without it, only a build of the same image counts).
- GitLab's comparison of the commits proves a change ("your change",
  naming the files) but not its absence: such a rebuild stays unclear, and
  the console says what the comparison showed and what it cannot see.
  `control-plane.reasons.trustGitComparison=true` restores the old
  reading for installations whose build contexts hold repository files
  alone. Switching it recounts the stored builds from the stored
  comparisons; GitLab is not asked again.
- `--no-cache` is the job's choice (reason `no-cache`, counted as the
  build's own change). A step behind an unpinned download is unclear
  (`remote-source`), with the sources named without credentials.
- Settling never takes a FilesChanged it made as a witness.
- The lists of change, farm and unclear reasons are defined once in Go
  and every SQL query derives from them.

## Consequences

- More rebuilds read "unclear" until the console can see the context: on
  the test monorepo's day, 136 of 196 rebuilds that 0.3.10 called the
  farm's. Insights says how many had no change in the repository and
  points to the switch.
- A context fingerprint recorded by the CI component (a hash of the files
  sent) would settle these rebuilds both ways; it needs a measurement of
  its cost on a large repository first.
