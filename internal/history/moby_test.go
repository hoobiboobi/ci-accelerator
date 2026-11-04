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
