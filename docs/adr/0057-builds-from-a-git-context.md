# 0057. Builds from a git context

Date: 2026-11-02

## Context

With a local context every image build job clones the repository and
sends the build context to the daemon. For repositories with a large
history (a few GB of `.git`) the clone dominates the job: on a test
repository with kubernetes' history (1.5 GB, a 373 MB tree), 19-27 s
of checkout and a 283 MB context upload per job, before BuildKit does
anything. Most of it travels again for every job,
although the build node already holds almost all of it.

BuildKit can fetch the context itself from git. Measured on gitlab.com
with BuildKit v0.34.1, the form `<repository>?ref=<name>&checksum=<commit>
&fetch-by-commit=true` fetches only that commit (`--depth=1`) into one
copy per repository that the daemon keeps: 45 MB the first time, about
nothing for each next commit, and about 13 s per job instead of 60-68 s.
A plain commit id as the reference instead refetches the whole history
on every new commit.

## Decision

- The CI component gets the input `context-source: local | git`,
  default `local`. With `git` the job extends a template that sets
  `GIT_STRATEGY: none` for itself only (other jobs of the pipeline keep
  their checkout; with `local` the job sets no `GIT_STRATEGY`, so the
  pipeline's applies), passes
  the pipeline's commit in the form above, and gives the daemon the
  job token as the `GIT_AUTH_HEADER.<host>` secret (a file, never an
  argument or a log line).
- The console treats the commit of a git context as the build context:
  the steps' identities leave it out (like a local context's files), a
  rebuild behind it is unclear until the commits are compared, and the
  comparison of the commits holds for it in full (the context is the
  repository at that commit; not for a build that also reads a named
  local context), whatever
  `control-plane.reasons.trustGitComparison` says. A rebuild of the same
  commit is the cache's.
- Release 0.4.0: a new way to build, not a fix.

## Consequences

- With `git`, the context is committed files only: `.dockerignore` is not
  applied by BuildKit for a git context (use `COPY --exclude`), CI
  artifacts and files made in the job are not there, Git LFS files
  arrive as pointers and private submodules fail.
- A daemon that has fetched a commit serves its tree to any client that
  names the commit, with or without a token, and lists its recent builds'
  git addresses to every client. One installation is already one trust
  domain; the README's Limitations say so.
- The repository copy is an ordinary cache record: GC may remove it,
  after which the next job fetches its commit cold again.
