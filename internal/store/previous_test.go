package store

import (
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// A build of the same pipeline (a parallel job or a retry building the
// same target, same commit) is never "the previous build": comparing with
// one makes the commit's own work look like "nothing changed" and the
// same-commit shortcut then blames the farm for the commit's rebuild.
func TestPreviousSkipsTheOwnPipeline(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	var older, cur *history.Build
	for _, b := range bs {
		if b.Target != "api" || b.Attribution.State != history.Attributed {
			continue
		}
		if older == nil {
			older = b
		} else if cur == nil || b.Started.After(cur.Started) {
			cur = b
		}
	}
	if older == nil || cur == nil {
		t.Fatal("fixtures hold fewer than two attributed api builds")
	}
	// A concurrent sibling of cur's pipeline, started just before cur, and
	// a retry-like one just after it.
	before, after := *cur, *cur
	before.Ref, after.Ref = "sib-before-"+cur.Ref, "sib-after-"+cur.Ref
	before.Started, after.Started = cur.Started.Add(-time.Second), cur.Started.Add(time.Second)
	before.Attribution.Job, after.Attribution.Job = cur.Attribution.Job+500000, cur.Attribution.Job+500001
	for _, sib := range []*history.Build{&before, &after} {
		if err := s.SaveJob(ctx, Job{Instance: sib.Attribution.Instance, JobID: sib.Attribution.Job,
			PipelineID: sib.Attribution.Pipeline, ProjectID: 42, ProjectPath: "example/app", Name: "retry", Stage: "build"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []*history.Build{older, &before, &after} {
		if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	prev, known, err := s.Previous(ctx, cur)
	if err != nil || !known {
		t.Fatalf("previous: known %v, err %v", known, err)
	}
	if prev == nil || prev.Build.Ref != older.Ref {
		got := "<none>"
		if prev != nil {
			got = prev.Build.Ref
		}
		t.Fatalf("previous = %s, want %s (siblings of the same pipeline must not count)", got, older.Ref)
	}
	// Next skips the own pipeline symmetrically: cur's next must not be
	// the sibling that started a second later.
	next, err := s.Next(ctx, cur)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range next {
		if n.Build.Attribution.Pipeline == cur.Attribution.Pipeline {
			t.Fatalf("next = %s of cur's own pipeline", n.Build.Ref)
		}
	}
}

// A failed or canceled build is never "the previous build": BuildKit
// records no provenance for it, so every step of the next build would look
// new ("Dockerfile changed", a change of the user's). The next build is
// compared with the last one that succeeded.
func TestPreviousSkipsFailedBuilds(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	for _, b := range bs {
		if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	// A rebuild of the api target with a previous build.
	var (
		cur  *history.Build
		good *Stored
		want history.Reason
	)
	for _, b := range bs {
		if b.Target != "api" || b.Attribution.State != history.Attributed || b.Error != "" {
			continue
		}
		p, _, err := s.Previous(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		if p == nil {
			continue
		}
		if r, ok := history.FirstRebuild(p.Build, b); ok {
			cur, good, want = b, p, r
			break
		}
	}
	if cur == nil {
		t.Fatal("fixtures: no rebuilt api build with a previous build")
	}
	// The pipeline in between failed: no images, no provenance (no step
	// identities), an error.
	failed := *good.Build
	failed.Ref, failed.Error, failed.Images = "failed-"+good.Build.Ref, "process \"/bin/sh -c npm test\" did not complete successfully: exit code: 1", nil
	failed.Started = good.Build.Started.Add(cur.Started.Sub(good.Build.Started) / 2)
	failed.Completed = failed.Started.Add(time.Second)
	failed.Attribution.Pipeline, failed.Attribution.Job = good.Build.Attribution.Pipeline+900000, good.Build.Attribution.Job+900000
	failed.Steps = append([]history.Step(nil), good.Build.Steps...)
	for i := range failed.Steps {
		failed.Steps[i].Identity, failed.Steps[i].StepID = "", ""
	}
	if err := s.SaveJob(ctx, Job{Instance: failed.Attribution.Instance, JobID: failed.Attribution.Job,
		PipelineID: failed.Attribution.Pipeline, ProjectID: 42, ProjectPath: "example/app", Name: "build-api", Stage: "build"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveBuild(ctx, Build{Build: &failed, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	prev, _, err := s.Previous(ctx, cur)
	if err != nil || prev == nil || prev.Build.Ref != good.Build.Ref {
		got := "<none>"
		if prev != nil {
			got = prev.Build.Ref
		}
		t.Fatalf("previous of %s = %s (%v), want %s, not the failed build", cur.Ref, got, err, good.Build.Ref)
	}
	if got, _ := history.FirstRebuild(prev.Build, cur); got.Kind != want.Kind || got.Step.Key != want.Step.Key {
		t.Errorf("reason %s on %s, want %s on %s", got.Kind, got.Step.Key, want.Kind, want.Step.Key)
	}
	// The failed build and the next that succeeded are compared with the
	// good one, and nothing after that.
	next, err := s.Next(ctx, good.Build)
	if err != nil || len(next) < 2 || next[0].Build.Ref != failed.Ref || next[len(next)-1].Build.Error != "" {
		t.Errorf("next of %s: %d builds (%v), want the failed build, then one that did not fail", good.Build.Ref, len(next), err)
	}
	for _, n := range next[:max(len(next)-1, 0)] {
		if n.Build.Error == "" {
			t.Errorf("next of %s goes on after %s, which did not fail", good.Build.Ref, n.Build.Ref)
		}
	}
	if next, err := s.Next(ctx, &failed); err != nil || len(next) != 0 {
		t.Errorf("next of the failed build: %d builds (%v)", len(next), err)
	}
}
