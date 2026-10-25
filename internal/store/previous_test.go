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
	if next != nil && next.Build.Attribution.Pipeline == cur.Attribution.Pipeline {
		t.Fatalf("next = %s of cur's own pipeline", next.Build.Ref)
	}
}
