package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// Two Dockerfiles without a target in one project (services/api and
// services/worker, each pushing its own repository) are two images: a
// build is compared with the previous build of its own repository, not
// with the other Dockerfile's latest build.
func TestPreviousWithoutTargetByRepository(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	var base *history.Build
	for _, b := range bs {
		if b.Attribution.State == history.Attributed {
			base = b
		}
	}
	if base == nil {
		t.Fatal("no attributed fixture")
	}
	mk := func(i int, image string) *history.Build {
		b := *base
		b.Ref, b.Target = fmt.Sprintf("img-%d", i), ""
		b.Attribution.Pipeline, b.Attribution.Job = uint64(7000+i), uint64(8000+i)
		b.Started = base.Started.Add(time.Duration(i) * time.Minute)
		b.Completed = b.Started.Add(time.Second)
		b.Images = nil
		if image != "" {
			b.Images = []string{image}
		}
		return &b
	}
	api1, worker, api2, local1, local2 := mk(1, "registry.example.com/g/app/api:a"), mk(2, "registry.example.com/g/app/worker:b"),
		mk(3, "registry.example.com/g/app/api:c"), mk(4, ""), mk(5, "")
	all := []*history.Build{api1, worker, api2, local1, local2}
	saveJobs(t, s, all)
	for _, b := range all {
		if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		b    *history.Build
		want string
	}{{api2, "img-1"}, {worker, ""}, {api1, ""}, {local2, "img-4"}, {local1, ""}} {
		prev, known, err := s.Previous(ctx, c.b)
		if err != nil || !known {
			t.Fatalf("%s: %v, known %v", c.b.Ref, err, known)
		}
		got := ""
		if prev != nil {
			got = prev.Build.Ref
		}
		if got != c.want {
			t.Errorf("previous of %s (%v): %q, want %q", c.b.Ref, c.b.Images, got, c.want)
		}
	}
	next, err := s.Next(ctx, api1)
	if err != nil || len(next) != 1 || next[0].Build.Ref != "img-3" {
		t.Errorf("next of img-1: %v, %v (want img-3, not the worker's img-2)", next, err)
	}
}

// A rebuild of an image without a target whose cache was on another daemon
// is settled against the commit of that image's previous build, the one
// its reason compared it with: not the commit of the project's other
// Dockerfile built in between (its comparison would miss the image's own
// changes, or find "the same commit" and blame the farm).
func TestElsewhereWithoutTargetComparesTheSameImage(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	var base *history.Build
	for _, b := range bs {
		if b.Attribution.State == history.Attributed {
			base = b
		}
	}
	if base == nil {
		t.Fatal("no attributed fixture")
	}
	var step history.Step
	for _, st := range base.Steps {
		if st.State == history.Built && st.StepID != "" {
			step = st
		}
	}
	mk := func(i int, image, sha string) *history.Build {
		b := *base
		b.Ref, b.Target = fmt.Sprintf("img-%d", i), ""
		b.Attribution.Pipeline, b.Attribution.Job = uint64(7000+i), uint64(8000+i)
		b.Started = base.Started.Add(time.Duration(i) * time.Minute)
		b.Completed = b.Started.Add(time.Second)
		b.Images = []string{image}
		if err := s.SaveJob(ctx, Job{Instance: b.Attribution.Instance, JobID: b.Attribution.Job, PipelineID: b.Attribution.Pipeline,
			ProjectID: 42, ProjectPath: "example/app", Name: "build", Stage: "build", SHA: sha}); err != nil {
			t.Fatal(err)
		}
		return &b
	}
	api1 := mk(1, "registry.example.com/g/app/api:a", "aaaaaaaa")
	worker1 := mk(2, "registry.example.com/g/app/worker:b", "bbbbbbbb")
	worker2 := mk(3, "registry.example.com/g/app/worker:c", "cccccccc")
	api2 := mk(4, "registry.example.com/g/app/api:c", "cccccccc")
	for _, b := range []*history.Build{api1, worker1, worker2} {
		if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	id, err := s.SaveBuild(ctx, Build{Build: api2, Generation: 1,
		Reason: history.Reason{Kind: history.CacheElsewhere, Step: step, Old: "bk-x", Paths: []string{"services/api"}}})
	if err != nil {
		t.Fatal(err)
	}
	if prev, _, err := s.Previous(ctx, api2); err != nil || prev == nil || prev.Build.Ref != api1.Ref {
		t.Fatalf("previous of %s: %v, %v", api2.Ref, prev, err)
	}
	open, err := s.Elsewhere(ctx, ProjectKey{Instance: api2.Attribution.Instance, ID: 42}, time.Time{}, 10)
	if err != nil || len(open) != 1 || open[0].ID != id {
		t.Fatalf("elsewhere: %+v, %v", open, err)
	}
	if e := open[0]; e.SHA != "cccccccc" || e.PrevSHA != "aaaaaaaa" {
		t.Errorf("elsewhere compares %s..%s, want aaaaaaaa..cccccccc (the api image's previous build)", e.PrevSHA, e.SHA)
	}
}

// An image built rarely in a project whose other Dockerfiles without a
// target build all the time keeps its previous build: hundreds of the
// other images' builds since then do not hide it (it would read "first
// build of this image", a change of the user's).
func TestPreviousWithoutTargetBehindManyOthers(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	var base *history.Build
	for _, b := range bs {
		if b.Attribution.State == history.Attributed {
			base = b
		}
	}
	if base == nil {
		t.Fatal("no attributed fixture")
	}
	mk := func(i int, image string) *history.Build {
		b := *base
		b.Ref, b.Target = fmt.Sprintf("img-%d", i), ""
		b.Attribution.Pipeline, b.Attribution.Job = uint64(7000+i), uint64(8000+i)
		b.Started = base.Started.Add(time.Duration(i) * time.Second)
		b.Completed = b.Started.Add(time.Second)
		b.Images = []string{image}
		b.Steps = b.Steps[:1]
		return &b
	}
	api1 := mk(0, "registry.example.com/g/app/api:a")
	all := []*history.Build{api1}
	for i := 1; i <= 501; i++ {
		all = append(all, mk(i, fmt.Sprintf("registry.example.com/g/app/worker:%d", i)))
	}
	api2 := mk(502, "registry.example.com/g/app/api:b")
	all = append(all, api2)
	saveJobs(t, s, all)
	for _, b := range all {
		if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	prev, known, err := s.Previous(ctx, api2)
	if err != nil || !known || prev == nil || prev.Build.Ref != api1.Ref {
		t.Fatalf("previous of %s: %v (known %v, err %v), want %s", api2.Ref, prev, known, err, api1.Ref)
	}
	next, err := s.Next(ctx, api1)
	if err != nil || len(next) != 1 || next[0].Build.Ref != api2.Ref {
		t.Fatalf("next of %s: %v (err %v), want %s", api1.Ref, next, err, api2.Ref)
	}
}
