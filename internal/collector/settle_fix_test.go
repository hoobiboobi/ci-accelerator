package collector

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// overlapSetup stores a branch pipeline (202) whose job on daemon-a
// rebuilt a step (cache elsewhere, its previous build ran on daemon-z)
// while its job on daemon-b found the step cached; with mr, a merge
// request pipeline (201) of the same project executed that step on
// daemon-b seconds before.
func overlapSetup(t *testing.T, mr bool) (*Collector, *store.Store) {
	t.Helper()
	c, s := newCollector(t)
	const inst = "051b90"
	t0 := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	for _, j := range []store.Job{
		{Instance: inst, JobID: 71, PipelineID: 201, ProjectID: 42, Name: "build-mr"},
		{Instance: inst, JobID: 72, PipelineID: 202, ProjectID: 42, Name: "build-a"},
		{Instance: inst, JobID: 73, PipelineID: 202, ProjectID: 42, Name: "build-b"},
	} {
		if err := s.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(ref, daemon string, pipeline, job uint64, state history.StepState, at time.Time) *history.Build {
		return &history.Build{Ref: ref, Daemon: daemon, Target: ref, Context: ".", Started: at, Completed: at.Add(time.Minute),
			Attribution: history.Attribution{State: history.Attributed, Instance: inst, Pipeline: pipeline, Job: job},
			Steps: []history.Step{{Key: "[app 1/2]", Name: "COPY common/ ./common", Identity: "00000000000000aa", State: state,
				Started: at, Completed: at.Add(time.Second)}}}
	}
	if mr {
		if _, err := s.SaveBuild(ctx, store.Build{Build: mk("mr", "daemon-b", 201, 71, history.Built, t0), Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	br := mk("br", "daemon-a", 202, 72, history.Built, t0.Add(20*time.Second))
	elsewhere := history.Reason{Kind: history.CacheElsewhere, Step: br.Steps[0], Old: "daemon-z", New: "daemon-a", Paths: []string{"common"}}
	if _, err := s.SaveBuild(ctx, store.Build{Build: br, Generation: 1, Reason: elsewhere}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveBuild(ctx, store.Build{Build: mk("hit", "daemon-b", 202, 73, history.Cached, t0.Add(30*time.Second)), Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.settle(ctx, inst, 202); err != nil {
		t.Fatal(err)
	}
	return c, s
}

func reasonOf(t *testing.T, s *store.Store, pipeline uint64, ref string) history.Reason {
	t.Helper()
	got, err := s.Pipeline(ctx, "051b90", pipeline)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range got {
		if b.Build.Ref == ref {
			return b.Reason
		}
	}
	t.Fatalf("%s not in pipeline %d", ref, pipeline)
	return history.Reason{}
}

// A cache hit that another pipeline of the project produced on the
// witness's daemon meanwhile proves nothing: the rebuild stays "cache
// elsewhere" instead of becoming the farm's fault. Without that pipeline
// the same builds settle to another node.
func TestSettleSeesOverlappingPipelines(t *testing.T) {
	_, s := overlapSetup(t, false)
	if r := reasonOf(t, s, 202, "br"); r.Kind != history.OtherNode || r.By != "hit" {
		t.Fatalf("control without the other pipeline: %s by %q, want other-node by hit", r.Kind, r.By)
	}
	_, s = overlapSetup(t, true)
	if r := reasonOf(t, s, 202, "br"); r.Kind != history.CacheElsewhere || r.By != "" {
		t.Errorf("with the merge request pipeline on daemon-b: %s by %q, want cache-elsewhere", r.Kind, r.By)
	}
	// The other pipeline's builds are read, never written.
	if r := reasonOf(t, s, 201, "mr"); r.Kind != "" {
		t.Errorf("the other pipeline's build got reason %s", r.Kind)
	}
}

// A record new to the store is named in the log before its blobs are
// read: a record whose reading ends the process is the last one logged.
func TestRecordLoggedBeforeItsRead(t *testing.T) {
	f := recorded(t)
	c, _ := newCollector(t)
	var buf bytes.Buffer
	c.log = slog.New(slog.NewTextHandler(&buf, nil))
	r := f.recs[0]
	// Every blob read fails: the line is there all the same.
	_ = c.Record(ctx, &fake{name: f.name, blobs: map[string][]byte{}}, r)
	out := buf.String()
	if !strings.Contains(out, `level=INFO msg="reading record"`) || !strings.Contains(out, "ref="+r.Ref) || !strings.Contains(out, "daemon="+f.name) {
		t.Errorf("log:\n%s", out)
	}
}
