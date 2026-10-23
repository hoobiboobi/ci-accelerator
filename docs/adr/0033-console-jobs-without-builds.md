# 0033. Console: GitLab says how jobs went; pipelines without builds

Date: 2026-10-22

Status: superseded by [ADR-0037](0037-console-builds-only.md): the console shows image builds only.

## Context

The console listed only pipelines that built an image and read a job's state from its pod. A pipeline with real jobs on gitlab.com showed what that leaves out: deploy-only pipelines vanished from the list once their pods were gone, their time was 0, a job's link answered "not found", and a job showed as running for up to 30 s after it ended. A pod never knows how its job ended (the runner reports that to GitLab), does not exist while the job waits, and outlives it.

## Decision

- **GitLab's API is the only source of a job's status and times**: queue, start, end, result, retries (every try), manual and delayed jobs, and whether a failed job was allowed to fail (GitLab decides that from `allow_failure` and its exit codes). A job pod gives the job's data (project, name, stage, commit: GitLab's CI variables, copied by the runner) and the signal "running now". No status is ever derived from a pod.
- **A job ends when its pod's deletion is asked** (deadline less grace period), not when the pod is gone.
- **When GitLab's word is out of date, the result is unknown.** GitLab last called the job running, pending or manual, and its pod has ended since: the page says "finished", its result GitLab's to say, until GitLab is read again. Without a token, that is what a page always says, and times from pods are marked approximate.
- **GitLab is read while a page shows a pipeline**: a running pipeline every 20 s at most, including when nothing else changed, and a finished one again only when a job of it ran since GitLab's last answer (a manual job run, a job retried). Nothing is read when no page asks.
- **A job's wait for a runner is GitLab's `queued_duration`.** GitLab creates every job of a pipeline at once, so a job's creation time also counts the earlier stages.
- **Pipelines without builds are first-class.** They are listed with their jobs; their page sums up jobs by status, time waiting for runners and the slowest job, and its timeline shows every job. A job without a build has a page of its own: what GitLab says it did, its facts, its runs in earlier pipelines, and the last lines of its log read from GitLab when asked (secrets masked, shown as text). Jobs with builds keep the build views (Dockerfile, steps, timeline).
- **Never reachable from GitLab.** No webhooks: the console only reads GitLab's API, outbound.

## Consequences

- A pipeline whose jobs ran here but built nothing is visible for as long as builds are kept (90 days). Jobs and pipelines record when the console first heard of them, so rows without any time (a manual job never run) are pruned too.
- Projects that only deploy can be chosen in the console.
- Without a token the console cannot say whether a job without a build passed; it says so instead of guessing.
- An open page of a running pipeline costs one GitLab request every 20 s; closing it stops them.
