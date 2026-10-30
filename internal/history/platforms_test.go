package history

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
	digest "github.com/opencontainers/go-digest"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// platformProvenance is the provenance BuildKit writes for one platform of
// a build of "FROM alpine / RUN <run>": each platform's result has its own,
// covering only that platform's vertices.
func platformProvenance(t *testing.T, arch, run string) []byte {
	t.Helper()
	platform := map[string]string{"OS": "linux", "Architecture": arch}
	at := func(line int) map[string]any {
		return map[string]any{"locations": []any{map[string]any{"ranges": []any{map[string]any{"start": map[string]int{"line": line}, "end": map[string]int{"line": line}}}}}}
	}
	p := map[string]any{
		"buildDefinition": map[string]any{
			"internalParameters": map[string]any{
				"buildConfig": map[string]any{
					"llbDefinition": []any{
						map[string]any{"id": "step0", "op": map[string]any{"Op": map[string]any{"source": map[string]any{"identifier": "docker-image://docker.io/library/alpine:3.22"}}, "platform": platform}},
						map[string]any{"id": "step1", "inputs": []string{"step0:0"}, "op": map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{"args": []string{"/bin/sh", "-c", run}}}}, "platform": platform}},
						map[string]any{"id": "step2", "inputs": []string{"step1:0"}, "op": map[string]any{"Op": map[string]any{}}},
					},
					"digestMapping": map[string]string{
						vertexDigest(arch, "from", ""): "step0",
						vertexDigest(arch, "run", run): "step1",
					},
				},
			},
		},
		"runDetails": map[string]any{"metadata": map[string]any{"buildkit_metadata": map[string]any{"source": map[string]any{
			"locations": map[string]any{"step0": at(1), "step1": at(2)},
			"infos":     []any{map[string]any{"filename": "Dockerfile", "language": "Dockerfile", "data": []byte("FROM alpine:3.22\nRUN make\n")}},
		}}}},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func vertexDigest(arch, step, run string) string {
	return digest.FromString(arch + "/" + step + "/" + run).String()
}

// multiPlatform is one build for linux/amd64 and linux/arm64 started at t0;
// each platform's RUN step ran from span[0] to span[1] seconds after t0, or
// came from the cache. With provArchs, only those platforms' provenance is
// read, as before every platform's was.
func multiPlatform(t *testing.T, ref string, t0 time.Time, run map[string]string, span map[string][2]float64, cached map[string]bool, provArchs ...string) *Build {
	t.Helper()
	if provArchs == nil {
		provArchs = []string{"amd64", "arm64"}
	}
	at := func(s float64) *time.Time { x := t0.Add(time.Duration(s * float64(time.Second))); return &x }
	var vs []*client.Vertex
	var provs [][]byte
	for _, arch := range []string{"amd64", "arm64"} {
		prefix := fmt.Sprintf("[linux/%s ", arch)
		vs = append(vs,
			&client.Vertex{Digest: digest.Digest(vertexDigest(arch, "from", "")), Name: prefix + "1/2] FROM docker.io/library/alpine:3.22", Started: at(0), Completed: at(0.5)},
			&client.Vertex{Digest: digest.Digest(vertexDigest(arch, "run", run[arch])), Name: prefix + "2/2] RUN " + run[arch], Started: at(span[arch][0]), Completed: at(span[arch][1]), Cached: cached[arch]},
		)
		if slices.Contains(provArchs, arch) {
			provs = append(provs, platformProvenance(t, arch, run[arch]))
		}
	}
	rec := &controlapi.BuildHistoryRecord{Ref: ref, CreatedAt: timestamppb.New(t0), CompletedAt: timestamppb.New(t0.Add(time.Minute))}
	b, err := NewBuild(rec, []*client.SolveStatus{{Vertexes: vs}}, provs, nil, nil, Config{SecretKey: []byte("test")})
	if err != nil {
		t.Fatal(err)
	}
	b.Daemon = "daemon-a"
	return b
}

// A build for several platforms has one provenance per platform: the steps
// each platform executed are the build's own, with their time and their
// rebuilds, not work received from nobody.
func TestMultiPlatformStepsAreOwn(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	run := map[string]string{"amd64": "make", "arm64": "make"}
	b := multiPlatform(t, "ref1", t0, run, map[string][2]float64{"amd64": {1, 20}, "arm64": {20, 40}}, nil)
	Link([]*Build{b})
	ids := map[string]bool{}
	for _, s := range b.Steps {
		if s.Name != "RUN make" {
			continue
		}
		if s.State != Built || s.StepID == "" || s.Identity == "" || s.Line != 2 || s.Duration() < 19*time.Second {
			t.Errorf("%s: %s, step %q, identity %q, line %d, %s; want built with its own step", s.Key, s.State, s.StepID, s.Identity, s.Line, s.Duration())
		}
		ids[s.Identity] = true
	}
	if len(ids) != 2 {
		t.Errorf("identities %v, want one per platform", ids)
	}
	if got := b.ExecutedTime(); got != 39*time.Second {
		t.Errorf("executed %s, want 39s (both platforms)", got)
	}
	if !b.complete() {
		t.Error("a build that reported every platform's steps is not complete")
	}

	// The next build: amd64 from the cache, only arm64 rebuilt. Its rebuild
	// is explained, also against the previous build as the store restores it.
	cur := multiPlatform(t, "ref2", t0.Add(time.Hour), run, map[string][2]float64{"amd64": {1, 1}, "arm64": {1, 30}}, map[string]bool{"amd64": true})
	defs := map[string]Def{}
	for _, d := range b.Defs() {
		defs[d.Identity] = d
	}
	restored, err := Restore(*b, defs)
	if err != nil {
		t.Fatal(err)
	}
	for name, prev := range map[string]*Build{"read": b, "restored": restored} {
		r, ok := FirstRebuild(prev, cur)
		if !ok || r.Kind != NotInCache || r.Step.Key != "[linux/arm64 2/2]" {
			t.Errorf("%s: reason %v %s %q, want not-in-cache for the arm64 step", name, ok, r.Kind, r.Step.Key)
		}
	}

	// Only arm64's command changed: that step changed.
	run2 := map[string]string{"amd64": "make", "arm64": "make arm"}
	cur = multiPlatform(t, "ref3", t0.Add(2*time.Hour), run2, map[string][2]float64{"amd64": {1, 1}, "arm64": {1, 30}}, map[string]bool{"amd64": true})
	if r, ok := FirstRebuild(b, cur); !ok || r.Kind != StepChanged || r.Step.Key != "[linux/arm64 2/2]" {
		t.Errorf("changed arm64 step: reason %v %s %q, want step-changed", ok, r.Kind, r.Step.Key)
	}
}

// A build stored when only the first platform's provenance was read has
// the other platforms' steps without a definition. The next build's
// rebuilt step of such a platform is not new: it has nothing to be
// compared with.
func TestMultiPlatformPreviousWithoutEveryProvenance(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	run := map[string]string{"amd64": "make", "arm64": "make"}
	prev := multiPlatform(t, "old", t0, run, map[string][2]float64{"amd64": {1, 20}, "arm64": {20, 40}}, nil, "amd64")
	defs := map[string]Def{}
	for _, d := range prev.Defs() {
		defs[d.Identity] = d
	}
	restored, err := Restore(*prev, defs)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		span   map[string][2]float64
		cached map[string]bool
		ok     bool
		key    string
	}{
		"arm64 only":  {map[string][2]float64{"amd64": {1, 1}, "arm64": {1, 30}}, map[string]bool{"amd64": true}, false, ""},
		"arm64 first": {map[string][2]float64{"amd64": {2, 20}, "arm64": {1, 30}}, nil, false, ""},
		"amd64 first": {map[string][2]float64{"amd64": {1, 20}, "arm64": {2, 30}}, nil, true, "[linux/amd64 2/2]"},
	} {
		cur := multiPlatform(t, "new", t0.Add(3*time.Hour), run, c.span, c.cached)
		r, ok := FirstRebuild(restored, cur)
		if ok != c.ok || r.Step.Key != c.key || ok && r.Kind != NotInCache {
			t.Errorf("%s: reason %v %s %q, want %v %q", name, ok, r.Kind, r.Step.Key, c.ok, c.key)
		}
	}
}

// A stage built for the build platform ("FROM --platform=$BUILDPLATFORM")
// is the same operation, with the same vertex, in every platform's
// provenance: it is one step, under one id, and the platform's own steps
// read it from there.
func TestMergeProvenanceSharedStep(t *testing.T) {
	op := func(o string) json.RawMessage { return json.RawMessage(o) }
	shared := op(`{"Op":{"exec":{"meta":{"args":["go","build"]}}},"platform":{"OS":"linux","Architecture":"amd64"}}`)
	prov := func(arch string) *Provenance {
		p := &Provenance{
			Steps: map[string]ProvStep{
				"step0": {ID: "step0", Op: shared},
				"step1": {ID: "step1", Inputs: []string{"step0"}, Op: op(`{"Op":{"exec":{"meta":{"args":["test"]}}},"platform":{"OS":"linux","Architecture":"` + arch + `"}}`)},
			},
			DigestMapping: map[string]string{"sha256:build": "step0", "sha256:test-" + arch: "step1"},
			Lines:         map[string]int{"step0": 2, "step1": 4},
			Ranges:        map[string][]LineRange{},
			Identity:      map[string]string{},
			Usage:         map[string]Usage{"step0": {CPU: time.Second}},
		}
		for id := range p.Steps {
			if _, err := p.identity(id, map[string]bool{}); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	amd, arm := prov("amd64"), prov("arm64")
	m := mergeProvenance([]*Provenance{amd, arm})
	if len(m.Steps) != 3 || len(m.Lines) != 3 || m.DigestMapping["sha256:build"] != "step0" {
		t.Fatalf("merged steps %v, lines %v, mapping %v; want the shared step once", m.Steps, m.Lines, m.DigestMapping)
	}
	test := m.DigestMapping["sha256:test-arm64"]
	if test == "step1" || m.Identity[test] != arm.Identity["step1"] || m.Steps[test].Inputs[0] != "step0" {
		t.Errorf("arm64 test step %q: %+v", test, m.Steps[test])
	}
	if len(amd.Steps) != 2 || len(amd.DigestMapping) != 2 {
		t.Error("merging changed the first platform's provenance")
	}
	b := &Build{prov: m, Steps: []Step{{StepID: "step0"}, {StepID: "step1"}, {StepID: test}}}
	if !b.complete() {
		t.Error("every step reported, but not complete")
	}
}

// singlePlatform is one build of "FROM alpine / RUN <run>" with the build
// arguments args. foreign makes its RUN vertex another build's: "received"
// (that build executed it) or "cached" (both took it from the cache).
func singlePlatform(t *testing.T, ref string, t0 time.Time, run string, args map[string]string, foreign string) *Build {
	t.Helper()
	at := func(s float64) *time.Time { x := t0.Add(time.Duration(s * float64(time.Second))); return &x }
	runDigest := vertexDigest("amd64", "run", run)
	if foreign != "" {
		runDigest = vertexDigest("sibling", "run", run)
	}
	vs := []*client.Vertex{
		{Digest: digest.Digest(vertexDigest("amd64", "from", "")), Name: "[1/2] FROM docker.io/library/alpine:3.22", Started: at(0), Completed: at(0.5)},
		{Digest: digest.Digest(runDigest), Name: "[2/2] RUN " + run, Started: at(1), Completed: at(20), Cached: foreign == "cached"},
	}
	rec := &controlapi.BuildHistoryRecord{Ref: ref, CreatedAt: timestamppb.New(t0), CompletedAt: timestamppb.New(t0.Add(time.Minute)), FrontendAttrs: map[string]string{}}
	for k, v := range args {
		rec.FrontendAttrs["build-arg:"+k] = v
	}
	b, err := NewBuild(rec, []*client.SolveStatus{{Vertexes: vs}}, [][]byte{platformProvenance(t, "amd64", run)}, nil, nil, Config{SecretKey: []byte("test")})
	if err != nil {
		t.Fatal(err)
	}
	b.Daemon = "daemon-a"
	return b
}

// The previous build received a step from a parallel build, or took it
// from the cache through another build's vertex: the step has no
// definition there, but its text does. A changed text is still the
// change that rebuilt it, live and after a restart.
func TestRebuildOfStepThePreviousBuildReceived(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		prevRun, curRun     string
		prevArgs, curArgs   map[string]string
		ok                  bool
		kind                ReasonKind
		arg, oldVal, newVal string
	}{
		"text changed":   {"make", "make all", nil, nil, true, StepChanged, "", "make", "make all"},
		"arg in text":    {"make V=1.0.1", "make V=1.0.2", map[string]string{"VERSION": "1.0.1"}, map[string]string{"VERSION": "1.0.2"}, true, ArgChanged, "VERSION", "1.0.1", "1.0.2"},
		"secret in text": {"make T=aaaa1111", "make T=bbbb2222", map[string]string{"API_TOKEN": "aaaa1111"}, map[string]string{"API_TOKEN": "bbbb2222"}, false, "", "", "", ""},
		"short secret":   {"make T=abc", "make T=xyz", map[string]string{"API_TOKEN": "abc"}, map[string]string{"API_TOKEN": "xyz"}, true, ArgChanged, "API_TOKEN", Masked, Masked},
		"same text":      {"make", "make", nil, nil, false, "", "", "", ""},
	} {
		for _, foreign := range []string{"received", "cached"} {
			prev := singlePlatform(t, "old", t0, c.prevRun, c.prevArgs, foreign)
			if s := step(t, prev, "[2/2]"); s.StepID != "" {
				t.Fatalf("%s/%s: previous step has a definition %q", name, foreign, s.StepID)
			}
			cur := singlePlatform(t, "new", t0.Add(time.Hour), c.curRun, c.curArgs, "")
			if c.curRun == c.prevRun {
				// The same text with another definition below it.
				cur.prov.Identity[cur.Steps[1].StepID] = "other"
				cur.Steps[1].Identity = "other"
			}
			defs := map[string]Def{}
			for _, d := range prev.Defs() {
				defs[d.Identity] = d
			}
			restored, err := Restore(*prev, defs)
			if err != nil {
				t.Fatal(err)
			}
			for from, p := range map[string]*Build{"live": prev, "restored": restored} {
				r, ok := FirstRebuild(p, cur)
				if ok != c.ok || r.Kind != c.kind || ok && (r.Step.Key != "[2/2]" || r.Arg != c.arg || !strings.HasSuffix(r.Old, c.oldVal) || !strings.HasSuffix(r.New, c.newVal)) {
					t.Errorf("%s/%s/%s: %v %s %q %q -> %q, want %v %s %q %q -> %q", name, foreign, from, ok, r.Kind, r.Arg, r.Old, r.New, c.ok, c.kind, c.arg, c.oldVal, c.newVal)
				}
			}
		}
	}
}
