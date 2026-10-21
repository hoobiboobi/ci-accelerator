package web

// research holds, per page, the findings the page is built on, with their
// sources: shown under "Why this screen looks like this".
var research = map[string][][]string{
	"home": {
		{"Every successful build-insight product delivers the link where the developer already is: Develocity prints the build-scan URL at the end of every build, Depot prints a build summary link, Docker writes a GitHub job summary.", "https://gradle.com/blog/maven-build-scans-and-build-caching"},
		{"GitLab auto-links plain URLs in job logs (own line, no trailing punctuation); terminal hyperlinks (OSC 8) are broken there (#550221).", "https://gitlab.com/gitlab-org/gitlab/-/issues/550221"},
		{"The job token cannot post merge-request comments (read-only for notes), so MR comments would need a personal token — later, opt-in.", "https://docs.gitlab.com/ci/jobs/ci_job_token/"},
	},
	"build": {
		{"The headline is the cause, not the timing: “why did the cache miss” is open upstream since 2014 (moby/moby#9294) and no tool answers it.", "https://github.com/moby/moby/issues/9294"},
		{"“Not your change” vs “your change”: Namespace splits code regressions from infrastructure problems; with per-node caches the cold node is our most likely false blame.", "https://namespace.so/docs/dashboard/builds"},
		{"Typical-vs-now: showing the expected duration helps developers plan their wait even without a speed-up.", "https://newsletter.getdx.com/p/build-times-and-developer-productivity"},
		{"Pull and push as their own segments: users file requests just to see image pull time (Bitbucket “show image pull time explicitly”).", "https://bitbucket.org/site/master/issues/14986/show-image-pull-time-explicitly"},
	},
	"timeline": {
		{"One waterfall from pipeline to step — the trace-view pattern of Honeycomb and Sentry (spans by time and parent/child, collapsed by default).", "https://docs.honeycomb.io/investigate/analyze/explore-traces/"},
		{"GitLab users have asked since 2017 for a “Chrome-devtools-network-style” job timeline (#2665).", "https://gitlab.com/gitlab-org/gitlab/-/issues/2665"},
		{"One-click critical path: Datadog CI Visibility.", "https://docs.datadoghq.com/continuous_integration/search/"},
	},
	"pipeline": {
		{"A job timeline with the critical path has been requested in GitLab since 2017 (#2665: “3 h pipeline, longest job 90 min — difficult to tell why”).", "https://gitlab.com/gitlab-org/gitlab/-/issues/2665"},
		{"Datadog CI Visibility ships a one-click critical path — table stakes, so it is here, but it is not the differentiator.", "https://docs.datadoghq.com/continuous_integration/search/"},
		{"“Cache saved” makes the value visible: few projects use CI caching at all, mostly from low awareness.", "https://arxiv.org/abs/2601.19146"},
	},
	"pdockerfile": {
		{"One chip per job and line makes BuildKit's deduplication visible: identical steps of concurrent builds share one vertex digest, so the step runs once and the other jobs receive it.", "https://github.com/moby/buildkit#readme"},
		{"BuildKit keeps the Dockerfile itself and a source map from every step to its Dockerfile lines in the build record (SLSA provenance), so this view needs no access to the repository.", "https://docs.docker.com/build/metadata/attestations/slsa-provenance/"},
	},
	"insights": {
		{"Per-step hit/miss history is what Depot users asked for most (“how much time is spent in each step”) — hosted only.", "https://depot.dev/changelog/2026-02-11-inline-build-step-analytics"},
		{"GitLab's own analytics: rounded to the minute, last 30 commits; the new CI Insights needs ClickHouse and is not GA on self-managed.", "https://gitlab.com/gitlab-org/gitlab/-/issues/444468"},
		{"Causes, not just rates: most mature GitLab projects carry cache misconfigurations — a cause breakdown points at what to fix.", "https://arxiv.org/html/2604.17890v1"},
	},
	"nodes": {
		{"Operators cannot see what fills the build disk or prune per project: buildkit#1512 is the most-upvoted BuildKit issue ever; Depot ships “Storage by Project” — hosted only.", "https://github.com/moby/buildkit/issues/1512"},
		{"GC is opaque (buildkit#1385, open since 2020) and du undercounts real usage (#6931) — so BuildKit's own number is labelled as BuildKit's.", "https://github.com/moby/buildkit/issues/6931"},
		{"A daemon can hang while its health probe still answers (#7275): “running now” comes from build history, not from the probe.", "https://github.com/moby/buildkit/issues/7275"},
	},
}

func researchFor(page string) [][]string { return research[page] }
