package web

import (
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// A pipeline that has just started: one job runs (its pod is up, its
// build still in BuildKit, nothing stored), one waits. The page shows the
// timeline with the running job, says builds come when they complete, and
// asks for itself again while the pipeline runs.
func TestRunningPipelinePage(t *testing.T) {
	ts, st, _ := console(t)
	keys, byPipe := pipelines(t, st)
	inst, _, _ := strings.Cut(keys[0], "/")
	const pid = 3000736659
	key := inst + "/" + strconv.Itoa(pid)
	save := func(j store.Job) {
		t.Helper()
		j.Instance, j.PipelineID, j.ProjectID = inst, pid, 42
		j.URL = "https://gitlab.example.com/p/-/jobs/" + strconv.FormatUint(j.JobID, 10)
		if err := st.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	save(store.Job{JobID: 9_200_001, Name: "build-image", Stage: "build", StartedAt: testNow.Add(-90 * time.Second)})
	save(store.Job{JobID: 9_200_002, Name: "deploy", Stage: "deploy"})
	// GitLab says the deploy job waits (as SaveAPIPipeline stores it).
	if err := st.SaveAPIPipeline(ctx, store.APIPipeline{Instance: inst, ID: pid, ProjectID: 42, Status: "running", Read: testNow,
		Jobs: []store.APIJob{{ID: 9_200_001, Name: "build-image", Stage: "build", Status: "running", Started: testNow.Add(-90 * time.Second)},
			{ID: 9_200_002, Name: "deploy", Stage: "deploy", Status: "created"}}}); err != nil {
		t.Fatal(err)
	}
	p := get(t, ts, "/pipelines/"+key)
	p.safe(t)
	if strings.Contains(p.body, "No job of this pipeline built an image") {
		t.Error("a running pipeline says no job built an image")
	}
	if !strings.Contains(p.body, "data-wf") || !regexp.MustCompile(`class="row k-job[^"]*"[^>]*data-link="/jobs/`+inst+`/9200001"`).MatchString(p.body) {
		t.Error("the running job is not on the timeline")
	}
	if !strings.Contains(p.body, "c-job-running") {
		t.Error("the running job's bar is not marked running")
	}
	if !strings.Contains(p.body, "1 job running, 1 waiting to start.") {
		t.Error("running and waiting jobs are not counted")
	}
	if strings.Contains(p.body, "built no image here") {
		t.Error("jobs that have not ended are counted as jobs that built no image")
	}
	if !strings.Contains(p.body, "data-refresh-every=") {
		t.Error("a running pipeline does not ask for itself again")
	}
	if strings.Contains(p.body, "ended last") || strings.Contains(p.body, "critrow") || strings.Contains(p.body, "data-wf-crit") {
		t.Error("a running pipeline has a critical path")
	}
	if strings.Contains(p.body, "1 jobs") || !strings.Contains(p.body, "Job running, no finished build yet") {
		t.Error("timeline: plural or legend")
	}
	if strings.Contains(p.body, `<section class="box"><div class="blist"></div></section>`) {
		t.Error("an empty list of builds")
	}
	// Its job page does not say the job built nothing while it runs.
	if j := get(t, ts, "/jobs/"+inst+"/9200001"); strings.Contains(j.body, "This job built no image") || !strings.Contains(j.body, "has no finished image build") {
		t.Error("the running job's page says it built no image")
	}

	// The job ends without a build (its pod went away): one other job, a
	// grey bar, said in the singular; the waiting job still waits.
	if err := st.JobEnded(ctx, inst, 9_200_001, testNow.Add(-10*time.Second)); err != nil {
		t.Fatal(err)
	}
	p = get(t, ts, "/pipelines/"+key)
	if !strings.Contains(p.body, "1 other job of this pipeline built no image here; it is a grey bar on the timeline") {
		t.Errorf("one ended job: %s", between(p.body, `class="page inpipe"`, "</div>\n"))
	}
	if strings.Contains(p.body, "of them are") || strings.Contains(p.body, "1 of them") {
		t.Error("grammar: 1 of them are")
	}

	// A pipeline with stored builds and a job still running: "so far".
	inst0, id0, _ := strings.Cut(keys[0], "/")
	p0, _ := strconv.ParseUint(id0, 10, 64)
	if err := st.SaveJob(ctx, store.Job{Instance: inst0, JobID: 9_200_003, PipelineID: p0, ProjectID: 42, Name: "build-late", Stage: "build",
		StartedAt: testNow.Add(-30 * time.Second), URL: "https://gitlab.example.com/p/-/jobs/9200003"}); err != nil {
		t.Fatal(err)
	}
	p = get(t, ts, "/pipelines/"+keys[0])
	n := len(byPipe[keys[0]])
	if !strings.Contains(p.body, strconv.Itoa(n)+" image") || !strings.Contains(p.body, "so far") || strings.Contains(p.body, "Ready ") {
		t.Errorf("builds of a running pipeline: %s", between(p.body, `<p class="lead">`, "</p>"))
	}
}

func between(s, from, to string) string {
	i := strings.Index(s, from)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], to)
	if j < 0 {
		return s[i:min(len(s), i+800)]
	}
	return s[i : i+j]
}

// runningFixture saves jobs of pipeline pid of project 42 as job pods
// report them, and returns the pipeline's key.
type runningFixture struct {
	t    *testing.T
	st   *store.Store
	inst string
}

func newRunningFixture(t *testing.T, st *store.Store) runningFixture {
	keys, _ := pipelines(t, st)
	inst, _, _ := strings.Cut(keys[0], "/")
	return runningFixture{t, st, inst}
}

func (f runningFixture) job(pid uint64, j store.Job) string {
	f.t.Helper()
	j.Instance, j.PipelineID, j.ProjectID = f.inst, pid, 42
	j.URL = "https://gitlab.example.com/p/-/jobs/" + strconv.FormatUint(j.JobID, 10)
	if err := f.st.SaveJob(ctx, j); err != nil {
		f.t.Fatal(err)
	}
	return f.inst + "/" + strconv.FormatUint(pid, 10)
}

func (f runningFixture) gitlab(pid uint64, status string, jobs ...store.APIJob) {
	f.t.Helper()
	if err := f.st.SaveAPIPipeline(ctx, store.APIPipeline{Instance: f.inst, ID: pid, ProjectID: 42, Status: status, Read: testNow, Jobs: jobs}); err != nil {
		f.t.Fatal(err)
	}
}

var mainLive = regexp.MustCompile(`<div id="main" data-live="([^"]*)"`)

// The page a job's log links to (printed when the job starts) is live on
// the job's pipeline and asks for itself again while the job runs or
// waits: the job's build, once stored, turns it into the build's page.
func TestRunningJobPageIsLive(t *testing.T) {
	ts, st, _ := console(t)
	f := newRunningFixture(t, st)
	key := f.job(3_000_000_101, store.Job{JobID: 9_400_001, Name: "build-image", Stage: "build",
		StartedAt: testNow.Add(-40 * time.Second), PodCreated: testNow.Add(-45 * time.Second), PodRunning: testNow.Add(-40 * time.Second)})
	f.job(3_000_000_101, store.Job{JobID: 9_400_002, Name: "push", Stage: "push", PodCreated: testNow.Add(-5 * time.Second)})
	for _, c := range []struct {
		path, why string
	}{{"/jobs/" + f.inst + "/9400001", "running"}, {"/jobs/" + f.inst + "/9400002", "starting (pod created, not started)"}} {
		p := get(t, ts, c.path)
		p.safe(t)
		if m := mainLive.FindStringSubmatch(p.body); m == nil || m[1] != key {
			t.Errorf("%s job page: not live on its pipeline %s (%v)", c.why, key, m)
		}
		if !strings.Contains(p.body, `data-refresh-every="15"`) {
			t.Errorf("%s job page does not ask for itself again", c.why)
		}
		if strings.Contains(p.body, "This job built no image") {
			t.Errorf("%s job page says the job built no image", c.why)
		}
	}
	if p := get(t, ts, "/jobs/"+f.inst+"/9400002"); !strings.Contains(p.body, "This job has not started yet") || strings.Contains(p.body, "running for") {
		t.Error("a starting job's page")
	}
	// Ended: still live (a late build), but no more asking.
	if err := st.JobEnded(ctx, f.inst, 9_400_001, testNow.Add(-2*time.Second)); err != nil {
		t.Fatal(err)
	}
	p := get(t, ts, "/jobs/"+f.inst+"/9400001")
	if strings.Contains(p.body, "data-refresh-every") || !strings.Contains(p.body, "This job built no image") {
		t.Error("an ended job's page asks for itself again, or does not say it built no image")
	}
	// A job of no known pipeline: nothing to be live on.
	if err := st.SaveJob(ctx, store.Job{Instance: f.inst, JobID: 9_400_003, ProjectID: 42, Name: "lone"}); err != nil {
		t.Fatal(err)
	}
	if p := get(t, ts, "/jobs/"+f.inst+"/9400003"); strings.Contains(p.body, "data-live=") {
		t.Error("a job without a pipeline is live")
	}
	// Once its build is stored, the job's page answers with the build's.
	keys, byPipe := pipelines(t, st)
	pg, err := st.BuildPage(ctx, byPipe[keys[0]][0].ID)
	if err != nil {
		t.Fatal(err)
	}
	b := *pg.Build
	b.Ref, b.Attribution.Pipeline, b.Attribution.Job = "running-job-build", 3_000_000_101, 9_400_002
	id, err := st.SaveBuild(ctx, store.Build{Build: &b, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if p := get(t, ts, "/jobs/"+f.inst+"/9400002"); p.status != 302 || p.header.Get("Location") != "/builds/"+strconv.FormatInt(id, 10)+"?via=log" {
		t.Errorf("the job's page with its build stored: %d %s", p.status, p.header.Get("Location"))
	}
}

// A job is running when GitLab says so, or, without GitLab's word, when
// its pod started its build container less than a day ago; a pod created
// and not started is a job still starting. A pod deleted while the
// controller was down leaves a job without an end: it must not run (and
// its pipeline refresh itself) for ever.
func TestNoEndlessRunning(t *testing.T) {
	now := testNow
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	for _, c := range []struct {
		name             string
		j                store.JobInfo
		running, waiting bool
	}{
		{"GitLab: running", store.JobInfo{Status: "running", Started: ago(72 * time.Hour)}, true, false},
		{"GitLab: pending", store.JobInfo{Status: "pending"}, false, true},
		// The runner takes a job (GitLab: running) before it creates its
		// pod, so "pending" with a pod up is GitLab read before that.
		{"GitLab: pending, pod up", store.JobInfo{Status: "pending", PodCreated: ago(time.Second), PodRunning: ago(time.Second)}, true, false},
		{"GitLab: pending, pod created, not started", store.JobInfo{Status: "pending", PodCreated: ago(time.Second)}, false, true},
		{"GitLab: manual", store.JobInfo{Status: "manual"}, false, false},
		{"GitLab: success", store.JobInfo{Status: "success", Started: ago(time.Minute)}, false, false},
		{"pod started a minute ago", store.JobInfo{Started: ago(time.Minute), PodCreated: ago(time.Minute), PodRunning: ago(time.Minute)}, true, false},
		{"pod started 25 h ago, never ended", store.JobInfo{Started: ago(25 * time.Hour), PodCreated: ago(25 * time.Hour), PodRunning: ago(25 * time.Hour)}, false, false},
		{"pod created, not started", store.JobInfo{Started: ago(time.Minute), PodCreated: ago(time.Minute)}, false, true},
		{"pod created 2 days ago, never started", store.JobInfo{PodCreated: ago(48 * time.Hour)}, false, false},
		{"pod ended", store.JobInfo{Started: ago(time.Minute), PodCreated: ago(time.Minute), PodRunning: ago(time.Minute), Finished: ago(time.Second)}, false, false},
		{"older store: start only, an hour ago", store.JobInfo{Started: ago(time.Hour)}, true, false},
		{"older store: start only, 3 days ago", store.JobInfo{Started: ago(72 * time.Hour)}, false, false},
		{"nothing known", store.JobInfo{}, false, false},
	} {
		if r, w := jobRunning(c.j, now), jobWaiting(c.j, now); r != c.running || w != c.waiting {
			t.Errorf("%s: running %v waiting %v, want %v %v", c.name, r, w, c.running, c.waiting)
		}
	}

	ts, st, _ := console(t)
	f := newRunningFixture(t, st)
	// No token: a job whose pod went away unseen three days ago.
	stale := f.job(3_000_000_201, store.Job{JobID: 9_500_001, Name: "build-image", Stage: "build",
		StartedAt: ago(72 * time.Hour), PodCreated: ago(72 * time.Hour), PodRunning: ago(72 * time.Hour)})
	p := get(t, ts, "/pipelines/"+stale)
	p.safe(t)
	if strings.Contains(p.body, "data-refresh-every") || strings.Contains(p.body, "running") || strings.Contains(p.body, "c-job-running") {
		t.Errorf("a job gone unseen three days ago still runs: %s", between(p.body, `class="page inpipe"`, "</header>"))
	}
	if j := get(t, ts, "/jobs/"+f.inst+"/9500001"); strings.Contains(j.body, "data-refresh-every") || strings.Contains(j.body, "running for") {
		t.Error("its job page still runs")
	}
	// Its card in the list: neither running nor a bar to now.
	if c := pipeCardOf(t, ts, f.inst, stale); strings.Contains(c, "running for") || strings.Contains(c, "jw-running") || !strings.Contains(c, "<b>1</b> job") {
		t.Errorf("the card of a pipeline whose job went unseen: %q", c)
	}
	// The same in a pipeline with builds, whose timeline is shown.
	keys, _ := pipelines(t, st)
	pid0, _ := strconv.ParseUint(strings.SplitN(keys[0], "/", 2)[1], 10, 64)
	f.job(pid0, store.Job{JobID: 9_500_003, Name: "gone-unseen", Stage: "test",
		StartedAt: ago(30 * time.Hour), PodCreated: ago(30 * time.Hour), PodRunning: ago(30 * time.Hour)})
	p = get(t, ts, "/pipelines/"+keys[0])
	if !strings.Contains(p.body, "data-wf") || strings.Contains(p.body, "data-refresh-every") || strings.Contains(p.body, "c-job-running") || strings.Contains(p.body, "so far") {
		t.Error("a pipeline with builds and a job gone unseen 30 hours ago still runs")
	}
	// A pod created and not started: the job starts, it does not run.
	starting := f.job(3_000_000_202, store.Job{JobID: 9_500_002, Name: "build-image", Stage: "build",
		StartedAt: ago(10 * time.Second), PodCreated: ago(10 * time.Second)})
	p = get(t, ts, "/pipelines/"+starting)
	if !strings.Contains(p.body, "1 waiting to start.") || regexp.MustCompile(`\d+ jobs? running`).MatchString(p.body) || !strings.Contains(p.body, "data-refresh-every") {
		t.Errorf("a starting job: %s", between(p.body, `<p class="lead">`, "</div>"))
	}
	// GitLab's word on the pipeline: still running in every state that
	// is not over, even when no job runs or waits; manual is not.
	for i, status := range []string{"created", "waiting_for_resource", "preparing", "pending", "running", "scheduled", "canceling", "manual", "success", "failed", "canceled", "skipped"} {
		pid := uint64(3_000_000_300 + i)
		key := f.job(pid, store.Job{JobID: 9_600_000 + uint64(i), Name: "lint", Stage: "test", StartedAt: ago(time.Minute)})
		if err := st.JobEnded(ctx, f.inst, 9_600_000+uint64(i), ago(30*time.Second)); err != nil {
			t.Fatal(err)
		}
		f.gitlab(pid, status, store.APIJob{ID: 9_600_000 + uint64(i), Name: "lint", Stage: "test", Status: "success", Started: ago(time.Minute), Finished: ago(30 * time.Second)})
		p := get(t, ts, "/pipelines/"+key)
		want := status != "manual" && !store.Finished(status)
		if got := strings.Contains(p.body, "data-refresh-every"); got != want {
			t.Errorf("GitLab says %s: in progress %v, want %v", status, got, want)
		}
		if got := strings.Contains(p.body, ", running for <b>"); got != want {
			t.Errorf("GitLab says %s: running for %v, want %v", status, got, want)
		}
	}
	// GitLab says the pipeline waits for a person: the jobs behind the
	// manual one may never start.
	manual := f.job(3_000_000_350, store.Job{JobID: 9_650_001, Name: "plan", Stage: "plan", StartedAt: ago(time.Minute)})
	if err := st.JobEnded(ctx, f.inst, 9_650_001, ago(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	f.gitlab(3_000_000_350, "manual", store.APIJob{ID: 9_650_001, Name: "plan", Stage: "plan", Status: "success", Started: ago(time.Minute), Finished: ago(30 * time.Second)},
		store.APIJob{ID: 9_650_002, Name: "apply", Stage: "apply", Status: "manual"}, store.APIJob{ID: 9_650_003, Name: "notify", Stage: "notify", Status: "created"})
	if p := get(t, ts, "/pipelines/"+manual); strings.Contains(p.body, "data-refresh-every") || strings.Contains(p.body, "waiting to start") {
		t.Error("a pipeline waiting for a person refreshes itself, or its jobs behind the manual one wait to start")
	}
	// GitLab says it is over: a pod still up does not make it run.
	over := f.job(3_000_000_400, store.Job{JobID: 9_700_001, Name: "build-image", Stage: "build",
		StartedAt: ago(time.Minute), PodCreated: ago(time.Minute), PodRunning: ago(time.Minute)})
	f.gitlab(3_000_000_400, "success", store.APIJob{ID: 9_700_001, Name: "build-image", Stage: "build", Status: "success", Started: ago(time.Minute), Finished: ago(time.Second)})
	if p := get(t, ts, "/pipelines/"+over); strings.Contains(p.body, "data-refresh-every") {
		t.Error("a pipeline GitLab says is over refreshes itself")
	}
}

// The pipelines list: a pipeline in progress counts its jobs that have
// not started as waiting, not as jobs that did not run, and its time as
// running for so long; once over, they did not run.
func TestPipelineCardInProgress(t *testing.T) {
	ts, st, _ := console(t)
	f := newRunningFixture(t, st)
	const pid = 3_000_000_501
	key := f.job(pid, store.Job{JobID: 9_800_001, Name: "build-image", Stage: "build", StartedAt: testNow.Add(-2 * time.Minute)})
	jobs := []store.APIJob{{ID: 9_800_001, Name: "build-image", Stage: "build", Status: "running", Started: testNow.Add(-2 * time.Minute)},
		{ID: 9_800_002, Name: "test", Stage: "test", Status: "created"}, {ID: 9_800_003, Name: "deploy", Stage: "deploy", Status: "created"},
		{ID: 9_800_004, Name: "release", Stage: "deploy", Status: "manual"}}
	f.gitlab(pid, "running", jobs...)
	card := func() string {
		t.Helper()
		return regexp.MustCompile(`<[^>]+>`).ReplaceAllString(pipeCardOf(t, ts, f.inst, key), "")
	}
	c := card()
	if !strings.Contains(c, "4 jobs · 2 waiting · 1 manual") || strings.Contains(c, "did not run") || !strings.Contains(c, "running for 2\u00a0min") {
		t.Errorf("a running pipeline's card: %q", c)
	}
	if p := get(t, ts, "/pipelines/"+key); !strings.Contains(p.body, "jobs, running for <b>2\u00a0min") || strings.Contains(p.body, " in <b>") || strings.Contains(p.body, "did not run") {
		t.Errorf("the pipeline page's facts: %s", between(p.body, `<div class="pc-facts">`, "</div>"))
	}
	// Over: what never started did not run.
	jobs[0].Status, jobs[0].Finished = "failed", testNow.Add(-time.Minute)
	for i := 1; i < 3; i++ {
		jobs[i].Status = "skipped"
	}
	f.gitlab(pid, "failed", jobs...)
	c = card()
	if !strings.Contains(c, "4 jobs · 3 did not run") || strings.Contains(c, "waiting") || strings.Contains(c, "running for") {
		t.Errorf("an ended pipeline's card: %q", strings.Join(strings.Fields(c), " "))
	}
}

// Without a token GitLab's status is not known: between two stages no job
// runs or waits, and the page must not say the pipeline is over.
func TestNoTokenBetweenStages(t *testing.T) {
	ts, st, _ := console(t)
	f := newRunningFixture(t, st)
	key := f.job(3_000_000_601, store.Job{JobID: 9_900_001, Name: "lint", Stage: "test",
		StartedAt: testNow.Add(-time.Minute), PodCreated: testNow.Add(-time.Minute), PodRunning: testNow.Add(-time.Minute)})
	if err := st.JobEnded(ctx, f.inst, 9_900_001, testNow.Add(-5*time.Second)); err != nil {
		t.Fatal(err)
	}
	p := get(t, ts, "/pipelines/"+key)
	p.safe(t)
	if !strings.Contains(p.body, "No job of this pipeline has built an image on the box so far.") || strings.Contains(p.body, "built an image on the box.</b>") {
		t.Errorf("no token, between stages: %s", between(p.body, `<div class="empty">`, "</div>"))
	}
	// GitLab says it is over: then it is.
	f.gitlab(3_000_000_601, "success", store.APIJob{ID: 9_900_001, Name: "lint", Stage: "test", Status: "success",
		Started: testNow.Add(-time.Minute), Finished: testNow.Add(-5 * time.Second)})
	if p := get(t, ts, "/pipelines/"+key); !strings.Contains(p.body, "No job of this pipeline built an image on the box.") || strings.Contains(p.body, "so far") {
		t.Errorf("GitLab says it is over: %s", between(p.body, `<div class="empty">`, "</div>"))
	}
}

// pipeCardOf is the markup of a pipeline's card in its project's list
// (project 42).
func pipeCardOf(t *testing.T, ts *httptest.Server, inst, key string) string {
	t.Helper()
	p := get(t, ts, "/pipelines?project="+inst+"/42")
	p.safe(t)
	for _, c := range strings.Split(p.body, `<article class="pcard">`)[1:] {
		if strings.Contains(c, `href="/pipelines/`+key+`"`) {
			return c[:strings.Index(c, "</article>")]
		}
	}
	t.Fatalf("no card for %s", key)
	return ""
}

// GitLab's word can be older than the pod's: read while the job was
// pending, then its pod started the build container. The pod wins until
// GitLab is read again; a job with a stored build never waits; a job
// behind a manual job, in a pipeline GitLab calls manual, waits for
// nothing on its own page either.
func TestStaleGitLabWordAndManual(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	stale := store.JobInfo{ID: 1, Status: "pending", Started: now.Add(-4 * time.Minute),
		PodCreated: now.Add(-5 * time.Minute), PodRunning: now.Add(-4 * time.Minute)}
	if !jobRunning(stale, now) || jobWaiting(stale, now) {
		t.Errorf("a pending job whose pod runs: running %v, waiting %v", jobRunning(stale, now), jobWaiting(stale, now))
	}
	notYet := store.JobInfo{ID: 2, Status: "pending"}
	if jobRunning(notYet, now) || !jobWaiting(notYet, now) {
		t.Error("a pending job without a pod is not waiting")
	}
	built := store.JobInfo{ID: 3, Status: "pending"}
	js := jobStates("running", []store.JobInfo{built}, map[uint64]bool{3: true}, now)
	if js.waiting[3] || js.running[3] {
		t.Errorf("a job with a stored build: waiting %v, running %v", js.waiting[3], js.running[3])
	}
	behind := store.JobInfo{ID: 4, PipelineID: 7, Status: "created"}
	v := &JobView{}
	jobFacts(v, &behind, "manual", now)
	if v.Waiting || v.Running {
		t.Errorf("a created job behind a manual job: waiting %v, running %v", v.Waiting, v.Running)
	}
	v = &JobView{}
	jobFacts(v, &behind, "running", now)
	if !v.Waiting {
		t.Error("a created job of a running pipeline does not wait")
	}
	v = &JobView{}
	jobFacts(v, &stale, "success", now)
	if v.Waiting || v.Running {
		t.Error("a job of a pipeline GitLab says is over still runs or waits")
	}
}
