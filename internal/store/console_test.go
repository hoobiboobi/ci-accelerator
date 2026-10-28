package store

import (
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

func savedFixtures(t *testing.T, s *Store) []*history.Build {
	t.Helper()
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	for _, b := range bs {
		prev, _, err := s.Previous(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		var p *history.Build
		if prev != nil {
			p = prev.Build
		}
		r, _ := history.FirstRebuild(p, b)
		if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1, Reason: r}); err != nil {
			t.Fatal(err)
		}
	}
	return bs
}

func TestConsoleQueries(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := savedFixtures(t, s)

	projects, err := s.Projects(ctx)
	if err != nil || len(projects) != 1 || projects[0].ID != 42 || projects[0].Path != "example/app" || projects[0].Last.IsZero() {
		t.Fatalf("projects %+v %v", projects, err)
	}
	key := projects[0].ProjectKey
	if k, ok := ParseProjectKey(key.String()); !ok || k != key {
		t.Errorf("project key %q -> %+v", key.String(), k)
	}
	for _, bad := range []string{"", "x/1", "abcdef/0", "abcdef", "abcdef/x", "abcdefg/1"} {
		if _, ok := ParseProjectKey(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}

	// The builds of a project, newest first, with what lists need.
	attributed := 0
	for _, b := range bs {
		if b.Attribution.State == history.Attributed {
			attributed++
		}
	}
	// A build no job is known for belongs to no project's list.
	rows, err := s.Builds(ctx, BuildQuery{Project: key})
	if err != nil || len(rows) != attributed || attributed == len(bs) {
		t.Fatalf("builds: %d of %d attributed (%d in all), %v", len(rows), attributed, len(bs), err)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Started.After(rows[i-1].Started) {
			t.Fatalf("not newest first at %d", i)
		}
	}
	if other, err := s.Builds(ctx, BuildQuery{Project: ProjectKey{Instance: key.Instance, ID: 7}}); err != nil || len(other) != 0 {
		t.Errorf("other project: %d %v", len(other), err)
	}
	p104, err := s.Builds(ctx, BuildQuery{Project: key, Pipeline: 104})
	if err != nil || len(p104) != 3 {
		t.Fatalf("pipeline 104: %d %v", len(p104), err)
	}
	for _, b := range p104 {
		if b.JobName == "" || b.Daemon != "daemon-a" || b.Started.IsZero() || b.Completed.IsZero() || b.Instance != key.Instance || b.Pipeline != 104 || b.Title != "a change" {
			t.Errorf("build row %+v", b)
		}
	}
	// What a build executed is counted when it is saved: the outcomes
	// add up.
	byOutcome := map[string]int{}
	for _, o := range []string{"cached", "change", "farm", "unclear", "failed", "rebuilt"} {
		got, err := s.Builds(ctx, BuildQuery{Project: key, Outcome: o})
		if err != nil {
			t.Fatal(err)
		}
		byOutcome[o] = len(got)
		for _, b := range got {
			if o == "cached" && (b.Built != 0 || b.Error != "") {
				t.Errorf("a cached build executed steps: %+v", b)
			}
			if o == "rebuilt" && b.Built == 0 && b.Error == "" && b.Reason == "" {
				t.Errorf("a rebuilt build executed nothing: %+v", b)
			}
		}
	}
	if byOutcome["cached"]+byOutcome["rebuilt"] != attributed || byOutcome["rebuilt"] == 0 || byOutcome["cached"] == 0 ||
		byOutcome["change"]+byOutcome["farm"]+byOutcome["unclear"]+byOutcome["failed"] != byOutcome["rebuilt"] {
		t.Errorf("outcomes do not add up: %v of %d", byOutcome, attributed)
	}
	if few, _ := s.Builds(ctx, BuildQuery{Project: key, Limit: 3}); len(few) != 3 {
		t.Errorf("limit: %d", len(few))
	}
	older, _ := s.Builds(ctx, BuildQuery{Project: key, Before: rows[2].Started})
	if len(older) != len(rows)-3 || older[0].ID != rows[3].ID {
		t.Errorf("before: %d, first %d", len(older), older[0].ID)
	}
	target := rows[0].Target
	if ts, err := s.Targets(ctx, key, time.Time{}); err != nil || len(ts) == 0 {
		t.Errorf("targets %v %v", ts, err)
	}
	for _, b := range mustBuilds(t, s, BuildQuery{Project: key, Target: target}) {
		if b.Target != target {
			t.Errorf("target filter: %q", b.Target)
		}
	}
	if err := s.SaveJob(ctx, Job{Instance: key.Instance, JobID: p104[0].Job, PipelineID: 104, ProjectID: 42, Name: p104[0].JobName,
		Ref: "main", CommitTitle: "a change"}); err != nil {
		t.Fatal(err)
	}
	refs, err := s.Branches(ctx, key, time.Time{})
	if err != nil || len(refs) == 0 {
		t.Fatalf("branches %v %v", refs, err)
	}
	if on := mustBuilds(t, s, BuildQuery{Project: key, Ref: refs[0]}); refs[0] != "main" || len(on) != 3 || on[0].Pipeline != 104 {
		t.Errorf("branch filter: %v, %d builds", refs, len(on))
	}
	if counts, err := s.StepCounts(ctx, key, target, time.Time{}, 5); err != nil {
		t.Errorf("step counts %v", err)
	} else {
		for _, c := range counts {
			if c.Ran <= 0 || c.RanTime < 0 || strings.HasPrefix(c.Name, "FROM ") {
				t.Errorf("step count %+v", c)
			}
		}
	}

	// GitLab's API adds times and jobs from other runners, and keeps what
	// the pods said.
	j0 := JobInfo{ID: p104[0].Job, Name: p104[0].JobName}
	started := time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)
	err = s.SaveAPIPipeline(ctx, APIPipeline{Instance: key.Instance, ID: 104, ProjectID: 42, Status: "success",
		WebURL: "https://gitlab.example.com/p/-/pipelines/104", Read: c.now(), Jobs: []APIJob{
			{ID: j0.ID, Name: "renamed", Status: "success", Started: started, Finished: started.Add(13 * time.Second),
				Duration: 13 * time.Second, Waited: 2 * time.Second, Runner: "box", WebURL: "https://x/jobs/1"},
			{ID: 999, Name: "deploy", Stage: "deploy", Status: "manual"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := s.PipelineJobs(ctx, key.Instance, 104)
	if err != nil || len(jobs) != 4 {
		t.Fatalf("pipeline jobs %d %v", len(jobs), err)
	}
	var got, deploy JobInfo
	for _, j := range jobs {
		switch j.ID {
		case j0.ID:
			got = j
		case 999:
			deploy = j
		}
	}
	if got.Name != j0.Name || got.Duration != 13*time.Second || got.Waited != 2*time.Second || got.Runner != "box" ||
		!got.Started.Equal(started) || got.Status != "success" || got.URL != "https://x/jobs/1" {
		t.Errorf("api job %+v (the pod's name stays, an empty url is filled)", got)
	}
	if deploy.Name != "deploy" || deploy.Status != "manual" || !deploy.Started.IsZero() {
		t.Errorf("job of another runner %+v", deploy)
	}
	// A pod's end does not override GitLab's times, and fills them in
	// for a job GitLab has not described.
	if err := s.JobEnded(ctx, key.Instance, j0.ID, started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	j1 := JobInfo{ID: p104[1].Job, Name: p104[1].JobName}
	if err := s.SaveJob(ctx, Job{Instance: key.Instance, JobID: j1.ID, PipelineID: 104, ProjectID: 42, Name: j1.Name, StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	if err := s.JobEnded(ctx, key.Instance, j1.ID, started.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.JobEnded(ctx, key.Instance, 123456, started); err != nil {
		t.Errorf("unknown job: %v", err)
	}
	jobs, _ = s.PipelineJobs(ctx, key.Instance, 104)
	for _, j := range jobs {
		if j.ID == j0.ID && j.Duration != 13*time.Second {
			t.Errorf("pod end replaced GitLab's duration: %v", j.Duration)
		}
		if j.ID == j1.ID && (j.Duration != 20*time.Second || !j.Finished.Equal(started.Add(20*time.Second))) {
			t.Errorf("pod end: %+v", j)
		}
	}
	pl, err := s.PipelineInfo(ctx, key.Instance, 104)
	if err != nil || pl == nil || pl.Status != "success" || pl.WebURL == "" || pl.APIAt.IsZero() || !Finished(pl.Status) {
		t.Errorf("pipeline %+v %v", pl, err)
	}

	counts, err := s.DaemonCounts(ctx, time.Time{}, ProjectsIn{All: true})
	if err != nil || counts["daemon-a"].Builds != len(bs) || counts["daemon-a"].Last.IsZero() {
		t.Errorf("daemon counts %+v %v", counts, err)
	}
	// Per project: its builds (the fixtures have one build of no known
	// project), rebuilds and last build, and only the projects asked for.
	a := counts["daemon-a"]
	if a.ProjectCount != 1 || len(a.Projects) != 1 || a.Projects[0].ProjectKey != key {
		t.Fatalf("daemon-a per project %+v", a)
	}
	if p := a.Projects[0]; p.Builds != a.Builds-1 || p.Farm != a.Farm || p.Unclear != a.Unclear || !p.Last.Equal(a.Last) {
		t.Errorf("project %v on daemon-a: builds %d farm %d unclear %d last %v; daemon %+v", p.ProjectKey, p.Builds, p.Farm, p.Unclear, p.Last, a)
	}
	if none, _ := s.DaemonCounts(ctx, time.Time{}, ProjectsIn{}); none["daemon-a"].Projects != nil || none["daemon-a"].ProjectCount != 1 {
		t.Errorf("no projects asked for: %+v", none["daemon-a"])
	}
	if one, _ := s.DaemonCounts(ctx, time.Time{}, ProjectsIn{Only: key}); len(one["daemon-a"].Projects) != 1 {
		t.Errorf("the one project asked for: %+v", one["daemon-a"])
	}
	if other, _ := s.DaemonCounts(ctx, time.Time{}, ProjectsIn{Only: ProjectKey{Instance: key.Instance, ID: 43}}); other["daemon-a"].Projects != nil {
		t.Errorf("another project's builds listed: %+v", other["daemon-a"])
	}
}

func mustBuilds(t *testing.T, s *Store, q BuildQuery) []BuildRow {
	t.Helper()
	rows, err := s.Builds(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestFinished(t *testing.T) {
	for st, want := range map[string]bool{"success": true, "failed": true, "canceled": true, "skipped": true,
		"running": false, "pending": false, "manual": false, "created": false, "": false} {
		if Finished(st) != want {
			t.Errorf("%q: %v", st, !want)
		}
	}
}

// A pipeline whose jobs built no image is known from its jobs, timed by
// them, and kept as long as builds are.
func TestPipelineWithoutBuilds(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	for _, b := range bs {
		if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	inst := history.InstancePrefix("https://gitlab.example.com/")
	start := bs[len(bs)-1].Started.Add(time.Hour).Truncate(time.Millisecond)
	for i, name := range []string{"compile", "deploy"} {
		err := s.SaveJob(ctx, Job{Instance: inst, JobID: 5001 + uint64(i), PipelineID: 5000, ProjectID: 42, ProjectPath: "example/app",
			Name: name, Stage: "build", CommitTitle: "deploy only", StartedAt: start.Add(time.Duration(i) * time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
	}
	err := s.SaveAPIPipeline(ctx, APIPipeline{Instance: inst, ID: 5000, ProjectID: 42, Status: "success", Read: start.Add(time.Hour),
		Jobs: []APIJob{
			{ID: 5001, Name: "compile", Status: "success", Started: start, Finished: start.Add(30 * time.Second)},
			{ID: 5002, Name: "deploy", Status: "success", Started: start.Add(time.Minute), Finished: start.Add(3 * time.Minute)},
			{ID: 5003, Name: "deploy-production", Status: "manual"},
		}})
	if err != nil {
		t.Fatal(err)
	}

	// Its jobs are known; it has no builds to list.
	if rows := mustBuilds(t, s, BuildQuery{Project: ProjectKey{Instance: inst, ID: 42}, Pipeline: 5000}); len(rows) != 0 {
		t.Errorf("builds of a pipeline that built nothing: %d", len(rows))
	}
	if n, err := s.PipelineJobCount(ctx, inst, 5000); err != nil || n != 3 {
		t.Errorf("its jobs: %d %v", n, err)
	}
	// The console explains image builds: a project that only ever deployed
	// is not one of its projects.
	if err := s.SaveJob(ctx, Job{Instance: inst, JobID: 9901, PipelineID: 9900, ProjectID: 99, ProjectPath: "example/deploy-only", Name: "deploy", StartedAt: start}); err != nil {
		t.Fatal(err)
	}
	prs, err := s.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range prs {
		if pr.ID == 99 {
			t.Errorf("a project without builds is listed: %+v", pr)
		}
	}
	p, err := s.PipelineInfo(ctx, inst, 5000)
	if err != nil || p == nil || p.Builds != 0 || p.Status != "success" || !p.Started.Equal(start) || !p.Finished.Equal(start.Add(3*time.Minute)) {
		t.Fatalf("pipeline: %+v %v", p, err)
	}
	if p, err := s.PipelineInfo(ctx, inst, 4999); p != nil || err != nil {
		t.Errorf("an unknown pipeline: %+v %v", p, err)
	}
	if j, err := s.Job(ctx, inst, 5002); err != nil || j == nil || j.ProjectID != 42 || j.Status != "success" || j.Name != "deploy" {
		t.Errorf("job 5002: %+v %v", j, err)
	}
	// GitLab last said running; the pod ended since: not known how it went.
	if err := s.SaveAPIPipeline(ctx, APIPipeline{Instance: inst, ID: 5000, ProjectID: 42, Status: "running", Read: start,
		Jobs: []APIJob{{ID: 5004, Name: "late", Status: "running", Started: start.Add(4 * time.Minute)}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.JobEnded(ctx, inst, 5004, start.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if j, err := s.Job(ctx, inst, 5004); err != nil || j == nil || j.Status != "" || j.Finished.IsZero() {
		t.Errorf("a job GitLab said ran, its pod gone: %+v %v", j, err)
	}
	// A manual job GitLab last called manual, run by hand since.
	if err := s.JobEnded(ctx, inst, 5003, start.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if j, err := s.Job(ctx, inst, 5003); err != nil || j == nil || j.Status != "" {
		t.Errorf("a manual job whose pod ended: %+v %v", j, err)
	}
	if j, err := s.Job(ctx, inst, 1); j != nil || err != nil {
		t.Errorf("an unknown job: %+v %v", j, err)
	}
	if err := s.SaveJob(ctx, Job{Instance: inst, JobID: 4002, PipelineID: 4000, ProjectID: 42, Name: "deploy", StartedAt: start.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	// 90 days later everything of it is gone, the manual job too.
	c.t = start.Add(91 * 24 * time.Hour)
	if _, err := s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow("SELECT (SELECT count(*) FROM jobs) + (SELECT count(*) FROM pipelines)").Scan(&n); err != nil || n != 0 {
		t.Errorf("after retention: %d rows of jobs and pipelines left, %v", n, err)
	}
}
