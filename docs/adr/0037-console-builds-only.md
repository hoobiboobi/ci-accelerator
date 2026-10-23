# 0037. Console v2: image builds only, four sections, five outcomes

Date: 2026-10-23

## Context

The console grew pages for every job of a pipeline, the runner cache per job, and Terraform runs. A test project shaped like a real monorepo (150 pipelines a day, 60 jobs each, 8 to 10 images per pipeline) showed what that costs. After 30 days every page took 13 to 25 s and weighed 1.4 to 1.7 MB, most of it in the pipeline list beside every page. The same 60 jobs appeared on up to five pages, and the numbers on a page outnumbered its sentences. GitLab already shows jobs, logs and pipelines well. What it cannot show is why an image build was slow or missed the cache, and whether the change or the build farm caused it: 63 % of the test project's rebuilds ran on a node other than the one that held the image's last build.

## Decision

- **One question:** why was this image build slow or not cached: the change or the build farm? Jobs without an image build get no pages; they appear only as grey bars on the pipeline's timeline, which shows every job in the order they ran (the author's choice after the first version dropped it: it shows which jobs wait for which). Terraform, OpenTofu and Terragrunt views (ADR-0036) and per-job runner cache views (ADR-0033) are dropped. The Nodes page keeps the bucket's size per project and the lifecycle warning. The S3 cache and runner tiers in the charts stay.
- **Four sections:**
  - **Overview:** rebuild reasons grouped like error fingerprints and ranked by the build time they cost. (A strip of each image's last 30 builds was dropped from it: without its legend nobody could read it; it stays on the Images page.)
  - **Pipelines** (added after the author's review the same day): each pipeline's jobs by stage on a time axis, with the time each waited for a runner (GitLab's `queued_duration`) and ran; a pipeline opens into its full timeline, each image build into stages and steps.
  - **Builds:** every image build of every branch, newest first. Filters for branch, outcome, image and "mine" live in the URL; rows can be grouped by pipeline; pages are cursor-based.
  - **Images:** cache rate, usual and p95 time, the steps that rebuild most and the reasons, over 24 hours, 7 or 30 days; builds per week by outcome over a year.
  - **Nodes.**
- **Five outcomes,** each with a colour, an icon and a word:
  - from cache;
  - your change (Dockerfile, build argument, base image, copied files, first build);
  - build farm (cached on another node, or evicted);
  - unclear (the last build ran on another node, files may or may not have changed);
  - failed.
- **A build page** opens with one sentence and at most three numbers. Below that it shows only what differs from the previous build of the image, then the steps with cached groups folded; everything else goes under Details. A duration is compared with the usual time of builds with the same outcome. Lists mark only rebuilds slower than the image's p95.
- **Scale:**
  - denormalised `built` counts and the project's last build time;
  - keyset pagination;
  - indexes chosen explicitly where SQLite picked badly;
  - a chart with more than 300 builds draws one dot per time slice and outcome.
- **Data:**
  - builds are kept 30 days by default (`retention.days`). Before they go, each image's UTC days are summed up (builds by outcome, executed steps' time, median cached and rebuilt time, p95 rebuilt time) and kept a year, for the image page's yearly view. The last three finished days are summed up again on every pass, because a daemon's history can bring builds in late;
  - GitLab's API is used for sign-in, permissions, and the end of a job's log on request;
  - live updates refresh the open pipeline or build; other pages offer "new builds" at most every 30 s.

## Consequences

- On a 30-day store of that project (270,000 jobs, 67,500 builds), every page except the 30-day image chart answers in 15 to 70 ms. Build lists stay under 45 kB. The 30-day image page takes about 0.5 s.
- People who used the console for deploy logs or test jobs go back to GitLab for them.
- The routes `/insights` and the job pages of jobs without builds no longer exist: `/insights` redirects to `/images`; a job link without a build shows a short page linking to GitLab.
