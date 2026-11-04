package hints

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// unplacedOnPurpose are the records whose cache folder the operation does
// not say: the step runs as a user whose home is only in the image's
// /etc/passwd (npm-user), or the manager keeps its cache somewhere that
// depends on its version and on running as root (Yarn 1 as root uses
// /usr/local/share/.cache/yarn). The hint must then say where to look,
// not guess.
var unplacedOnPurpose = map[string]bool{"npm-user": true, "yarn1": true}

// TestRealRecords reads the provenance of real builds, one per package
// manager (test/hints/managers.sh made them on kind: each install step
// without a cache mount), and checks the hints against what the manager
// itself reported in the same image: the cache folders they place hold
// the folders it named, and they find nothing else wrong.
func TestRealRecords(t *testing.T) {
	dirs, _ := filepath.Glob("testdata/records/*")
	if len(dirs) < 20 {
		t.Fatalf("%d records", len(dirs))
	}
	for _, d := range dirs {
		name := filepath.Base(d)
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(d, "provenance.json"))
			if err != nil {
				t.Fatal(err)
			}
			rep, _ := os.ReadFile(filepath.Join(d, "reported.txt"))
			op, anc, step, err := history.LastRun(data)
			if err != nil {
				t.Fatal(err)
			}
			hs := Analyze(op, anc)
			if len(hs) != 1 {
				t.Fatalf("%d hints: %v", len(hs), summary(hs))
			}
			h := hs[0]
			for _, p := range h.Problems {
				if p.Kind != Missing {
					t.Errorf("problem %+v on a step without cache mounts", p)
				}
			}
			if Fragment(step, hs) == "" {
				t.Errorf("no fragment for %q", step)
			}
			if unplacedOnPurpose[name] {
				if h.Caches[0].Path != "" || h.PathCommand == "" {
					t.Errorf("placed %q, command %q: want no path and a command", h.Caches[0].Path, h.PathCommand)
				}
				return
			}
			for _, want := range strings.Fields(string(rep)) {
				held := false
				for _, c := range h.Caches {
					held = held || c.Path != "" && within(want, c.Path)
				}
				if !held {
					t.Errorf("the manager reported %s; the hints place %v", want, summary(hs))
				}
			}
		})
	}
}
