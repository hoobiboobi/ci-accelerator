package history

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The GitLab URL test/history/generate.sh uses.
var testInstances = map[string]bool{InstancePrefix("https://gitlab.example.com/"): true}

func readOpt(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// loadFixtures builds every record of testdata/records (one daemon), linked,
// in the order they were created.
func loadFixtures(t *testing.T) []*Build {
	t.Helper()
	dirs, err := filepath.Glob("testdata/records/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	var out []*Build
	for _, d := range dirs {
		rec, err := ParseRecord(readOpt(t, filepath.Join(d, "record.json")))
		if err != nil {
			t.Fatal(err)
		}
		progress, err := DecodeProgress(bytes.NewReader(readOpt(t, filepath.Join(d, "logs.bin"))))
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		b, err := NewBuild(rec, progress, readOpt(t, filepath.Join(d, "provenance.json")), readOpt(t, filepath.Join(d, "trace.json")), testInstances)
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	Link(out)
	return out
}
