package history

import (
	"strings"
	"testing"
	"time"
)

// The fixtures are the builds of test/history/generate.sh, one per CI job:
// pipeline 100 cold, 101 a build argument, 102 a source file, 103 the
// dependencies, 104 three jobs at once sharing the "deps" stage, 105 a
// changed step, 106 another base image, 107 a failing build, then a build
// without trace context and two builds in one job (pipeline 108).

func byJob(t *testing.T, bs []*Build, job uint64, target string) *Build {
	t.Helper()
	for _, b := range bs {
		if b.Attribution.Job == job && b.Target == target {
			return b
		}
	}
	t.Fatalf("no build of job %d, target %s", job, target)
	return nil
}

func step(t *testing.T, b *Build, key string) Step {
	t.Helper()
	for _, s := range b.Steps {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("%s: no step %s", b.Ref, key)
	return Step{}
}

func TestEveryRecordDecodes(t *testing.T) {
	bs := loadFixtures(t)
	if len(bs) != 13 {
		t.Fatalf("got %d builds, want the 13 fixtures", len(bs))
	}
	for _, b := range bs {
		if len(b.Steps) == 0 {
			t.Errorf("%s: no Dockerfile steps", b.Ref)
		}
	}
}

func TestAttributionFromTheTraceID(t *testing.T) {
	bs := loadFixtures(t)
	want := map[uint64]uint64{1001: 100, 1002: 101, 1003: 102, 1004: 103, 1005: 104, 1006: 104, 1007: 104, 1008: 105, 1009: 106, 1010: 107, 1011: 108}
	got := map[uint64]int{}
	unattributed := 0
	for _, b := range bs {
		a := b.Attribution
		switch a.State {
		case Attributed:
			if want[a.Job] != a.Pipeline {
				t.Errorf("%s: job %d in pipeline %d, want %d", b.Ref, a.Job, a.Pipeline, want[a.Job])
			}
			got[a.Job]++
		case Unattributed:
			unattributed++
			if b.Target != "worker" {
				t.Errorf("%s: unattributed %s build, want only the worker built without trace context", b.Ref, b.Target)
			}
		default:
			t.Errorf("%s: %s", b.Ref, a.State)
		}
	}
	if unattributed != 1 {
		t.Errorf("%d unattributed builds, want 1", unattributed)
	}
	if got[1011] != 2 {
		t.Errorf("job 1011 has %d builds, want 2 (api and test)", got[1011])
	}
}

func TestAttributeRefusesWhatItCannotTell(t *testing.T) {
	known := InstancePrefix("https://gitlab.example.com/")
	other := InstancePrefix("https://gitlab.com/")
	tests := []struct {
		name string
		ids  []string
		want AttributionState
	}{
		{"no trace", nil, Unattributed},
		{"own TRACEPARENT", []string{"4bf92f3577b34da6a3ce929d0e0e4736"}, Unattributed},
		{"scheme 01 of an unknown GitLab", []string{"01" + other + "000000000064000000000065"}, Unattributed},
		{"pipeline id 0", []string{"01" + known + "000000000000000000000065"}, Unattributed},
		{"two jobs in one trace", []string{"01" + known + "000000000064000000000065", "01" + known + "000000000064000000000066"}, Ambiguous},
		{"a job of a known GitLab", []string{"01" + known + "000000000064000000000065"}, Attributed},
	}
	for _, tc := range tests {
		a := Attribute(tc.ids, map[string]bool{known: true})
		if a.State != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, a.State, tc.want)
		}
	}
	a := Attribute([]string{"01" + known + "000000000064000000000065"}, map[string]bool{known: true})
	if a.Pipeline != 100 || a.Job != 101 || a.Instance != known {
		t.Errorf("decoded %+v", a)
	}
}

func TestStepStates(t *testing.T) {
	bs := loadFixtures(t)
	cold := byJob(t, bs, 1001, "api")
	for _, s := range cold.Steps {
		if s.State != Built {
			t.Errorf("cold build: %s %s, want built", s.Key, s.State)
		}
	}
	warm := byJob(t, bs, 1011, "api")
	for _, s := range warm.Steps {
		if s.State == Built && !strings.HasPrefix(s.Name, "FROM ") {
			t.Errorf("unchanged rebuild: %s built, want cached", s.Key)
		}
	}
}

// Three jobs of pipeline 104 need the "deps" stage at once on one daemon:
// one builds it, the others wait and take the result.
func TestReceivedFromAnotherJob(t *testing.T) {
	bs := loadFixtures(t)
	jobs := []*Build{byJob(t, bs, 1005, "api"), byJob(t, bs, 1006, "worker"), byJob(t, bs, 1007, "test")}
	var builder *Build
	for _, b := range jobs {
		if step(t, b, "[deps 4/4]").State == Built {
			if builder != nil {
				t.Fatalf("the shared step was built by %s and %s", builder.Ref, b.Ref)
			}
			builder = b
		}
	}
	if builder == nil {
		t.Fatal("nobody built the shared step")
	}
	for _, b := range jobs {
		if b == builder {
			continue
		}
		s := step(t, b, "[deps 4/4]")
		if s.State != Received || s.ReceivedFrom != builder.Ref {
			t.Errorf("job %d: %s from %q, want received from %s", b.Attribution.Job, s.State, s.ReceivedFrom, builder.Ref)
		}
		// The waiting jobs started later than the builder: they waited for
		// less than the step took.
		built := step(t, builder, "[deps 4/4]").Duration()
		if d := s.Duration(); d < time.Second || d >= built {
			t.Errorf("job %d waited %v for a step the builder ran in %v", b.Attribution.Job, d, built)
		}
		if s.Line != 5 || s.Identity != step(t, builder, "[deps 4/4]").Identity {
			t.Errorf("job %d: received step line %d identity %s, want the builder's step", b.Attribution.Job, s.Line, s.Identity)
		}
	}
}

// The shared stage is the same work for every target: identities must not
// depend on which paths a target takes from the context.
func TestIdentityIsStableAcrossBuildsAndTargets(t *testing.T) {
	bs := loadFixtures(t)
	a, b, w := byJob(t, bs, 1002, "api"), byJob(t, bs, 1003, "api"), byJob(t, bs, 1006, "worker")
	for _, k := range []string{"[deps 2/4]", "[deps 3/4]", "[deps 4/4]"} {
		if step(t, a, k).Identity != step(t, b, k).Identity || step(t, a, k).Identity != step(t, w, k).Identity {
			t.Errorf("%s: identities %s / %s / %s differ", k, step(t, a, k).Identity, step(t, b, k).Identity, step(t, w, k).Identity)
		}
	}
}

func TestReasons(t *testing.T) {
	bs := loadFixtures(t)
	b := func(job uint64) *Build { return byJob(t, bs, job, "api") }
	tests := []struct {
		name      string
		prev, cur *Build
		kind      ReasonKind
		key       string
		check     func(Reason) bool
	}{
		{"first build", nil, b(1001), FirstBuild, "[deps 2/4]", nil},
		{"build argument", b(1001), b(1002), ArgChanged, "[api 2/2]", func(r Reason) bool { return r.Arg == "VERSION" && r.Old == "dev" && r.New == "2" }},
		{"source file", b(1002), b(1003), FilesChanged, "[api 1/2]", func(r Reason) bool { return strings.Join(r.Paths, ",") == "api" }},
		{"dependencies", b(1003), b(1004), FilesChanged, "[deps 3/4]", func(r Reason) bool { return strings.Join(r.Paths, ",") == "deps.txt" }},
		{"changed step", b(1005), b(1008), StepChanged, "[api 2/2]", func(r Reason) bool { return strings.Contains(r.New, "echo changed") && r.Step.Line == 10 }},
		{"base image", b(1008), b(1009), BaseImageChanged, "[deps 1/4]", func(r Reason) bool {
			return strings.Contains(r.Old, "alpine:3.22@") && strings.Contains(r.New, "alpine:3.21@")
		}},
	}
	for _, tc := range tests {
		r, ok := FirstRebuild(tc.prev, tc.cur)
		if !ok {
			t.Errorf("%s: no rebuild found", tc.name)
			continue
		}
		if r.Kind != tc.kind || r.Step.Key != tc.key {
			t.Errorf("%s: %s at %s, want %s at %s", tc.name, r.Kind, r.Step.Key, tc.kind, tc.key)
			continue
		}
		if tc.check != nil && !tc.check(r) {
			t.Errorf("%s: %+v", tc.name, r)
		}
	}
	if _, ok := FirstRebuild(b(1008), b(1011)); ok {
		t.Error("a fully cached build reported a rebuild")
	}
}

func TestFailedBuild(t *testing.T) {
	bs := loadFixtures(t)
	f := byJob(t, bs, 1010, "broken")
	if !strings.Contains(f.Error, "exit code: 3") {
		t.Errorf("error %q", f.Error)
	}
	last := f.Steps[len(f.Steps)-1]
	if last.Key != "[broken 1/1]" || !strings.Contains(last.Error, "exit code: 3") {
		t.Errorf("last step %s %q, want the failing step", last.Key, last.Error)
	}
	if _, ok := FirstRebuild(nil, f); ok {
		t.Error("a build without provenance reported a reason")
	}
}

func TestContextSize(t *testing.T) {
	bs := loadFixtures(t)
	api, test := byJob(t, bs, 1001, "api"), byJob(t, bs, 1007, "test")
	if api.ContextBytes == 0 || test.ContextBytes <= api.ContextBytes {
		t.Errorf("context bytes api %d, test %d: the test target copies the whole context", api.ContextBytes, test.ContextBytes)
	}
}
