package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// "Mine" finds a person's builds by the commit author's name, whatever
// alphabet it is written in (CI_COMMIT_AUTHOR is "Name <email>").
func TestAuthorFilterAnyAlphabet(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	var key ProjectKey
	for _, b := range bs {
		a := b.Attribution
		if a.State != history.Attributed {
			continue
		}
		key = ProjectKey{Instance: a.Instance, ID: 42}
		if err := s.SaveJob(ctx, Job{Instance: a.Instance, JobID: a.Job, PipelineID: a.Pipeline, ProjectID: 42,
			ProjectPath: "example/app", Name: "build", CommitAuthor: "Алексей Бондаренко <a@example.com>"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	for _, who := range []string{"Алексей Бондаренко", "Алексей Бондаренко <a@example.com>"} {
		rows, err := s.Builds(ctx, BuildQuery{Project: key, Author: who})
		if err != nil || len(rows) == 0 {
			t.Errorf("builds of %q: %d (%v)", who, len(rows), err)
		}
		pipes, err := s.PipelineList(ctx, PipeQuery{Project: key, Author: who})
		if err != nil || len(pipes) == 0 {
			t.Errorf("pipelines of %q: %d (%v)", who, len(pipes), err)
		}
	}
	if rows, _ := s.Builds(ctx, BuildQuery{Project: key, Author: "Алексей"}); len(rows) != 0 {
		t.Errorf("a first name alone matched %d builds", len(rows))
	}
	var v int
	if err := s.db.QueryRow("PRAGMA auto_vacuum").Scan(&v); err != nil || v != 2 {
		t.Errorf("auto_vacuum %d (%v), want 2 (incremental): the file would never shrink", v, err)
	}
}

// Pages of pipelines meet where the previous one ended, by time and then
// number: pipelines created in the same second (a tag and its branch) are
// on one page or the next, never on neither.
func TestPipelinePagesKeepTies(t *testing.T) {
	s := open(t, t.TempDir(), &clock{time.Now()})
	key := ProjectKey{Instance: "0a1b2c", ID: 42}
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for id := uint64(1); id <= 7; id++ {
		created := at.Add(time.Duration(id) * time.Second)
		if id >= 3 && id <= 5 {
			created = at.Add(5 * time.Second) // three in one second
		}
		if _, err := s.db.Exec(`INSERT INTO pipelines (instance, pipeline_id, project_id, created_at) VALUES (?, ?, 42, ?)`,
			nullInstance(key.Instance), id, created.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	var seen []uint64
	q := PipeQuery{Project: key, Limit: 2}
	for page := 0; page < 10; page++ {
		rows, err := s.PipelineList(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			seen = append(seen, r.ID)
		}
		if len(rows) < q.Limit {
			break
		}
		last := rows[len(rows)-1]
		q.Before, q.BeforeID = last.Created, last.ID
	}
	if fmt.Sprint(seen) != "[7 6 5 4 3 2 1]" {
		t.Errorf("pages: %v", seen)
	}
}
