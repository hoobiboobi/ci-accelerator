package history

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A real build of a Dockerfile that breaks five of BuildKit's checks
// (testdata/warnings: FROM ... as, MAINTAINER, ENV key value, WORKDIR app,
// CMD echo done): each is read once, with its rule, line and link.
func TestBuildCheckWarnings(t *testing.T) {
	read := func(n string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata/warnings", n))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	rec, err := ParseRecord(read("record.json"))
	if err != nil {
		t.Fatal(err)
	}
	progress, err := DecodeProgress(bytes.NewReader(read("logs.bin")))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBuild(rec, progress, nil, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		rule string
		line int
	}{{"FromAsCasing", 1}, {"MaintainerDeprecated", 2}, {"LegacyKeyValueFormat", 3}, {"WorkdirRelativePath", 4}, {"JSONArgsRecommended", 8}}
	if len(b.Warnings) != len(want) || int(rec.NumWarnings) != len(want) {
		t.Fatalf("%d warnings (record says %d): %+v", len(b.Warnings), rec.NumWarnings, b.Warnings)
	}
	for i, w := range want {
		got := b.Warnings[i]
		if got.Rule != w.rule || got.Line != w.line || got.EndLine != w.line || got.File != "Dockerfile" ||
			got.URL == "" || got.Detail == "" || got.Message == "" {
			t.Errorf("warning %d: %+v, want %s on line %d", i, got, w.rule, w.line)
		}
	}
	if m := b.Warnings[4].Message; m != "JSON arguments recommended for CMD to prevent unintended behavior related to OS signals" {
		t.Errorf("message %q: the rule name and the line belong elsewhere", m)
	}
	// The recorded CI builds break no rule.
	dirs, _ := filepath.Glob("testdata/records/*")
	for _, d := range dirs {
		lb, _ := os.ReadFile(filepath.Join(d, "logs.bin"))
		ps, err := DecodeProgress(bytes.NewReader(lb))
		if err != nil {
			t.Fatal(err)
		}
		if ws := warnings(ps); len(ws) != 0 {
			t.Errorf("%s: %+v", filepath.Base(d), ws)
		}
	}
}
