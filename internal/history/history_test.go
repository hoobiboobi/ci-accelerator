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
	// Resolving a base image happens in every build: never work received
	// from another build, although its vertex is shared.
	for _, b := range bs {
		for _, s := range b.Steps {
			if strings.HasPrefix(s.Name, "FROM ") && s.State == Received {
				t.Errorf("%s: %s received from %s", b.Ref, s.Key, s.ReceivedFrom)
			}
		}
	}
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
		if step(t, b, "[deps 6/6]").State == Built {
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
		for _, k := range []string{"[deps 3/6]", "[deps 4/6]", "[deps 5/6]", "[deps 6/6]"} {
			s := step(t, b, k)
			if s.State != Received || s.ReceivedFrom != builder.Ref {
				t.Errorf("job %d %s: %s from %q, want received from %s", b.Attribution.Job, k, s.State, s.ReceivedFrom, builder.Ref)
			}
			if s.Identity != step(t, builder, k).Identity || s.Line != step(t, builder, k).Line {
				t.Errorf("job %d %s: line %d identity %s, want the builder's step", b.Attribution.Job, k, s.Line, s.Identity)
			}
		}
		// [deps 3/6] and [deps 4/6] read nothing from the context: the same
		// vertex is in every build's own mapping. [deps 3/6] started before
		// this build existed; [deps 4/6] has the same vertex and start as in
		// the builder: one execution. The waiting jobs started later than
		// the builder: they waited for less than the first step took.
		s, built := step(t, b, "[deps 3/6]"), step(t, builder, "[deps 3/6]").Duration()
		if d := s.Duration(); d < time.Second || d >= built {
			t.Errorf("job %d waited %v for a step the builder ran in %v", b.Attribution.Job, d, built)
		}
	}
}

// The executor's record may already be gone from the daemon's history
// (it keeps 50 records by default): a shared step that started before the
// waiting build existed is still received, from an unknown build.
func TestReceivedWithoutTheExecutorsRecord(t *testing.T) {
	var rest []*Build
	for _, b := range loadRecords(t) {
		if b.Attribution.Job != 1005 { // the build that ran the shared steps
			rest = append(rest, b)
		}
	}
	Link(rest)
	for _, b := range rest {
		if b.Attribution.Job != 1006 {
			continue
		}
		if s := step(t, b, "[deps 3/6]"); s.State != Received || s.ReceivedFrom != "" {
			t.Errorf("[deps 3/6]: %s from %q, want received from an unknown build", s.State, s.ReceivedFrom)
		}
	}
}

// The shared stage is the same work for every target: identities must not
// depend on which paths a target takes from the context.
func TestIdentityIsStableAcrossBuildsAndTargets(t *testing.T) {
	bs := loadFixtures(t)
	same := func(k string, builds ...*Build) {
		t.Helper()
		for _, b := range builds[1:] {
			if step(t, b, k).Identity != step(t, builds[0], k).Identity {
				t.Errorf("%s: %s in %s, %s in %s", k, step(t, builds[0], k).Identity, builds[0].Ref, step(t, b, k).Identity, b.Ref)
			}
		}
	}
	// Two builds of one target with the same inputs (the context changed
	// only in api/).
	for _, k := range []string{"[deps 2/6]", "[deps 5/6]", "[deps 6/6]"} {
		same(k, byJob(t, bs, 1002, "api"), byJob(t, bs, 1003, "api"))
	}
	// Three targets of one pipeline: each takes other paths from the
	// context, the shared stage is the same work.
	for _, k := range []string{"[deps 3/6]", "[deps 4/6]", "[deps 5/6]", "[deps 6/6]"} {
		same(k, byJob(t, bs, 1005, "api"), byJob(t, bs, 1006, "worker"), byJob(t, bs, 1007, "test"))
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
		{"first build", nil, b(1001), FirstBuild, "[deps 2/6]", nil},
		{"build argument", b(1001), b(1002), ArgChanged, "[api 2/2]", func(r Reason) bool { return r.Arg == "VERSION" && r.Old == "dev" && r.New == "2" }},
		// The context feeds the first rebuilt step (or an ancestor): the
		// files or the cache — the comparison of the commits settles it
		// later. Paths carry the whole cone's context sources; the daemons
		// stay empty on one daemon.
		{"source file", b(1002), b(1003), CacheElsewhere, "[api 1/2]", func(r Reason) bool {
			return strings.Join(r.Paths, ",") == "api,deps.txt" && r.Old == ""
		}},
		{"dependencies", b(1003), b(1004), CacheElsewhere, "[deps 5/6]", func(r Reason) bool {
			return strings.Join(r.Paths, ",") == "deps.txt" && r.Old == ""
		}},
		{"changed step", b(1005), b(1008), StepChanged, "[api 2/2]", func(r Reason) bool { return strings.Contains(r.New, "echo changed") && r.Step.Line == 14 }},
		{"base image", b(1008), b(1009), BaseImageChanged, "[deps 1/6]", func(r Reason) bool {
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
	// Back on the earlier base image, whose steps are still cached: the
	// image differs from the previous build, but nothing was rebuilt.
	if r, ok := FirstRebuild(b(1009), b(1011)); ok {
		t.Errorf("a fully cached build reported %s at %s", r.Kind, r.Step.Key)
	}
	// COPY . . reads the whole context: "." is among the cone's sources.
	if r, ok := FirstRebuild(byJob(t, bs, 1007, "test"), byJob(t, bs, 1011, "test")); !ok || r.Kind != CacheElsewhere || strings.Join(r.Paths, ",") != ".,deps.txt" {
		t.Errorf("COPY . .: %+v", r)
	}
}

// The cache is per daemon: a build on another daemon than the previous one
// does not find it there, whatever the files did.
func TestCacheOnAnotherDaemon(t *testing.T) {
	bs := loadFixtures(t)
	prev, cur := *byJob(t, bs, 1002, "api"), *byJob(t, bs, 1003, "api")
	prev.Daemon, cur.Daemon = "daemon-a", "daemon-b"
	r, ok := FirstRebuild(&prev, &cur)
	if !ok || r.Kind != CacheElsewhere || r.Step.Key != "[api 1/2]" || r.Old != "daemon-a" || r.New != "daemon-b" {
		t.Errorf("another daemon: %+v", r)
	}
	cur.Daemon = "daemon-a"
	if r, _ := FirstRebuild(&prev, &cur); r.Kind != CacheElsewhere || r.Old != "" {
		t.Errorf("same daemon: %+v, want cache-elsewhere without daemons", r)
	}

	// A step that reads no files and did not change was rebuilt only
	// because the cache was not there: on another daemon that is the
	// node, never the code; on the same daemon the cache lost it.
	again := *byJob(t, bs, 1003, "api")
	again.Steps = append([]Step(nil), again.Steps...)
	rebuilt := -1
	for i, s := range again.Steps {
		switch {
		case s.Key == "[deps 3/6]":
			again.Steps[i].State, rebuilt = Built, i
		case s.State == Built && again.prov.imageSource(s.StepID) == "":
			again.Steps[i].State = Cached
		}
	}
	if rebuilt < 0 || again.prov.localInput(again.Steps[rebuilt].StepID) {
		t.Fatalf("[deps 3/6] is not a step without files")
	}
	same := *byJob(t, bs, 1003, "api")
	same.Daemon, again.Daemon = "daemon-a", "daemon-b"
	if r, ok := FirstRebuild(&same, &again); !ok || r.Kind != OtherNode || r.Step.Key != "[deps 3/6]" || r.Old != "daemon-a" || r.New != "daemon-b" || r.By != "" {
		t.Errorf("no files, another daemon: %+v", r)
	}
	again.Daemon = "daemon-a"
	if r, _ := FirstRebuild(&same, &again); r.Kind != NotInCache {
		t.Errorf("no files, same daemon: %s, want not-in-cache", r.Kind)
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
