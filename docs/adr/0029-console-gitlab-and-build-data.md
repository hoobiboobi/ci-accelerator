# 0029. Console: GitLab sign-in, job data from pods, Dockerfiles and logs kept

Date: 2026-10-20

## Context

The console design draft used no GitLab API and had no access control. Three things changed that:

- **Builds can carry secrets.** Step output, the Dockerfile and the provenance (which records `--build-arg` values) can hold anything a build printed or was given. Showing every project's builds to everyone who can open the console is not acceptable.
- **A comparison of three UI designs on real gitlab.com builds.** Developers arrive from the job log and need the cause first. Platform teams need the whole pipeline at once. The Dockerfile is the natural map of a build: each line can show what happened to its step.
- **The data is in the build history.** A record's provenance holds the full Dockerfile and which lines each step came from. Its progress stream holds each step's output.

Two constraints shape how GitLab is used:

- **API limits.** gitlab.com allows 2000 API requests per minute per user today. Proposed limits for the Free tier are 100 per minute and 5000 per hour. Polling every job of a busy installation does not fit, and community exporters that poll are known for missing data.
- **Reachability.** GitLab (gitlab.com in particular) usually cannot call into a private cluster; the cluster can usually call GitLab.

One more finding: in six real pipelines, 8 builds were attributed to "the cache is on another node", but only 1 could be proven. The others also had changed files.

## Decision

- **Job data at build time, without the API.** The runner annotates every job pod with CI variables: job name and stage, commit title and author, ref, merge request, pipeline source, job start and pipeline creation time. The collector reads the annotations.
- **Sign-in and permissions through GitLab.** A user sees a project's builds only when GitLab lets them read that project's jobs. The console checks this as that user and caches the answer for a few minutes. An administrator can also sign in with basic auth. Whether sign-in runs in oauth2-proxy or in the console is decided when it is built.
- **The GitLab API only while a page is open**, as the signed-in user. It is one request per pipeline for what the cluster cannot see: jobs on other runners, the final status, queue time. A finished pipeline's answer is cached. There is no background polling. When GitLab is down or rate-limited, the page still renders from the console's own data.
- **Kept per build:**
  - the Dockerfile text and each step's line range;
  - each step's output, capped per step, with known secret formats masked.
- **Views:**
  - the build page has Steps, Dockerfile and Timeline;
  - the pipeline page has Summary, Timeline (one waterfall: pipeline, jobs, stages, steps) and Dockerfile (every job's result on every line).
- **"Not your change" needs proof.**
  - A rebuild is blamed on the node only when another job of the same pipeline (same commit) found the same step in the cache on its node.
  - When another job of the pipeline, on its own previous node, had to rebuild the same step because its files changed, the reason is "files changed".
  - Otherwise the page says the files or the node, and that it cannot tell which.
- **Stack:** Go `html/template` and `go:embed`, server-rendered SVG, a small script with server-sent events, and a JSON API under `/api/v1`. No single-page framework.
- **Collection and disk usage:**
  - the collector copies each record when its build completes;
  - the free and used space of the cache directory comes from a small sidecar in the daemon pod, because BuildKit's API does not report it.

## Consequences

- **Permissions follow GitLab.** The console has no users or roles of its own. Signing in needs the cluster to reach GitLab. A cluster that cannot reach GitLab can use basic auth only, and then everyone who signs in sees every project. The docs say so.
- **Logs are most of the stored data.** They are kept 7 days, like the failed-build excerpts in the draft.
- **Masking is best effort.** BuildKit does not know which output is secret. The docs keep the existing advice: do not pass secrets as build arguments or print them.
- **Webhooks are not used.**
- **Cache size per project is not shown in v0.2.** BuildKit's cache records do not say which build created them; that needs research first.
