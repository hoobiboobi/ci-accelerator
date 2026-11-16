package collector

import (
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// The e2e's box_settle: pipeline 100 builds the step on daemon-a; in
// pipeline 200 the same files are rebuilt on daemon-b (cold) and then
// found in daemon-a's cache. The previous pipeline's execution on daemon-a
// is the cache the witness found, not the change's product: the rebuild
// on daemon-b is the node's, proven by the witness. A build of another
// pipeline that executed the step on daemon-a after the previous build
// (a merge request's pipeline of the same commit) still makes the witness
// prove nothing.
func TestSettleWitnessOfThePreviousPipelinesCache(t *testing.T) {
	for _, mr := range []bool{false, true} {
		c, s := newCollector(t)
		const inst = "051b90"
		t0 := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
		jobs := []store.Job{
			{Instance: inst, JobID: 101, PipelineID: 100, ProjectID: 42, Name: "build"},
			{Instance: inst, JobID: 201, PipelineID: 200, ProjectID: 42, Name: "build"},
			{Instance: inst, JobID: 202, PipelineID: 200, ProjectID: 42, Name: "build-again"},
			{Instance: inst, JobID: 301, PipelineID: 300, ProjectID: 42, Name: "build-mr"},
		}
		for _, j := range jobs {
			if err := s.SaveJob(ctx, j); err != nil {
				t.Fatal(err)
			}
		}
		mk := func(ref, daemon string, pipeline, job uint64, state history.StepState, at time.Time) *history.Build {
			return &history.Build{Ref: ref, Daemon: daemon, Target: "app", Context: ".", Started: at, Completed: at.Add(time.Minute),
				Attribution: history.Attribution{State: history.Attributed, Instance: inst, Pipeline: pipeline, Job: job},
				Steps: []history.Step{{Key: "[app 1/2]", Name: "COPY files/ /files/", Identity: "00000000000000aa", State: state,
					Started: at, Completed: at.Add(time.Second)}}}
		}
		save := func(b *history.Build, r history.Reason) {
			if _, err := s.SaveBuild(ctx, store.Build{Build: b, Generation: 1, Reason: r}); err != nil {
				t.Fatal(err)
			}
		}
		save(mk("j101", "daemon-a", 100, 101, history.Built, t0), history.Reason{Kind: history.FirstBuild})
		if mr {
			// Another image of the project that copies the same files.
			other := mk("j301", "daemon-a", 300, 301, history.Built, t0.Add(2*time.Minute))
			other.Target = "app-mr"
			save(other, history.Reason{})
		}
		cold := mk("j201", "daemon-b", 200, 201, history.Built, t0.Add(3*time.Minute))
		save(cold, history.Reason{Kind: history.CacheElsewhere, Step: cold.Steps[0], Old: "daemon-a", New: "daemon-b", Paths: []string{"files"}})
		save(mk("j202", "daemon-a", 200, 202, history.Cached, t0.Add(5*time.Minute)), history.Reason{})
		if err := c.settle(ctx, inst, 200); err != nil {
			t.Fatal(err)
		}
		r := reasonOf(t, s, 200, "j201")
		switch {
		case !mr && (r.Kind != history.OtherNode || r.By != "j202"):
			t.Errorf("after the previous pipeline: %s by %q, want other-node by j202", r.Kind, r.By)
		case mr && (r.Kind != history.CacheElsewhere || r.By != ""):
			t.Errorf("with another pipeline's execution after the previous build: %s by %q, want cache-elsewhere", r.Kind, r.By)
		}
	}
}
