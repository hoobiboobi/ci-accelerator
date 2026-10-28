package history

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Every step a recorded build executed with processes (RUN) carries their
// CPU time from the cgroup counters; steps from the cache, or received from
// a build that ran them at the same time, carry none (no double count).
func TestStepUsage(t *testing.T) {
	dirs, _ := filepath.Glob("testdata/records/*")
	withCPU, cachedWithCPU := 0, 0
	for _, d := range dirs {
		read := func(n string) []byte {
			b, err := os.ReadFile(filepath.Join(d, n))
			if err != nil && !os.IsNotExist(err) {
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
		b, err := NewBuild(rec, progress, read("provenance.json"), read("trace.json"), nil, Config{})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range b.Steps {
			if s.Usage.CPU > 0 {
				if s.State != Built {
					cachedWithCPU++
					t.Logf("%s %s %s %q cpu=%s dur=%s", b.Ref, s.Key, s.State, s.Name, s.Usage.CPU, s.Duration())
				}
				withCPU++
				if s.Usage.CPU > s.Duration()*64 {
					t.Errorf("%s %s: %s of CPU in %s", b.Ref, s.Key, s.Usage.CPU, s.Duration())
				}
			}
		}
	}
	if withCPU == 0 || cachedWithCPU != 0 {
		t.Errorf("%d executed steps with CPU time, %d cached ones", withCPU, cachedWithCPU)
	}
}
