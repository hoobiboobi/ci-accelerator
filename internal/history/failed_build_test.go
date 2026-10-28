package history

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// failedFixture builds a record of testdata/<dir>: failed builds recorded
// from a BuildKit v0.34.1 daemon with buildctl (2026-10-28).
func failedFixture(t *testing.T, dir string) *Build {
	t.Helper()
	read := func(n string) []byte { return readOpt(t, filepath.Join("testdata", dir, n)) }
	rec, err := ParseRecord(read("record.json"))
	if err != nil {
		t.Fatal(err)
	}
	progress, err := DecodeProgress(bytes.NewReader(read("logs.bin")))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBuild(rec, progress, nil, nil, read("error.bin"), Config{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// "FROM busybox AS deps / RUN echo ok / RUN exit 5 / FROM deps AS test /
// RUN echo testing" built with --opt target=test: the test stage never
// ran, and only the failed step has lines.
func TestFailedTargetFixture(t *testing.T) {
	b := failedFixture(t, "failed-target")
	if _, text := b.Dockerfile(); b.Target != "test" || !strings.HasPrefix(text, "FROM busybox:1.37 AS deps\nRUN echo ok\nRUN exit 5\n") {
		t.Fatalf("target %q, Dockerfile %q", b.Target, text)
	}
	var keys []string
	for _, s := range b.Steps {
		keys = append(keys, s.Key)
		if (s.Error != "") != (s.Key == "[deps 3/3]") {
			t.Errorf("%s: error %q", s.Key, s.Error)
		}
		if s.Key == "[deps 3/3]" && !reflect.DeepEqual(s.Lines, []LineRange{{3, 3}}) {
			t.Errorf("failed step lines %v", s.Lines)
		}
	}
	if !reflect.DeepEqual(keys, []string{"[deps 1/3]", "[deps 2/3]", "[deps 3/3]"}) {
		t.Errorf("steps %v: the test stage never started", keys)
	}
}

func TestParseErrorLine(t *testing.T) {
	for msg, want := range map[string]int{
		"dockerfile parse error on line 3: unknown instruction: FOOBAR":                                  3,
		"dockerfile parse error on line 12: FROM requires either one or three arguments":                 12,
		`process "/bin/sh -c echo dockerfile parse error on line 2: x" did not complete successfully: 1`: 0,
		"failed to solve: dockerfile parse error on line 3: unknown instruction: FOOBAR":                 0,
		"dockerfile parse error on line 99999999999999999999: overflow":                                  0,
		"context canceled": 0,
		"":                 0,
	} {
		if got := ParseErrorLine(msg); got != want {
			t.Errorf("ParseErrorLine(%q) = %d, want %d", msg, got, want)
		}
	}
	// The recorded parse error: the line its message names is the line
	// its error blob ranges over.
	b := failedFixture(t, "failed-parse")
	data, err := os.ReadFile(filepath.Join("testdata", "failed-parse", "error.bin"))
	if err != nil {
		t.Fatal(err)
	}
	es, err := ParseErrorSource(data)
	if err != nil || es == nil || len(es.Ranges) != 1 {
		t.Fatalf("parse error source: %v %v", es, err)
	}
	if n := ParseErrorLine(b.Error); n != 3 || n != es.Ranges[0].Start || n != es.Ranges[0].End {
		t.Errorf("line %d, ranges %v (error %q)", n, es.Ranges, b.Error)
	}
	if _, text := b.Dockerfile(); len(b.Steps) != 0 || strings.Split(text, "\n")[2] != "FOOBAR this is not an instruction" {
		t.Errorf("%d steps, Dockerfile %q", len(b.Steps), text)
	}
}

// A canceled build (the job's timeout or cancel) keeps no Dockerfile:
// its error names no source.
func TestCanceledFixture(t *testing.T) {
	b := failedFixture(t, "failed-canceled")
	if _, text := b.Dockerfile(); text != "" || !Canceled(b.Error) || len(b.Steps) == 0 {
		t.Errorf("canceled: Dockerfile %q, error %q, %d steps", text, b.Error, len(b.Steps))
	}
}
