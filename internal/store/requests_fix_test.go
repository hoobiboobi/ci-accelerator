package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// insBuild stores a finished build of project 42 (instance 000001) directly,
// with an error text of size bytes.
func insBuild(t *testing.T, s *Store, id int, started time.Time, size int) {
	t.Helper()
	ts := started.UnixMilli()
	if _, err := s.db.Exec(`INSERT INTO builds (id, daemon_id, ref, generation, attribution, instance, project_id, target, started_at, completed_at, error, built)
		VALUES (?, 1, ?, 1, 1, 1, 42, 'api', ?, ?, ?, 1)`, id, fmt.Sprint("r", id), ts, ts+1000, strings.Repeat("x", size)); err != nil {
		t.Fatal(err)
	}
}

func insDaemon(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec("INSERT INTO daemons (id, name) VALUES (1, 'd')"); err != nil {
		t.Fatal(err)
	}
}

// Builds that started in the same millisecond as the last one of a page
// are on the next page.
func TestBuildsPageSameMillisecond(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), &clock{now})
	insDaemon(t, s)
	at := now.Add(-time.Hour)
	for id := 1; id <= 3; id++ {
		insBuild(t, s, id, at, 1)
	}
	key := ProjectKey{Instance: "000001", ID: 42}
	ids := func(q BuildQuery) []int64 {
		t.Helper()
		rows, err := s.Builds(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	first := ids(BuildQuery{Project: key, Limit: 2})
	if fmt.Sprint(first) != "[3 2]" {
		t.Fatalf("first page %v, want [3 2]", first)
	}
	if got := ids(BuildQuery{Project: key, Limit: 2, Before: at, BeforeID: first[1]}); fmt.Sprint(got) != "[1]" {
		t.Errorf("next page %v, want [1]", got)
	}
	// Without the id, Before alone still excludes its millisecond.
	if got := ids(BuildQuery{Project: key, Before: at}); len(got) != 0 {
		t.Errorf("before the time alone: %v", got)
	}
}

// GitLab's commit of a pipeline is stored when nothing named one before,
// and never replaces one.
func TestSaveAPIPipelineCommit(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), &clock{now})
	sha := func() string {
		t.Helper()
		p, err := s.PipelineInfo(ctx, "000001", 5)
		if err != nil || p == nil {
			t.Fatalf("pipeline: %v %v", p, err)
		}
		return p.SHA
	}
	if _, err := s.SaveAPIPipeline(ctx, APIPipeline{Instance: "000001", ID: 5, ProjectID: 42, Status: "running", Read: now}); err != nil {
		t.Fatal(err)
	}
	if got := sha(); got != "" {
		t.Errorf("no commit given: %q", got)
	}
	if _, err := s.SaveAPIPipeline(ctx, APIPipeline{Instance: "000001", ID: 5, ProjectID: 42, Status: "running", SHA: "aaaa1111", Read: now}); err != nil {
		t.Fatal(err)
	}
	if got := sha(); got != "aaaa1111" {
		t.Errorf("the commit GitLab named: %q", got)
	}
	if _, err := s.SaveAPIPipeline(ctx, APIPipeline{Instance: "000001", ID: 5, ProjectID: 42, Status: "success", SHA: "bbbb2222", Read: now}); err != nil {
		t.Fatal(err)
	}
	if got := sha(); got != "aaaa1111" {
		t.Errorf("a stored commit was replaced: %q", got)
	}
}

// Above Options.MaxBytes retention deletes the oldest builds of days
// summed up, keeps today's and the daily figures as summed, and says so.
func TestPruneToSize(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	c := &clock{now}
	s := open(t, t.TempDir(), c)
	insDaemon(t, s)
	id := 0
	for d := 6; d >= 1; d-- {
		for i := range 10 {
			id++
			insBuild(t, s, id, now.Add(-time.Duration(d)*24*time.Hour+time.Duration(i)*time.Minute), 40<<10)
		}
	}
	for i := range 5 {
		id++
		insBuild(t, s, id, now.Add(-time.Hour+time.Duration(i)*time.Minute), 40<<10)
	}
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	used, err := s.dbUsed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Without a bound nothing goes.
	if n, err := s.Prune(ctx); n != 0 || err != nil {
		t.Fatalf("no bound: %d deleted, %v", n, err)
	}
	s.opts.MaxBytes = used / 2
	n, err := s.Prune(ctx)
	if err == nil || !strings.Contains(err.Error(), "over its bound") {
		t.Errorf("the early deletion is not reported: %v", err)
	}
	if n == 0 {
		t.Fatal("nothing deleted above the bound")
	}
	if after, err := s.dbUsed(ctx); err != nil || after > s.opts.MaxBytes {
		t.Errorf("the database holds %d bytes, bound %d (%v)", after, s.opts.MaxBytes, err)
	}
	var kept, oldest, today int64
	if err := s.db.QueryRow("SELECT count(*), min(id), count(*) FILTER (WHERE started_at >= ?) FROM builds", dayOf(now)*dayMs).Scan(&kept, &oldest, &today); err != nil {
		t.Fatal(err)
	}
	if today != 5 {
		t.Errorf("%d of today's 5 builds kept", today)
	}
	if kept != int64(id)-oldest+1 || kept+n != int64(id) {
		t.Errorf("not the oldest went: %d kept from id %d, %d deleted, of %d", kept, oldest, n, id)
	}
	// The days cut into keep the figures summed with all their builds,
	// also when they are summed up again.
	c.t = now.Add(24 * time.Hour)
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	days, err := s.ImageDays(ctx, ProjectKey{Instance: "000001", ID: 42}, "api", now.Add(-10*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range days {
		got = append(got, fmt.Sprintf("%s:%d", d.Day.Format("01-02"), d.Builds))
	}
	if want := "11-06:10 11-07:10 11-08:10 11-09:10 11-10:10 11-11:10 11-12:5"; strings.Join(got, " ") != want {
		t.Errorf("daily figures %v, want %s", got, want)
	}
	// Only today's builds over the bound: they stay, and the error says
	// the bound is not kept.
	c.t = now
	s.opts.MaxBytes = 1
	if _, err := s.Prune(ctx); err == nil || !strings.Contains(err.Error(), "not summed up yet") {
		t.Errorf("a bound that cannot be kept: %v", err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM builds").Scan(&kept); err != nil || kept != 5 {
		t.Errorf("%d builds left, want today's 5 (%v)", kept, err)
	}
}

// A failed listing keeps the time of the last one that succeeded, the
// figures' time, and records its own.
func TestCacheScanFailedKeepsListed(t *testing.T) {
	t1 := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), &clock{t1})
	key := ProjectKey{Instance: "000001", ID: 42}
	state := func() CacheStore {
		t.Helper()
		_, stores, err := s.ProjectCache(ctx, key)
		if err != nil || len(stores) != 1 {
			t.Fatalf("stores: %+v %v", stores, err)
		}
		return stores[0]
	}
	objs := []CacheObject{{ProjectID: 42, Key: "deps", Bytes: 10, Modified: t1}}
	if err := s.ReplaceCacheScan(ctx, "b", "000001", t1, objs, nil, nil); err != nil {
		t.Fatal(err)
	}
	t2 := t1.Add(time.Hour)
	if err := s.ReplaceCacheScan(ctx, "b", "000001", t2, nil, nil, errors.New("AccessDenied")); err != nil {
		t.Fatal(err)
	}
	if c := state(); !c.Scanned.Equal(t1) || !c.Attempted.Equal(t2) || c.Err != "AccessDenied" {
		t.Errorf("after a failed listing: %+v", c)
	}
	t3 := t2.Add(time.Hour)
	if err := s.ReplaceCacheScan(ctx, "b", "000001", t3, objs, nil, nil); err != nil {
		t.Fatal(err)
	}
	if c := state(); !c.Scanned.Equal(t3) || !c.Attempted.Equal(t3) || c.Err != "" {
		t.Errorf("after a listing that succeeded: %+v", c)
	}
	// A row an older release wrote: its scanned_at is the last attempt.
	if _, err := s.db.Exec("UPDATE cache_stores SET attempted_at = NULL"); err != nil {
		t.Fatal(err)
	}
	if c := state(); !c.Attempted.Equal(t3) {
		t.Errorf("an older release's row: %+v", c)
	}
	// A folder whose first listing fails was never listed.
	if err := s.ReplaceCacheScan(ctx, "c", "000001", t3, nil, nil, errors.New("timeout")); err != nil {
		t.Fatal(err)
	}
	_, stores, err := s.ProjectCache(ctx, ProjectKey{Instance: "000001", ID: 43})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range stores {
		if c.ID == "c" && (!c.Scanned.IsZero() || !c.Attempted.Equal(t3)) {
			t.Errorf("never listed: %+v", c)
		}
	}
}
