package store

import (
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

var allReasonKinds = []history.ReasonKind{
	history.FirstBuild, history.StepChanged, history.NewStep, history.ArgChanged,
	history.BaseImageChanged, history.FilesChanged, history.NotInCache,
	history.CacheElsewhere, history.OtherNode,
}

// One definition of "the farm's fault" (history.FarmKinds): the Nodes
// counter and the outcome filter must classify EVERY reason kind exactly
// as that list says, or the Overview and the Nodes page drift apart (the
// audit's CF-03: "Build farm 3 builds" against "Rebuilt for want of cache
// here: 0").
func TestFarmCountersFollowFarmKinds(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	var donor *history.Build
	for _, b := range bs {
		if b.Attribution.State == history.Attributed && b.Error == "" {
			donor = b
			break
		}
	}
	if donor == nil {
		t.Fatal("no clean attributed fixture build")
	}
	farm := map[history.ReasonKind]bool{}
	for _, k := range history.FarmKinds() {
		farm[k] = true
	}
	for i, k := range allReasonKinds {
		clone := *donor
		clone.Ref = "farm-test-" + string(k)
		clone.Started = donor.Started.Add(time.Duration(i+1) * time.Minute)
		clone.Completed = clone.Started.Add(time.Second)
		clone.Attribution.Job = donor.Attribution.Job + 900000 + uint64(i)
		clone.Attribution.Pipeline = donor.Attribution.Pipeline + 900000 + uint64(i)
		if err := s.SaveJob(ctx, Job{Instance: clone.Attribution.Instance, JobID: clone.Attribution.Job,
			PipelineID: clone.Attribution.Pipeline, ProjectID: 42, ProjectPath: "example/app", Name: "j", Stage: "build"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SaveBuild(ctx, Build{Build: &clone, Generation: 1, Reason: history.Reason{Kind: k}}); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := s.DaemonCounts(ctx, donor.Started, ProjectsIn{All: true})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, dc := range counts {
		total += dc.Farm
	}
	if total != len(history.FarmKinds()) {
		t.Errorf("DaemonCounts farm = %d, want %d (one per history.FarmKinds entry)", total, len(history.FarmKinds()))
	}
	rows, err := s.Builds(ctx, BuildQuery{Project: ProjectKey{Instance: donor.Attribution.Instance, ID: 42},
		Outcome: "farm", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(history.FarmKinds()) {
		t.Errorf("outcome=farm returned %d builds, want %d", len(rows), len(history.FarmKinds()))
	}
	for _, r := range rows {
		if !farm[r.Reason] {
			t.Errorf("outcome=farm returned a build with reason %q", r.Reason)
		}
	}
}
