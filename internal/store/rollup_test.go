package store

import (
	"testing"
	"time"
)

// The images' daily figures add up to the builds' outcomes, survive the
// builds' retention, and go after a year.
func TestRollup(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	savedFixtures(t, s)
	projects, err := s.Projects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects %+v %v", projects, err)
	}
	key := projects[0].ProjectKey
	all := mustBuilds(t, s, BuildQuery{Project: key})
	want := map[string]map[string]int{} // target -> outcome -> builds
	for _, o := range []string{"cached", "change", "farm", "unclear", "failed"} {
		for _, b := range mustBuilds(t, s, BuildQuery{Project: key, Outcome: o}) {
			if want[b.Target] == nil {
				want[b.Target] = map[string]int{}
			}
			want[b.Target][o]++
		}
	}
	first := all[len(all)-1].Started
	c.t = all[0].Started.AddDate(0, 0, 2)
	for range 2 { // summing up again changes nothing
		if err := s.Rollup(ctx); err != nil {
			t.Fatal(err)
		}
	}
	check := func(when string) {
		t.Helper()
		total := 0
		for target, w := range want {
			days, err := s.ImageDays(ctx, key, target, first.AddDate(0, 0, -1))
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]int{}
			for _, d := range days {
				got["cached"] += d.Cached
				got["change"] += d.Change
				got["farm"] += d.Farm
				got["unclear"] += d.Unclear
				got["failed"] += d.Fail
				total += d.Builds
				if d.Builds != d.Cached+d.Change+d.Farm+d.Unclear+d.Fail || d.Rebuilt() != d.Builds-d.Cached {
					t.Errorf("%s: %s %s does not add up: %+v", when, target, d.Day, d)
				}
				if d.Rebuilt() > d.Fail && (d.RebuiltP50 <= 0 || d.RebuiltP95 < d.RebuiltP50) {
					t.Errorf("%s: %s %s rebuild times %+v", when, target, d.Day, d)
				}
			}
			for o, n := range w {
				if got[o] != n {
					t.Errorf("%s: %s %s: %d, want %d", when, target, o, got[o], n)
				}
			}
		}
		if total != len(all) {
			t.Errorf("%s: %d builds in the daily figures, want %d", when, total, len(all))
		}
	}
	check("summed up")

	c.t = c.t.AddDate(0, 0, 40)
	if _, err := s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if left := mustBuilds(t, s, BuildQuery{Project: key}); len(left) != 0 {
		t.Errorf("after 40 days: %d builds left", len(left))
	}
	check("after the builds' retention")

	c.t = c.t.AddDate(1, 0, 0)
	if _, err := s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM image_days").Scan(&n); err != nil || n != 0 {
		t.Errorf("after a year: %d daily figures left, %v", n, err)
	}
}

// The daily figures name every finished build's outcome as the lists'
// filters select it: exactly one filter matches, and it is the same.
func TestRollupOutcomesAgreeWithFilters(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	if _, err := s.db.Exec("CREATE TABLE combos (error TEXT, reason_kind TEXT, built INTEGER, completed_at INTEGER)"); err != nil {
		t.Fatal(err)
	}
	kinds := []string{"", "first-build", "step-changed", "new-step", "arg-changed", "base-image-changed", "files-changed",
		"other-node", "not-in-cache", "cache-elsewhere"}
	for _, e := range []string{"", "failed to solve"} {
		for _, k := range kinds {
			for _, built := range []int{0, 2} {
				if _, err := s.db.Exec("INSERT INTO combos VALUES (?, ?, ?, 1)", e, k, built); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	rows, err := s.db.Query("SELECT error, reason_kind, built, " + statusCase + " FROM combos b")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			e, k, got string
			built     int
		)
		if err := rows.Scan(&e, &k, &built, &got); err != nil {
			t.Fatal(err)
		}
		var match []string
		for _, o := range []string{"cached", "change", "farm", "unclear", "failed"} {
			var n int
			if err := s.db.QueryRow("SELECT count(*) FROM combos b WHERE error = ? AND reason_kind = ? AND built = ? AND "+outcomes[o], e, k, built).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n > 0 {
				match = append(match, o)
			}
		}
		if len(match) != 1 || match[0] != got {
			t.Errorf("error %q, reason %q, built %d: daily figures say %s, filters %v", e, k, built, got, match)
		}
	}
}
