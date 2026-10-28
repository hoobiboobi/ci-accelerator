package store

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// noTargetBuilds saves builds of one project made from an attributed
// fixture: target "" unless given, each pushing the names given (one
// exporter attribute, as the CI component joins them with commas), a
// minute apart, numbered from first (ref img-<n>, job 8000+n).
func noTargetBuilds(t *testing.T, s *Store, first int, specs ...[2]string) []*history.Build {
	t.Helper()
	var base *history.Build
	for _, b := range fixtures(t, s.SecretKey()) {
		if b.Attribution.State == history.Attributed {
			base = b
		}
	}
	if base == nil {
		t.Fatal("no attributed fixture")
	}
	var out []*history.Build
	for i, sp := range specs {
		i += first - 1
		b := *base
		b.Ref, b.Target = fmt.Sprintf("img-%d", i+1), sp[0]
		b.Attribution.Pipeline, b.Attribution.Job = uint64(7001+i), uint64(8001+i)
		b.Started = base.Started.Add(time.Duration(i+1) * time.Minute)
		b.Completed = b.Started.Add(time.Second)
		b.Images = nil
		if sp[1] != "" {
			b.Images = []string{sp[1]}
		}
		out = append(out, &b)
	}
	saveJobs(t, s, out)
	for _, b := range out {
		if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// The CI component pushes "<repo>:<commit>,<repo>:latest" in one
// attribute: the previous build of an image without a target is the one
// that pushed the same repository, whatever the tags.
func TestPreviousWithoutTargetJoinedNames(t *testing.T) {
	s := open(t, t.TempDir(), &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)})
	bs := noTargetBuilds(t, s, 1,
		[2]string{"", "registry.example.com:5000/g/api:a1,registry.example.com:5000/g/api:latest"},
		[2]string{"", "registry.example.com:5000/g/worker:b2,registry.example.com:5000/g/worker:latest"},
		[2]string{"", "registry.example.com:5000/g/api:c3,registry.example.com:5000/g/api:latest"},
		[2]string{"", "registry.example.com:5000/g/api@sha256:0123"})
	for _, c := range []struct {
		b    *history.Build
		want string
	}{{bs[2], "img-1"}, {bs[1], ""}, {bs[3], "img-3"}} {
		prev, _, err := s.Previous(ctx, c.b)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if prev != nil {
			got = prev.Build.Ref
		}
		if got != c.want {
			t.Errorf("previous of %s (%v): %q, want %q", c.b.Ref, c.b.Images, got, c.want)
		}
	}
	if next, err := s.Next(ctx, bs[0]); err != nil || next == nil || next.Build.Ref != "img-3" {
		t.Errorf("next of img-1: %v, %v (want img-3)", next, err)
	}
}

// Builds without a target: the NoTarget filter, the images a target's
// builds pushed, and the names read back on each row.
func TestNoTargetQueries(t *testing.T) {
	s := open(t, t.TempDir(), &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)})
	bs := noTargetBuilds(t, s, 1,
		[2]string{"", "reg/g/api:1,reg/g/api:latest"},
		[2]string{"app", "reg/g/app:1"},
		[2]string{"", ""},
		[2]string{"", "reg/g/worker:2"},
		[2]string{"app", ""},
		[2]string{"", "reg/g/api:3"},
		[2]string{"tools", ""})
	p := ProjectKey{Instance: bs[0].Attribution.Instance, ID: 42}
	ids := func(q BuildQuery) []string {
		t.Helper()
		rows, err := s.Builds(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, fmt.Sprintf("img-%d", r.Job-8000))
		}
		return out
	}
	if got := ids(BuildQuery{Project: p, NoTarget: true}); !reflect.DeepEqual(got, []string{"img-6", "img-4", "img-3", "img-1"}) {
		t.Errorf("no target: %v", got)
	}
	if got := ids(BuildQuery{Project: p, Target: "app"}); !reflect.DeepEqual(got, []string{"img-5", "img-2"}) {
		t.Errorf("target app: %v", got)
	}
	if got := ids(BuildQuery{Project: p}); len(got) != 7 {
		t.Errorf("no filter: %v", got)
	}

	rows, err := s.Builds(ctx, BuildQuery{Project: p, NoTarget: true})
	if err != nil {
		t.Fatal(err)
	}
	images := map[string][]string{}
	for _, r := range rows {
		images[fmt.Sprintf("img-%d", r.Job-8000)] = r.Images
	}
	if !reflect.DeepEqual(images["img-1"], []string{"reg/g/api:1,reg/g/api:latest"}) || len(images["img-3"]) != 0 ||
		!reflect.DeepEqual(images["img-6"], []string{"reg/g/api:3"}) {
		t.Errorf("images read back: %q", images)
	}

	got, err := s.TargetImages(ctx, p, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"": {"reg/g/api", "reg/g/worker"}, "app": {"reg/g/app"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TargetImages: %q, want %q", got, want)
	}
	// Newest first: once worker's build is the latest, it leads.
	noTargetBuilds(t, s, 8, [2]string{"", "reg/g/worker:9"})
	if got, _ := s.TargetImages(ctx, p, time.Time{}); !reflect.DeepEqual(got[""], []string{"reg/g/worker", "reg/g/api"}) {
		t.Errorf("TargetImages after a newer worker build: %q", got[""])
	}
}
