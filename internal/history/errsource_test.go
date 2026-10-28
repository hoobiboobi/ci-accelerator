package history

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Blobs from BuildKit v0.34.1 records of failed builds (buildctl against a
// plain daemon, 2026-10-28).
func TestParseErrorSource(t *testing.T) {
	for _, tc := range []struct {
		file   string
		vertex string
		ranges []LineRange
		first  string // the Dockerfile's first line
	}{
		{"run.bin", "sha256:6035a92b96c7dcf857a947860dac23578bb2b844e37ab3b50d789c03fa0ac54e", []LineRange{{7, 8}}, "FROM busybox:1.37 AS base"},
		{"copy.bin", "sha256:77f554e69f725712afb9280290a8e9b2f3478a276f42ad814b4d8ab80416641b", []LineRange{{3, 3}}, "FROM busybox:1.37"},
		{"syntax.bin", "sha256:ae9cfc04226d0c667f6f098c4db2fd80b6a59bbbd8d53701a434329666d6068b", []LineRange{{4, 4}}, "# syntax=docker/dockerfile:1.20"},
		{"parse.bin", "", []LineRange{{3, 3}}, "FROM busybox:1.37"},
	} {
		data, err := os.ReadFile(filepath.Join("testdata", "errors", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		es, err := ParseErrorSource(data)
		if err != nil || es == nil {
			t.Fatalf("%s: %v %v", tc.file, es, err)
		}
		if es.Name != "Dockerfile" || es.Vertex != tc.vertex || !reflect.DeepEqual(es.Ranges, tc.ranges) || !strings.HasPrefix(es.Text, tc.first+"\n") {
			t.Errorf("%s: got %q vertex %q ranges %v text %q", tc.file, es.Name, es.Vertex, es.Ranges, es.Text)
		}
	}
	data, _ := os.ReadFile(filepath.Join("testdata", "errors", "canceled.bin"))
	if es, err := ParseErrorSource(data); es != nil || err != nil {
		t.Errorf("canceled: %v %v", es, err)
	}
	if es, err := ParseErrorSource([]byte("not a status")); es != nil || err == nil {
		t.Errorf("garbage: %v %v", es, err)
	}
}

func TestFailedBuildDockerfile(t *testing.T) {
	d := filepath.Join("testdata", "failed-run")
	read := func(n string) []byte { return readOpt(t, filepath.Join(d, n)) }
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
	name, text := b.Dockerfile()
	if name != "Dockerfile" || !strings.Contains(text, "RUN echo first line && \\\n    exit 3\n") {
		t.Fatalf("Dockerfile %q %q", name, text)
	}
	var failed *Step
	for i := range b.Steps {
		s := &b.Steps[i]
		if s.Error != "" {
			if failed != nil {
				t.Fatalf("two failed steps: %s, %s", failed.Key, s.Key)
			}
			failed = s
		} else if s.Line != 0 || s.Lines != nil {
			// Only the failed step's lines are on record.
			t.Errorf("%s has lines %v", s.Key, s.Lines)
		}
	}
	if failed == nil || failed.Key != "[app 2/3]" || failed.Line != 7 || !reflect.DeepEqual(failed.Lines, []LineRange{{7, 8}}) {
		t.Fatalf("failed step %+v", failed)
	}
	// Without the error blob: as before, no Dockerfile.
	b, err = NewBuild(rec, progress, nil, nil, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, text := b.Dockerfile(); text != "" {
		t.Error("a Dockerfile without the error blob")
	}
	// A blob that does not parse leaves the build as it was.
	if _, err := NewBuild(rec, progress, nil, nil, []byte("junk"), Config{}); err != nil {
		t.Error(err)
	}
}
