# 0038. Files or node: settled with GitLab's comparison of two commits

Date: 2026-10-23

## Context

A step that copies files from the build context, rebuilt on a node other than the one that ran the image's previous build, is either the change's doing (its files changed) or the build farm's (the cache was on the other node). BuildKit's records keep no fingerprint of the copied files, so the console called it "unclear". Another job of the same pipeline settles it only sometimes. On a test monorepo with three build nodes (8 services, 150 pipelines a day) 196 of 849 builds in a day, 63 % of the rebuilds, stayed unclear, and the console's front page led with "we do not know".

## Decision

- The console asks GitLab which files differ between the commit of the image's previous build and this build's commit (`GET /projects/:id/repository/compare?from&to&straight=true`, the viewer's token or the console's). If one of them is something the first rebuilt step copies, the reason becomes "files changed", listing them. If none is and GitLab's list is complete, it becomes "build farm". The same commit needs no request.
- A changed file matches a COPY source when it is the source or lies under it, also when the source is matched against the file's trailing path parts (the build context may be a folder of the repository), and through globs. This can call a farm rebuild the change's, never the other way round, so a "build farm" verdict can be trusted.
- When `compare_timeout` is set, the answer is over 32 MiB, or GitLab refuses, nothing is concluded ("unresolved: ..." is stored, so the build is not asked about again).
- The verdict is stored on the build with who decided it (`reason_via`). Lists, filters and the daily figures count it at once; the build page names the commits compared.
- When it runs: a build page settles its own build before it renders (3 s budget). Every other page settles the project's builds in the background, at most 25 requests a pass and one pass per 30 s, stopping on 429. Every image of a pipeline shares one comparison.

## Consequences

- On the test project's day, 136 builds became "build farm" (13 of them the same commit), 60 "your change"; the front page now says that most rebuild time went to jobs landing on the wrong node.
- Without a GitLab token or sign-in, builds stay "unclear", as before.
- Files excluded by `.dockerignore` still count as changes: a rebuild can be called the change's when it was not.

## Update 2026-10-28

- GitLab's comparison of two commits never changes, so it is stored
  (`compares`, kept as long as builds). Settling works the verdict out from
  the stored comparison and the current data whenever a build is read
  again (a controller restart reads the daemons' history again), without
  asking GitLab. Stored verdicts are not protected against a re-read: the
  previous build and the first rebuilt step can change, and the verdict
  must follow them.
- The controller settles every project in the background once a minute:
  with the installation's token it asks GitLab (25 requests a project a
  pass); with sign-in, from the stored comparisons alone.
- GitLab failing for a moment (5xx, timeouts) is no longer stored as
  "unresolved": those builds are asked about again later. A 4xx still is.
