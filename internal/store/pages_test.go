package store

import (
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

func TestPages(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	ids := map[string]int64{}
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
		id, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1, Reason: r})
		if err != nil {
			t.Fatal(err)
		}
		ids[b.Ref] = id
	}
	ps, err := s.Pipelines(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	pipelines := map[uint64]Pipeline{}
	for _, p := range ps {
		pipelines[p.ID] = p
	}
	// Pipeline 104 ran three jobs at once (api, worker, test).
	p104 := pipelines[104]
	if p104.Builds != 3 || p104.ProjectPath != "example/app" || p104.CommitTitle != "a change" || p104.Started.IsZero() || p104.Rebuilt == 0 {
		t.Errorf("pipeline 104: %+v", p104)
	}
	if len(ps) < 8 || ps[0].Started.Before(ps[len(ps)-1].Started) {
		t.Errorf("%d pipelines, newest first expected", len(ps))
	}
	pl, jobs, builds, err := s.PipelinePage(ctx, p104.Instance, 104)
	if err != nil || pl == nil || len(jobs) != 3 || len(builds) != 3 {
		t.Fatalf("pipeline page: %+v, %d jobs, %d builds, %v", pl, len(jobs), len(builds), err)
	}
	if pl.Rebuilt != p104.Rebuilt || pl.Builds != p104.Builds {
		t.Errorf("pipeline page counts %d/%d, list %d/%d", pl.Rebuilt, pl.Builds, p104.Rebuilt, p104.Builds)
	}
	if pl, _, _, err := s.PipelinePage(ctx, p104.Instance, 999); pl != nil || err != nil {
		t.Errorf("unknown pipeline: %+v, %v", pl, err)
	}

	api := byRef(bs, "api", 1001)
	page, err := s.BuildPage(ctx, ids[api.Ref])
	if err != nil {
		t.Fatal(err)
	}
	name, text := api.Dockerfile()
	if page.Dockerfile != text || page.DockerfileName != name || page.Job == nil || page.Job.ID != 1001 || page.Pipeline == nil || !page.OutputKept {
		t.Errorf("build page: job %+v pipeline %+v dockerfile %d bytes, output kept %v", page.Job, page.Pipeline, len(page.Dockerfile), page.OutputKept)
	}
	if _, err := s.BuildPage(ctx, 99999); err != ErrNotFound {
		t.Errorf("unknown build: %v", err)
	}

	// The dependency step of job 1001's api build ran in many builds since.
	idx := -1
	for i, st := range api.Steps {
		if st.Key == "[deps 6/6]" {
			idx = i
		}
	}
	runs, err := s.StepHistory(ctx, ids[api.Ref], idx, 50)
	if err != nil {
		t.Fatal(err)
	}
	built, cached := 0, 0
	for _, r := range runs {
		switch r.State {
		case history.Built:
			built++
		case history.Cached:
			cached++
		}
	}
	if built == 0 || cached == 0 || runs[0].Started.Before(runs[len(runs)-1].Started) {
		t.Errorf("step history: %d runs, %d built, %d cached", len(runs), built, cached)
	}
}

func byRef(bs []*history.Build, target string, job uint64) *history.Build {
	for _, b := range bs {
		if b.Target == target && b.Attribution.Job == job {
			return b
		}
	}
	return nil
}
