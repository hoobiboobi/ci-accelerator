package store

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// Writers tell subscribers which pipelines changed: a saved job, a saved
// build and a new reason. Changes between two reads are merged.
func TestChanges(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	sub := s.Subscribe()
	idle := func(what string) {
		t.Helper()
		select {
		case <-sub.C:
			t.Errorf("%s: a change was announced: %v", what, sub.Take())
		default:
		}
	}
	got := func(what string, want ...string) {
		t.Helper()
		select {
		case <-sub.C:
		default:
			t.Fatalf("%s: no change announced", what)
		}
		if keys := sub.Take(); !reflect.DeepEqual(keys, want) {
			t.Errorf("%s: %v, want %v", what, keys, want)
		}
	}
	var attributed, other *history.Build
	for _, b := range bs {
		if b.Attribution.State != history.Attributed {
			if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1}); err != nil {
				t.Fatal(err)
			}
			idle("a build without a pipeline")
		} else if attributed == nil {
			attributed = b
		} else if other == nil && b.Attribution.Pipeline != attributed.Attribution.Pipeline {
			other = b
		}
	}
	a := attributed.Attribution
	key := fmt.Sprintf("%s/%d", a.Instance, a.Pipeline)
	if PipelineKey(a.Instance, a.Pipeline) != key || len(a.Instance) != 6 {
		t.Fatalf("key %q", PipelineKey(a.Instance, a.Pipeline))
	}

	if _, err := s.SaveBuild(ctx, Build{Build: attributed, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	got("saved build", key)
	if _, err := s.SaveBuild(ctx, Build{Build: attributed, Generation: 0}); err != nil {
		t.Fatal(err)
	}
	idle("an older generation, not stored")

	if err := s.SetReason(ctx, attributed.Ref, attributed.Daemon, history.Reason{Kind: history.FirstBuild}); err != nil {
		t.Fatal(err)
	}
	got("new reason", key)
	if err := s.SetReason(ctx, "no-such-ref", "daemon-a", history.Reason{Kind: history.FirstBuild}); err != nil {
		t.Fatal(err)
	}
	idle("reason of an unknown build")

	// Two writes before the subscriber reads: one signal, both pipelines.
	o := other.Attribution
	if err := s.SaveJob(ctx, Job{Instance: a.Instance, JobID: a.Job, PipelineID: a.Pipeline, Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveBuild(ctx, Build{Build: other, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	got("job and another pipeline's build", key, PipelineKey(o.Instance, o.Pipeline))
	idle("after Take")

	s.Unsubscribe(sub)
	if err := s.SaveJob(ctx, Job{Instance: a.Instance, JobID: a.Job, PipelineID: a.Pipeline, Name: "y"}); err != nil {
		t.Fatal(err)
	}
	idle("after Unsubscribe")
}
