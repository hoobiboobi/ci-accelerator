package fleet

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// A file made by an earlier release lacks a column added since: opening it
// adds the column, with its default, and the registry works on it.
func TestOlderFileGetsNewColumns(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	// The members table as an earlier release had it: no removed_at, no
	// store_at.
	if _, err := db.ExecContext(ctx, `CREATE TABLE members (
	name        TEXT PRIMARY KEY,
	public_key  TEXT NOT NULL,
	fingerprint TEXT NOT NULL UNIQUE,
	origin      TEXT NOT NULL,
	added_at    INTEGER NOT NULL,
	added_by    TEXT NOT NULL
);
INSERT INTO members VALUES ('eu', 'k', 'SHA256:x', 'api', 1, 'token:t');`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	r, err := OpenRegistry(ctx, dir)
	if err != nil {
		t.Fatalf("opening an older file: %v", err)
	}
	defer r.Close()
	m, err := r.Get(ctx, "eu")
	if err != nil || m.Name != "eu" || !m.RemovedAt.IsZero() {
		t.Fatalf("the older file's member: %+v %v", m, err)
	}
	if ms, err := r.List(ctx); err != nil || len(ms) != 1 {
		t.Errorf("list: %v %v", ms, err)
	}
}

func TestDeclaredColumns(t *testing.T) {
	c := declaredColumns(schema)
	found := false
	for _, def := range c["members"] {
		if def == "removed_at  INTEGER NOT NULL DEFAULT 0" || def == "removed_at INTEGER NOT NULL DEFAULT 0" {
			found = true
		}
	}
	if !found || len(c["audit"]) == 0 {
		t.Errorf("declared columns: %v", c)
	}
}
