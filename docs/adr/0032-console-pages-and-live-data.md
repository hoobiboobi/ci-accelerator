# 0032. Console pages: the agreed design, live data, a GitLab token for now

Date: 2026-10-21

## Context

The console's pages follow the design agreed in ADR-0029: a sidebar, the pipeline list beside the pages, the build page with its verdict, Steps, Dockerfile and Timeline, a pipeline page with a waterfall, project insights and a build-nodes view. Several of these need data that BuildKit's records do not carry:
- how long a job ran before and after its build (runner, checkout);
- which jobs run now, and which builds;
- which node each daemon runs on, and what its cache holds;
- the link from a GitLab job log to the job's page.

Sign-in through GitLab, where each user's token reads the API, is not built yet.

## Decision

- **GitLab's API with one token, for now.** When the console has a token (`gitlab.token.existingSecret`), it reads a pipeline's jobs while a page shows the pipeline. That is one request per pipeline, a running one again after 20 s at the earliest, a finished one never again. Nothing runs in the background, and nothing is sent while GitLab rate-limits. Without a token, job times come from the job pods (start annotation, pod deletion). Sign-in through GitLab replaces the shared token with each user's own.
- **Running jobs from their pods, running builds from the daemons.** A job pod that exists is a job in progress. A daemon reports its running builds when the history stream starts and then as they start and complete. A daemon whose address leaves the Service is forgotten.
- **The node as a worker label.** The buildkit chart starts every daemon with `--oci-worker-labels ci-accelerator.io/node=$(node)` from the downward API. The console reads it, with the cache policies and a per-kind sum of the cache, from the daemon every minute. Free disk space is not reported by BuildKit; a disk probe in the daemon pod is planned.
- **History for the console.** The buildkit chart keeps 1000 records per daemon (48 h) by default, so the console can read the history again after a restart. It is the buildkit chart's default rather than the box's, so `helm upgrade --reuse-values` and `--reset-then-reuse-values` give the same daemons.
- **A link in every job log.** With `global.console.url` set, the runner hook prints `ci-accelerator: why this job's builds take their time: <url>/jobs/<instance>/<job id>`, computed from the job's own ids.
- **Style attributes allowed by the content security policy** (`style-src-attr 'unsafe-inline'`). Bars and positions are written into `style` attributes by the server. Scripts stay the console's own, and `<style>` elements stay forbidden. The tests check that every style attribute holds only numbers for left, right, width, height, top and flex.
- **A step that reads no files and was rebuilt on another daemon** than the target's previous build is put down to the node, with no other job needed as proof.

## Consequences

- The upgrade restarts every daemon once: the worker label and the history limits change the pod template. The upgrade notes say so.
- With a token, the console calls GitLab only while people look at pipelines. GitLab.com's free-plan burst limit (100 requests) is far away.
- A browser without `style-src-attr` (older than Chrome 75, Firefox 108, Safari 15.4) shows the pages without bars.
- The build-nodes view shows what BuildKit counts. Disk free and cache per project stay open.
