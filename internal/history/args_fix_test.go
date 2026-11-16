package history

import (
	"strings"
	"testing"

	controlapi "github.com/moby/buildkit/api/services/control"
)

// A build argument that is not secret by name keeps its value masked from
// the start, exactly as the store keeps it (history.MaskSecrets over the
// value), so a build read now compares with a build read back from the
// store: argInText finds the same argument change either way.
func TestBuildArgsMaskedAsStored(t *testing.T) {
	build := func(ref, version string) *Build {
		t.Helper()
		rec := &controlapi.BuildHistoryRecord{Ref: ref, FrontendAttrs: map[string]string{
			"build-arg:A_TOKEN":  "s3cretArgValue",
			"build-arg:REPO_URL": "https://oauth2:glpat-abcdefghijklmnop1234@gitlab.example.com/x/y.git",
			"build-arg:NOTE":     "uses s3cretArgValue here",
			"build-arg:VERSION":  version + " glpat-ZYXWVUTSRQPONMLK9876",
			"build-arg:PLAIN":    "1.2.3",
		}}
		b, err := NewBuild(rec, nil, nil, nil, nil, Config{})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cur := build("cur", "v2")
	for k, v := range cur.BuildArgs {
		if SecretArg(k) {
			continue
		}
		if stored := MaskSecrets(v); stored != v {
			t.Errorf("%s: kept %q, the store keeps %q", k, v, stored)
		}
		for _, s := range []string{"glpat-", "s3cretArgValue"} {
			if strings.Contains(v, s) {
				t.Errorf("%s: %q holds %q", k, v, s)
			}
		}
	}
	if cur.BuildArgs["PLAIN"] != "1.2.3" {
		t.Errorf("PLAIN = %q", cur.BuildArgs["PLAIN"])
	}
	if cur.BuildArgs["A_TOKEN"] != "s3cretArgValue" {
		t.Errorf("a secret argument is no longer kept whole: %q", cur.BuildArgs["A_TOKEN"])
	}

	// The previous build as the store gives it back: its non-secret values
	// through MaskSecrets.
	prev := build("prev", "v1")
	for k, v := range prev.BuildArgs {
		if !SecretArg(k) {
			prev.BuildArgs[k] = MaskSecrets(v)
		}
	}
	// A step's text as NewBuild keeps it: masked with the build's arguments.
	text := func(b *Build, raw string) string { return mask(raw, b.BuildArgs) }
	old := text(prev, "[2/3] RUN echo v1 glpat-ZYXWVUTSRQPONMLK9876")
	new := text(cur, "[2/3] RUN echo v2 glpat-ZYXWVUTSRQPONMLK9876")
	name, from, to, ok := argInText(prev, cur, old, new)
	if !ok || name != "VERSION" {
		t.Fatalf("argInText = %q %q %q %v, want VERSION", name, from, to, ok)
	}
}
