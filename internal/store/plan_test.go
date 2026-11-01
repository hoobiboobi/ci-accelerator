package store

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The pipeline pages read a pipeline's jobs by (instance, pipeline_id).
// Without statistics SQLite chose the jobs table's primary-key prefix
// (instance) and read every job of the instance per pipeline: a search
// took seconds at a few hundred thousand jobs and held the connections the
// collector writes with. The queries name the index, and the store keeps
// statistics.
func TestPipelineQueriesUseJobsIndex(t *testing.T) {
	s := open(t, t.TempDir(), &clock{time.Now()})
	where, args := searchWhere("deploy")
	queries := map[string]struct {
		q    string
		args []any
	}{
		"search": {"SELECT pipeline_id FROM pipelines WHERE instance = ? AND project_id = ? AND " + where,
			append([]any{1, 1}, args...)},
		"jobs":  {"SELECT job_id FROM jobs INDEXED BY jobs_pipeline WHERE instance = ? AND pipeline_id = ? AND " + pipelinesOwn, []any{1, 1}},
		"count": {"SELECT count(*) FROM jobs INDEXED BY jobs_pipeline WHERE instance = ? AND pipeline_id = ? AND " + pipelinesOwn, []any{1, 1}},
	}
	for name, c := range queries {
		rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+c.q, c.args...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		got := strings.Join(plan, " | ")
		if strings.Contains(got, "jobs USING PRIMARY KEY") || !strings.Contains(got, "jobs_pipeline") {
			t.Errorf("%s: jobs not read through jobs_pipeline: %s", name, got)
		}
	}
	// The pages' own queries name the index: a plan test of a copy would
	// not see the index dropped from them.
	for src, want := range map[string]int{"pipelist.go": 1, "console.go": 1, "feed.go": 1, "pages.go": 1} {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(b), "INDEXED BY jobs_pipeline"); n < want {
			t.Errorf("%s: %d queries read jobs INDEXED BY jobs_pipeline, want %d", src, n, want)
		}
	}
}

// Open leaves statistics behind for tables with rows (PRAGMA optimize).
func TestOpenAnalyzes(t *testing.T) {
	dir := t.TempDir()
	c := &clock{time.Now()}
	s, err := Open(ctx, dir, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		if _, err := s.db.ExecContext(ctx, "INSERT INTO jobs (instance, job_id, pipeline_id, project_id) VALUES (1, ?, ?, 1)", i, i/20); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s = open(t, dir, c)
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_stat1 WHERE tbl = 'jobs'").Scan(&n); err != nil || n == 0 {
		t.Fatalf("no statistics for jobs after Open: %d rows, %v", n, err)
	}
}
