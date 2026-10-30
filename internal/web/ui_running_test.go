package web

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// uiRunning is a pipeline of the browser checks' fixtures that runs (not
// the one the checks announce): a job ended, one runs with its build in
// BuildKit, one waits. Save stores the running job's build, as the
// collector does when BuildKit completes it.
type uiRunning struct {
	Pipeline, Job string // their pages
	Save          func() error
}

const (
	uiRunningPipeline = 3_000_736_700
	uiRunningJob      = 9_736_701
)

func seedRunning(t *testing.T, st *store.Store) uiRunning {
	t.Helper()
	keys, byPipe := pipelines(t, st)
	inst, _, _ := strings.Cut(keys[0], "/")
	pid := uint64(uiRunningPipeline)
	created := testNow.Add(-3 * time.Minute)
	for _, j := range []store.Job{
		{JobID: uiRunningJob - 1, Name: "lint", Stage: "test", StartedAt: created.Add(10 * time.Second),
			PodCreated: created.Add(6 * time.Second), PodRunning: created.Add(10 * time.Second)},
		{JobID: uiRunningJob, Name: "build-image", Stage: "build", StartedAt: testNow.Add(-90 * time.Second),
			PodCreated: testNow.Add(-95 * time.Second), PodRunning: testNow.Add(-90 * time.Second)},
	} {
		j.Instance, j.PipelineID, j.ProjectID = inst, pid, 42
		j.Ref, j.CommitTitle, j.PipelineCreatedAt = "main", "add the image", created
		j.URL = "https://gitlab.example.com/example/app/-/jobs/" + strconv.FormatUint(j.JobID, 10)
		if err := st.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.JobEnded(ctx, inst, uiRunningJob-1, created.Add(70*time.Second)); err != nil {
		t.Fatal(err)
	}
	// GitLab's word: running; the deploy job waits for the build.
	if _, err := st.SaveAPIPipeline(ctx, store.APIPipeline{Instance: inst, ID: pid, ProjectID: 42, Status: "running", Read: testNow,
		WebURL: "https://gitlab.example.com/example/app/-/pipelines/" + strconv.FormatUint(pid, 10),
		Jobs: []store.APIJob{
			{ID: uiRunningJob - 1, Name: "lint", Stage: "test", Status: "success", Queued: created, Started: created.Add(10 * time.Second),
				Finished: created.Add(70 * time.Second), Duration: time.Minute, Waited: 4 * time.Second},
			{ID: uiRunningJob, Name: "build-image", Stage: "build", Status: "running", Queued: testNow.Add(-97 * time.Second),
				Started: testNow.Add(-90 * time.Second), Waited: 7 * time.Second},
			{ID: uiRunningJob + 1, Name: "deploy", Stage: "deploy", Status: "created"},
		}}); err != nil {
		t.Fatal(err)
	}
	src := byPipe[keys[0]][0].ID
	return uiRunning{
		Pipeline: "/pipelines/" + store.PipelineKey(inst, pid),
		Job:      "/jobs/" + inst + "/" + strconv.Itoa(uiRunningJob),
		Save: func() error {
			pg, err := st.BuildPage(ctx, src)
			if err != nil {
				return err
			}
			b := *pg.Build
			b.Ref, b.Attribution.Pipeline, b.Attribution.Job = "ui-running-build", pid, uiRunningJob
			// Its times: it completes now, after its job started.
			shift := testNow.Add(-2 * time.Second).Sub(b.Completed)
			b.Started, b.Completed = b.Started.Add(shift), b.Completed.Add(shift)
			b.Steps = append([]history.Step(nil), b.Steps...)
			for i := range b.Steps {
				if !b.Steps[i].Started.IsZero() {
					b.Steps[i].Started = b.Steps[i].Started.Add(shift)
				}
				if !b.Steps[i].Completed.IsZero() {
					b.Steps[i].Completed = b.Steps[i].Completed.Add(shift)
				}
			}
			_, err = st.SaveBuild(ctx, store.Build{Build: &b, Generation: 1})
			return err
		},
	}
}

// The browser checks' running pipeline is what they expect of it, here
// where a failure is quicker to read: a running job's bar, a waiting job,
// a page that asks for itself; once Save stores the build, the job's page
// answers with the build's.
func TestUIRunningFixture(t *testing.T) {
	ts, st, _ := console(t)
	run := seedRunning(t, st)
	p := get(t, ts, run.Pipeline)
	p.safe(t)
	for _, want := range []string{"c-job-running", "1 job running, 1 waiting to start.", `data-refresh-every="15"`, "<b>3</b> jobs, running for <b>3\u00a0min",
		"1 other job of this pipeline built no image here; it is a grey bar on the timeline. Its log is in"} {
		if !strings.Contains(p.body, want) {
			t.Errorf("the running pipeline lacks %q: %s", want, between(p.body, `<div class="pc-facts">`, "</div>"))
		}
	}
	if j := get(t, ts, run.Job); j.status != 200 || !strings.Contains(j.body, `data-refresh-every="15"`) {
		t.Errorf("the running job's page: %d", j.status)
	}
	if err := run.Save(); err != nil {
		t.Fatal(err)
	}
	if j := get(t, ts, run.Job); j.status != 302 || !strings.HasSuffix(j.header.Get("Location"), "?via=log") {
		t.Errorf("the job's page with its build: %d %s", j.status, j.header.Get("Location"))
	}
	if b := get(t, ts, strings.TrimSuffix(get(t, ts, run.Job).header.Get("Location"), "?via=log")); b.status != 200 || regexp.MustCompile(`(^|\s|>)-\d+(\.\d+)?\s?(ms|s|min|h)\b`).MatchString(b.body) {
		t.Errorf("the stored build's page: %d, or a negative time", b.status)
	}
}
