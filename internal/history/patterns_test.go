package history

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	controlapi "github.com/moby/buildkit/api/services/control"
)

// Common CI patterns the reasons must not explain away as the build farm's
// doing, or explain by the wrong step. Each case is a recorded build of the
// fixtures, changed only where the pattern needs it.

// A stage shared by parallel jobs: the job that changed it (a build
// argument) built it, this one received it from that job and ran only its
// own steps. The reason is the argument, not "the step after it changed"
// with the same text on both sides.
func TestReasonOfAChangeReceivedFromAnotherJob(t *testing.T) {
	bs := loadFixtures(t)
	cur := *byJob(t, bs, 1005, "api")
	cur.Steps = append([]Step(nil), cur.Steps...)
	for i, s := range cur.Steps {
		switch s.Key {
		case "[deps 3/6]", "[deps 4/6]", "[deps 5/6]", "[deps 6/6]":
			cur.Steps[i].State = Received
		}
	}
	r, ok := FirstRebuild(byJob(t, bs, 1004, "api"), &cur)
	if !ok || r.Kind != ArgChanged || r.Arg != "TOOLS" || r.Old != "1" || r.New != "2" || r.Step.Key != "[api 1/2]" {
		t.Errorf("received changed stage: %+v", r)
	}
}

// --no-cache (or --opt no-cache=<stages>) is the job's choice, never the
// farm's: every step of those stages runs whatever is cached.
func TestNoCacheIsTheJobsChoice(t *testing.T) {
	bs := loadFixtures(t)
	prev := byJob(t, bs, 1003, "api")
	for _, c := range []struct {
		attr string
		set  bool
		kind ReasonKind
	}{
		{"", true, NoCache},           // --no-cache
		{"deps", true, NoCache},       // the stage of the first rebuilt step
		{"api,deps", true, NoCache},   // a list naming it
		{"api", true, CacheElsewhere}, // another stage: the usual reason
		{"", false, CacheElsewhere},
	} {
		rec := &controlapi.BuildHistoryRecord{FrontendAttrs: map[string]string{}}
		if c.set {
			rec.FrontendAttrs["no-cache"] = c.attr
		}
		b, err := NewBuild(rec, nil, nil, nil, nil, Config{})
		if err != nil {
			t.Fatal(err)
		}
		cur := *byJob(t, bs, 1004, "api")
		cur.NoCache = b.NoCache
		if r, ok := FirstRebuild(prev, &cur); !ok || r.Kind != c.kind {
			t.Errorf("no-cache=%q (set %v): %s at %s, want %s", c.attr, c.set, r.Kind, r.Step.Key, c.kind)
		}
	}
	if !contains(ChangeKinds(), NoCache) || contains(FarmKinds(), NoCache) {
		t.Error("NoCache must count as the build's own change")
	}
}

// Something fetched from the network can change while every definition
// stays the same: such a rebuild is unclear, not the farm's, unless a
// checksum (or a git commit) pins it. A step the build context feeds as
// well is the context's case, so that a changed file can prove the change;
// the remote source rides along in its paths and keeps it from ever being
// read as the farm's.
func TestRemoteSourceRebuild(t *testing.T) {
	bs := loadFixtures(t)
	prev := *byJob(t, bs, 1003, "api")
	prev.Daemon = "daemon-a"
	with := func(key, identifier string, attrs map[string]string) Reason {
		t.Helper()
		cur := *byJob(t, bs, 1003, "api")
		cur.Daemon = "daemon-b"
		p := *cur.prov
		p.Steps = map[string]ProvStep{}
		for k, v := range cur.prov.Steps {
			p.Steps[k] = v
		}
		op, _ := json.Marshal(map[string]any{"Op": map[string]any{"source": map[string]any{"identifier": identifier, "attrs": attrs}}})
		p.Steps["remote"] = ProvStep{ID: "remote", Op: op}
		run := step(t, &cur, key)
		s := p.Steps[run.StepID]
		s.Inputs = append(append([]string(nil), s.Inputs...), "remote")
		p.Steps[run.StepID] = s
		cur.prov = &p
		cur.Steps = append([]Step(nil), cur.Steps...)
		for i := range cur.Steps {
			cur.Steps[i].State = Cached
			if cur.Steps[i].Key == key {
				cur.Steps[i].State = Built
			}
		}
		r, _ := FirstRebuild(&prev, &cur)
		return r
	}
	const net = "[deps 3/6]" // RUN sleep 2 ...: no file of the context below it
	r := with(net, "https://user:token@example.com/tool.tar.gz?sig=abc", nil)
	if r.Kind != RemoteSource || strings.Join(r.Paths, ",") != "https://example.com/tool.tar.gz?…" || r.Old != "daemon-a" {
		t.Errorf("http source: %+v", r)
	}
	if r := with(net, "https://example.com/tool.tar.gz", map[string]string{"http.checksum": "sha256:" + strings.Repeat("a", 64)}); r.Kind == RemoteSource {
		t.Errorf("a checksum pins the source: %+v", r)
	}
	if r := with(net, "git://github.com/example/tool.git#main", nil); r.Kind != RemoteSource {
		t.Errorf("git by branch: %+v", r)
	}
	if r := with(net, "git://github.com/example/tool.git#"+strings.Repeat("c", 40), nil); r.Kind == RemoteSource {
		t.Errorf("git by commit: %+v", r)
	}
	// [api 2/2] builds on api/ and deps.txt of the context too.
	r = with("[api 2/2]", "git://github.com/example/tool.git#main", nil)
	if r.Kind != CacheElsewhere || !slices.Contains(r.Paths, "api") || !slices.Contains(r.Paths, "git://github.com/example/tool.git#main") {
		t.Errorf("context and git source: %+v", r)
	}
	if contains(FarmKinds(), RemoteSource) || !contains(UnclearKinds(), RemoteSource) {
		t.Error("a remote source rebuild must count as unclear")
	}
}

// A previous build whose top came from the cache reports only the steps
// above the hit. Read from its record that is known; restored from the
// store it must still be known, or a changed step reads as a new one.
func TestTruncatedSurvivesRestore(t *testing.T) {
	bs := loadFixtures(t)
	full := byJob(t, bs, 1001, "api")
	top := *full
	top.Steps = full.Steps[len(full.Steps)-1:]
	top.Truncated = !top.complete()
	if !top.Truncated {
		t.Fatal("a build with one reported step of eight must be truncated")
	}
	defs := map[string]Def{}
	for _, d := range full.Defs() {
		defs[d.Identity] = d
	}
	c := top
	c.prov = nil
	restored, err := Restore(c, defs)
	if err != nil {
		t.Fatal(err)
	}
	cur := byJob(t, bs, 1005, "api")
	in, _ := FirstRebuild(&top, cur)
	re, _ := FirstRebuild(restored, cur)
	if in.Kind != re.Kind || re.Kind == NewStep {
		t.Errorf("after a top cache hit: %s in memory, %s restored", in.Kind, re.Kind)
	}
}

// The context folder a job names (the CI component passes it) is kept as
// a path: ./services/api/ and services/api are one folder.
func TestContextOption(t *testing.T) {
	for in, want := range map[string]string{"./services/api/": "services/api", "services/api": "services/api", ".": ".", "./": "."} {
		b, err := NewBuild(&controlapi.BuildHistoryRecord{FrontendAttrs: map[string]string{ContextOpt: in}}, nil, nil, nil, nil, Config{})
		if err != nil || b.Context != want {
			t.Errorf("context %q: %q, %v; want %q", in, b.Context, err, want)
		}
	}
}

func contains(ks []ReasonKind, k ReasonKind) bool {
	for _, x := range ks {
		if x == k {
			return true
		}
	}
	return false
}

// Line ranges come from the client's source map: kept within the file and
// a few per step, or one page view walks billions of lines.
func TestClampLines(t *testing.T) {
	for _, c := range []struct {
		in   LineRange
		n    int
		want LineRange
		ok   bool
	}{
		{LineRange{1, 1<<31 - 1}, 2, LineRange{1, 2}, true},
		{LineRange{-5, 1}, 2, LineRange{1, 1}, true},
		{LineRange{3, 9}, 2, LineRange{3, 2}, false},
		{LineRange{2, 2}, 2, LineRange{2, 2}, true},
	} {
		got, ok := clampLines(c.in, c.n)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("clampLines(%v, %d) = %v, %v", c.in, c.n, got, ok)
		}
	}
	if lineCount("FROM a\nRUN b\n") != 2 || lineCount("FROM a") != 1 {
		t.Error("lineCount")
	}
}
