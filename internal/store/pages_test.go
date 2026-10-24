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
	// Pipeline 104 ran three jobs at once (api, worker, test).
	inst := bs[0].Attribution.Instance
	p104, err := s.PipelineInfo(ctx, inst, 104)
	if err != nil || p104 == nil {
		t.Fatalf("pipeline 104: %v", err)
	}
	if p104.Builds != 3 || p104.ProjectPath != "example/app" || p104.CommitTitle != "a change" || p104.Started.IsZero() || p104.Rebuilt == 0 {
		t.Errorf("pipeline 104: %+v", p104)
	}
	if pl, err := s.PipelineInfo(ctx, inst, 999); pl != nil || err != nil {
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
	// Each run names the step's index in its own build: builds differ in
	// their steps, so the same step sits at other indexes.
	for _, r := range runs {
		other, err := s.BuildPage(ctx, r.BuildID)
		if err != nil {
			t.Fatal(err)
		}
		if r.Idx < 0 || r.Idx >= len(other.Build.Steps) || other.Build.Steps[r.Idx].Key != api.Steps[idx].Key {
			t.Errorf("run in build %d: index %d does not name step %q (%d steps)", r.BuildID, r.Idx, api.Steps[idx].Key, len(other.Build.Steps))
		}
	}
	// A later build with one more step in front holds the same step one
	// index further: its run says so.
	grown := *api
	grown.Ref, grown.Started = "grownref", api.Started.Add(48*time.Hour)
	grown.Steps = append([]history.Step{{Key: "[extra 1/1]", Name: "RUN echo extra", State: history.Built}}, api.Steps...)
	gid, err := s.SaveBuild(ctx, Build{Build: &grown, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	runs, err = s.StepHistory(ctx, ids[api.Ref], idx, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range runs {
		if r.BuildID == gid {
			found = true
			if r.Idx != idx+1 {
				t.Errorf("the grown build holds the step at %d, its run says %d", idx+1, r.Idx)
			}
		}
	}
	if !found {
		t.Errorf("the grown build %d is not in the step's history", gid)
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
