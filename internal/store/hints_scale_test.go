package store

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestScaleStepRuns times the speed-ups' figures for an image with a month
// of builds (5000 of 30 steps): HINTS_SCALE=1. 67 ms on a laptop under
// load, 2026-11-04.
func TestScaleStepRuns(t *testing.T) {
	if os.Getenv("HINTS_SCALE") == "" {
		t.Skip()
	}
	now := time.Now()
	s := open(t, t.TempDir(), &clock{now})
	tx, _ := s.db.Begin()
	tx.Exec("INSERT INTO daemons (id, name) VALUES (1, 'd')")
	for b := 1; b <= 5000; b++ {
		if _, err := tx.Exec("INSERT INTO builds (id, daemon_id, ref, generation, attribution, instance, project_id, target, started_at) VALUES (?, 1, ?, 1, 1, 1, 42, 'api', ?)",
			b, fmt.Sprint("r", b), now.Add(-time.Duration(b)*8*time.Minute).UnixMilli()); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 30; i++ {
			tx.Exec("INSERT INTO steps (build_id, idx, key, name, identity, state, start_ms, dur_ms) VALUES (?, ?, 'k', ?, ?, ?, 0, 1000)",
				b, i, fmt.Sprint("RUN step ", i), i, b%3/2)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.db.Exec("PRAGMA optimize")
	t0 := time.Now()
	r, err := s.stepRuns(ctx, 1, []string{"RUN step 3", "RUN step 7", "RUN step 9"}, now.Add(-HintWindow), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%v for %d names, %d runs", time.Since(t0), len(r), len(r["RUN step 3"]))
}
