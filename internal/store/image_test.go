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
	if err != nil || next == nil || next.Build.Ref != "img-3" {
		t.Errorf("next of img-1: %v, %v (want img-3, not the worker's img-2)", next, err)
	}
}
