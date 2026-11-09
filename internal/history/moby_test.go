package history

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// Records of moby/moby's own Dockerfile (docker-v29.9.2) built by the CI
// component on a one-machine box, 2026-11-04:
//   - wm78gon… binary, the first build;
//   - pzrlnh0… binary, after a one-line comment in cmd/dockerd/main.go;
//   - jxi8m9n… all, after that same change;
//   - nlcgo1q… all, after the base stage's ENV CI_ACCELERATOR_CHECK
//     changed from 0.3.3 to 0.5.0.
//
// and on another such box, 2026-11-09, three targets in parallel per
// pipeline:
//   - gpzka9v… binary, built again with no change;
//   - 2l9nh7i… binary, after a one-line change in cmd/dockerd/main.go,
//     its last step received from the job building "all";
//   - 0upbxwq… dev, after that same one-line change;
//   - o0196ec… dev, after a change in hack/dockerfile/etc/docker/
//     daemon.json, which its stage copies with COPY --link.
func loadMoby(t *testing.T, ref string) *Build {
	t.Helper()
	d := filepath.Join("testdata/moby", ref)
	rec, err := ParseRecord(readOpt(t, filepath.Join(d, "record.json")))
	if err != nil {
		t.Fatal(err)
	}
	progress, err := DecodeProgress(bytes.NewReader(readOpt(t, filepath.Join(d, "logs.bin"))))
	if err != nil {
		t.Fatalf("%s: %v", d, err)
	}
	b, err := NewBuild(rec, progress, [][]byte{readOpt(t, filepath.Join(d, "provenance.json"))}, nil, nil, Config{Instances: testInstances, SecretKey: []byte("test")})
	if err != nil {
		t.Fatalf("%s: %v", d, err)
	}
	return b
}

// moby's Dockerfile ADDs git repositories at a tag. BuildKit resolves such
// a source again in every build, so it shows as run while the copy after
// it comes from the cache: the source must not be taken for a changed
// Dockerfile line (seen as "Rebuilt from ADD https://github.com/tc-hib/
// go-winres.git…: this step changed" with the same text before and after).
func TestGitSourceFetchedAgainIsNoReason(t *testing.T) {
	prev, cur := loadMoby(t, "wm78gon009eiywij5se0v5l51"), loadMoby(t, "pzrlnh08l50c5heeemgpe3h8b")
	r, ok := FirstRebuild(prev, cur)
	if !ok {
		t.Fatal("no reason for a build that ran steps")
	}
	if strings.HasPrefix(r.Step.Name, "ADD https://") || strings.HasPrefix(r.Step.Name, "ADD git") {
		t.Fatalf("the reason names the git source: %+v", r)
	}
	if r.Kind == StepChanged && r.Old == r.New {
		t.Fatalf("a step changed with the same text before and after: %+v", r)
	}
	// The step that read the changed file builds dockerd from the build
	// context (and from go-winres' git source): the context's case, so that
	// GitLab's comparison of the commits can prove the change.
	if r.Step.Key != "[build 6/6]" || r.Kind != CacheElsewhere || len(r.Paths) == 0 {
		t.Errorf("first rebuilt step %s (%s, paths %v), want [build 6/6] fed by the context", r.Step.Key, r.Kind, r.Paths)
	}
}

// An ENV line in the base stage changed: every RUN after it has the same
// text and command and another environment. The reason names the ENV,
// with both values, instead of "this step changed" between two equal texts.
func TestEnvLineChangeNamesTheVariable(t *testing.T) {
	prev, cur := loadMoby(t, "jxi8m9nx3ml2srd8l0lus38tz"), loadMoby(t, "nlcgo1qpdfi8m51ongesw8ly7")
	r, ok := FirstRebuild(prev, cur)
	if !ok {
		t.Fatal("no reason for a build that ran steps")
	}
	if r.Kind != StepChanged || r.Old != "ENV CI_ACCELERATOR_CHECK=0.3.3" || r.New != "ENV CI_ACCELERATOR_CHECK=0.5.0" {
		t.Fatalf("reason %s %q -> %q (step %s), want the ENV line's change", r.Kind, r.Old, r.New, r.Step.Key)
	}
}

// A build whose only executed steps are git sources fetched again built
// nothing: BuildKit resolves a tag again in every build, the steps that
// use it came from the cache or another job. It is not "1 step ran,
// nothing to compare with" (seen on the binary target, which received its
// last step from the job building "all").
func TestGitSourceFetchedAgainRanNothing(t *testing.T) {
	prev, cur := loadMoby(t, "gpzka9vad2oye8piy0ule0m8l"), loadMoby(t, "2l9nh7i2965vnggagtocjx2vf")
	for _, b := range []*Build{prev, cur} {
		fetched := 0
		for i, s := range b.Steps {
			if s.State == Built && b.prov.fetchSource(s.StepID) {
				fetched++
			}
			if b.Ran(i) {
				t.Errorf("%s: %s %q counts as run", b.Ref, s.Key, s.Name)
			}
		}
		if fetched == 0 {
			t.Fatalf("%s: no git source fetched again: the record no longer shows the case", b.Ref)
		}
	}
	if r, ok := FirstRebuild(prev, cur); ok {
		t.Errorf("a reason for a build that built nothing: %s %s", r.Kind, r.Step.Key)
	}
}

// COPY --link is two operations under one key and one line, a copy
// ("LINK COPY ...") and a merge ("COPY ..."), and a build lists one of
// them, not always the same: between two dev builds of one base stage
// several keys changed sides. A step is compared with the previous build's operation
// of its own kind, never a copy with a merge.
func TestCopyLinkComparedWithItsOwnKind(t *testing.T) {
	prev, cur := loadMoby(t, "0upbxwq1irnwiln0cvj1ojdm2"), loadMoby(t, "o0196ecatampo7br2dh677y5b")
	idx := stepsByKey(prev, cur)
	listed := map[string]Step{}
	for _, s := range prev.Steps {
		listed[s.Key] = s
	}
	swapped := 0
	for _, s := range cur.Steps {
		ps, ok := listed[s.Key]
		if !ok || s.StepID == "" || !strings.Contains(s.Name, "--link") || prev.prov.opKind(ps.StepID) == cur.prov.opKind(s.StepID) {
			continue
		}
		swapped++
		p, _ := idx.of(s)
		if prev.prov.opKind(p.StepID) != cur.prov.opKind(s.StepID) {
			t.Errorf("%s: %s compared with a %s", s.Key, cur.prov.opKind(s.StepID), prev.prov.opKind(p.StepID))
		}
		if p.Identity != s.Identity {
			t.Errorf("%s: an unchanged line, another identity (%s vs %s)", s.Key, p.Name, s.Name)
		}
	}
	if swapped == 0 {
		t.Fatal("no COPY --link step listed as the other operation: the records no longer show the case")
	}
}

// The case seen on moby (2026-11-04): the steps above a COPY --link were
// received from a parallel job, so the first step this build executed is
// the COPY itself, and the previous build listed its key as the other
// operation. The reason was "Dockerfile changed: LINK COPY ... -> COPY
// ..." for a line that did not change.
func TestCopyLinkFirstExecutedIsNotAChangedLine(t *testing.T) {
	prev, cur := loadMoby(t, "0upbxwq1irnwiln0cvj1ojdm2"), loadMoby(t, "o0196ecatampo7br2dh677y5b")
	listed := map[string]Step{}
	for _, s := range prev.Steps {
		listed[s.Key] = s
	}
	target := -1
	for i, s := range cur.Steps {
		ps, ok := listed[s.Key]
		if ok && s.StepID != "" && strings.Contains(s.Name, "--link --from=") && prev.prov.opKind(ps.StepID) != cur.prov.opKind(s.StepID) {
			target = i
			break
		}
	}
	if target < 0 {
		t.Fatal("every COPY --link --from is listed as the same operation in both builds: the records no longer show the case")
	}
	// Everything else came from the cache or the other job; the COPY ran.
	b := *cur
	b.Steps = append([]Step(nil), cur.Steps...)
	for i := range b.Steps {
		switch {
		case i == target:
			b.Steps[i].State = Built
		case b.Steps[i].State == Built:
			b.Steps[i].State = Received
		}
	}
	r, ok := FirstRebuild(prev, &b)
	if !ok {
		t.Fatal("no reason for a build that ran a step")
	}
	if r.Step.Key != cur.Steps[target].Key {
		t.Fatalf("first rebuilt step %s, want %s", r.Step.Key, cur.Steps[target].Key)
	}
	if r.Kind == StepChanged || r.Kind == NewStep {
		t.Fatalf("an unchanged COPY --link line reads as changed: %s %q -> %q", r.Kind, r.Old, r.New)
	}
}
