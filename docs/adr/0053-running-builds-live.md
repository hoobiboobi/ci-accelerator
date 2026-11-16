# 0053. A running build shows live on its job's page

Date: 2026-10-30

## Context

A job that builds for twenty minutes had a page that said only "a build
shows up when BuildKit completes it": no step, no line of output, no
load, nothing on the pipeline's timeline but one bar. The job's own log
in GitLab stops at 4 MB (a kernel build hit that in the middle of
`make modules`). The author chose, from mock-ups, a forecast against the
last build of the image, the node's load as four figures, and every step
on a growing timeline that opens to its last lines; the running step
open, a finished one folded, so the page follows the build by itself.

BuildKit streams any running build's progress to a client that asks for
it by the build's ref (the Control API's Status call), and replays what
it sent before: a page opened late still sees the build from its start.
Its history announces a build the moment it starts, with its ref and
options, but the build's trace (by which the console ties a completed
build to its job) exists only once the build is done.

## Decision

- **Which job a running build is for: from the build's options.** The CI
  component passes the job's TRACEPARENT as a build option,
  `ci-accelerator.io/traceparent`; the controller reads it from the
  build's start and attributes it as it attributes a completed build's
  trace (the same scheme and the same known GitLabs). The Dockerfile
  frontend ignores options it does not know, so no cache key sees it. A
  build without it is not shown live: matching by timing (which job's
  connection the router passed just before) was rejected, as it would
  show one project's output on another project's page when two jobs start
  together. A build counts as a job's only when its pipeline agrees, as
  for a completed build.
- **Progress only while someone looks.** The web asks the controller for a
  running build; the controller reads that build's Status stream from
  then on and for a minute after the last ask, at most 32 builds at once.
  It keeps each step's state and times and the last 8 lines it printed
  (secret build arguments and known token formats masked; a private key
  printed line by line is masked from its BEGIN line to its END line; a
  line still being printed shows only once it redraws itself, like a
  progress bar). The whole output stays what it was: stored from the
  build's record when it completes.
- **The node's load** comes from the probe next to the daemon, read every
  10 s while the build is followed.
- **The forecast** compares the build with the last successful build of
  the same image (same project and target, the same job name first; when
  the job's last stored build named its image, only builds of that image)
  that executed the step running now, by its text or else by its place in
  the Dockerfile and instruction. The rest takes what the steps still to
  run took then (a running one less what it ran; steps that ran side by
  side counted once). With fewer than 3 of 4 of those steps known, the
  page gives no time and says which steps have no earlier run. Without
  such a build the page says what runs and for how long, nothing more.
  (Changed 2026-11-16: the forecast was the reference's end shifted by
  how far ahead the build was, which a step taken from the cache now but
  built then, or the other way round, skewed by its whole time; a step
  whose text changes every pipeline found no reference at all.)
- **The pages** ask for themselves: a running job's page every 2 s, a
  pipeline with running builds every 5 s. The running job opens on the
  pipeline's timeline into its stages so far, its running stage open.
  What the reader opens or folds by hand stays so; the rest follows the
  build.

## Consequences

- Builds started by the CI component of an earlier release, or by a
  `buildctl` call of one's own, show live only with the option added
  (`--opt ci-accelerator.io/traceparent=$TRACEPARENT`); docs say so.
- One more stream per followed build between the controller and a daemon,
  with the build's logs (the client already receives the same); bounded
  by the 32 builds and the minute after the last ask.
- A web of a newer release than its controller (during an upgrade) shows
  the running job's page without the live part until the controller
  follows.
