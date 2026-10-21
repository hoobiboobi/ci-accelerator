package store

import (
	"fmt"
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
	if err != nil || len(projects) != 1 || projects[0].ID != 42 || projects[0].Path != "example/app" || projects[0].Builds < 10 {
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

	list, err := s.List(ctx, key, 100)
	if err != nil || len(list) < 8 {
		t.Fatalf("list: %d %v", len(list), err)
	}
	if other, err := s.List(ctx, ProjectKey{Instance: key.Instance, ID: 7}, 100); err != nil || len(other) != 0 {
		t.Errorf("other project: %d %v", len(other), err)
	}
	var p104 *ListPipeline
	for i := range list {
		if list[i].ID == 104 {
			p104 = &list[i]
		}
	}
	if p104 == nil || len(p104.Jobs) != 3 {
		t.Fatalf("pipeline 104: %+v", p104)
	}
	for _, j := range p104.Jobs {
		if j.Name == "" || j.BuildID == 0 || j.Daemon != "daemon-a" || j.Started.IsZero() || j.Instance != key.Instance || j.PipelineID != 104 {
			t.Errorf("job %+v", j)
		}
	}

	// GitLab's API adds times and jobs from other runners, and keeps what
	// the pods said.
	j0 := p104.Jobs[0]
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
	j1 := p104.Jobs[1]
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
	pl, _, _, err := s.PipelinePage(ctx, key.Instance, 104)
	if err != nil || pl.Status != "success" || pl.WebURL == "" || pl.APIAt.IsZero() || !Finished(pl.Status) {
		t.Errorf("pipeline %+v %v", pl, err)
	}

	// The usual time is the median of earlier successful builds.
	last := bs[len(bs)-1]
	usual, err := s.Usual(ctx, last, 42)
	if err != nil || usual <= 0 {
		t.Errorf("usual %v %v", usual, err)
	}
	if u, _ := s.Usual(ctx, bs[0], 42); u != 0 {
		t.Errorf("first build has a usual time: %v", u)
	}
	if u, _ := s.Usual(ctx, last, 0); u != 0 {
		t.Errorf("unknown project has a usual time: %v", u)
	}

	pb, err := s.ProjectBuilds(ctx, key, 5)
	if err != nil || len(pb) != 5 || !pb[0].Build.Started.Before(pb[4].Build.Started) || len(pb[4].Build.Steps) == 0 || pb[4].Build.Ref != last.Ref {
		t.Fatalf("project builds %d %v", len(pb), err)
	}
	counts, err := s.DaemonCounts(ctx)
	if err != nil || counts["daemon-a"].Builds != len(bs) || counts["daemon-a"].Last.IsZero() {
		t.Errorf("daemon counts %+v %v", counts, err)
	}
}

func TestFinished(t *testing.T) {
	for st, want := range map[string]bool{"success": true, "failed": true, "canceled": true, "skipped": true,
		"running": false, "pending": false, "manual": false, "created": false, "": false} {
		if Finished(st) != want {
			t.Errorf("%q: %v", st, !want)
		}
	}
}

// The usual time is the median of the last ten earlier builds of the
// target, not of the first ten.
func TestUsualIsRecent(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := savedFixtures(t, s)
	var base *history.Build
	for _, b := range bs {
		if b.Attribution.State == history.Attributed && b.Error == "" {
			base = b
		}
	}
	start := base.Started.Add(24 * time.Hour)
	var last *history.Build
	for i := 0; i <= 12; i++ {
		x := *base
		x.Ref = fmt.Sprintf("usual-%d", i)
		x.Started = start.Add(time.Duration(i) * time.Hour)
		x.Completed = x.Started.Add(time.Duration(i+1) * time.Second)
		if _, err := s.SaveBuild(ctx, Build{Build: &x, Generation: 1}); err != nil {
			t.Fatal(err)
		}
		last = &x
	}
	// Builds 2..11 before the last: 3..12 s, median 8 s.
	if u, err := s.Usual(ctx, last, 42); err != nil || u != 8*time.Second {
		t.Errorf("usual %v %v, want 8s", u, err)
	}
}
