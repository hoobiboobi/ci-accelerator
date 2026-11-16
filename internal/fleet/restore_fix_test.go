package fleet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// A file may hold token columns of another type than the hub's (a crafted
// or damaged backup, restored): values such as text '00' or a far date as
// text compare one way in SQL and scan another way in Go. Such a token
// never opens, before or after VoidTokens, and VoidTokens voids it.

const (
	oddAPICols = `name TEXT PRIMARY KEY, role TEXT NOT NULL, hash TEXT NOT NULL UNIQUE, hint TEXT NOT NULL, created_at INTEGER NOT NULL,
		created_by TEXT NOT NULL, expires_at TEXT NOT NULL DEFAULT 0, used_at INTEGER NOT NULL DEFAULT 0, revoked_at INTEGER NOT NULL DEFAULT 0`
	oddJoinCols = `id INTEGER PRIMARY KEY, member TEXT NOT NULL, hash TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL,
		created_by TEXT NOT NULL, expires_at TEXT NOT NULL, used_at TEXT NOT NULL DEFAULT 0, revoked_at INTEGER NOT NULL DEFAULT 0`
)

type oddToken struct{ name, secret string }

// oddTokens rebuilds db's token tables with TEXT time columns and adds
// tokens whose values SQL and Go read differently.
func oddTokens(t *testing.T, db *sql.DB) (api, join []oddToken) {
	t.Helper()
	stmts := []string{
		`ALTER TABLE api_tokens RENAME TO old_api`,
		`CREATE TABLE api_tokens (` + oddAPICols + `)`,
		`INSERT INTO api_tokens SELECT * FROM old_api`,
		`DROP TABLE old_api`,
		`ALTER TABLE join_tokens RENAME TO old_join`,
		`CREATE TABLE join_tokens (` + oddJoinCols + `)`,
		`INSERT INTO join_tokens SELECT * FROM old_join`,
		`DROP TABLE old_join`,
	}
	for i, exp := range []string{`'00'`, `'100000000000000'`, `'0'`} {
		tok := oddToken{fmt.Sprintf("odd%d", i), fmt.Sprintf("%sodd-api-%d", TokenPrefix, i)}
		api = append(api, tok)
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO api_tokens (name, role, hash, hint, created_at, created_by, expires_at) VALUES ('%s', 'owner', '%s', 'xodd', 0, 'x', %s)`,
			tok.name, tokenHash(tok.secret), exp))
	}
	for i, ev := range [][2]string{{`4102444800000`, `'00'`}, {`'100000000000000'`, `0`}, {`'4102444800000'`, `'0'`}} {
		tok := oddToken{fmt.Sprintf("odd%d", i), fmt.Sprintf("%sodd-join-%d", JoinPrefix, i)}
		join = append(join, tok)
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO join_tokens (member, hash, created_at, created_by, expires_at, used_at) VALUES ('%s', '%s', 0, 'x', %s, %s)`,
			tok.name, joinHash(tok.secret), ev[0], ev[1]))
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return api, join
}

func checkOddTokensClosed(t *testing.T, r *Registry, when string, api, join []oddToken) {
	t.Helper()
	ctx := context.Background()
	for _, tok := range api {
		if p, ok := r.TokenPrincipal(ctx, tok.secret); ok {
			t.Errorf("%s: API token %s opens as %v", when, tok.name, p)
		}
	}
	for _, tok := range join {
		_, pub, err := GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		if m, err := r.Join(ctx, tok.name, tok.secret, string(pub)); !errors.Is(err, ErrDenied) {
			t.Errorf("%s: join token for %s: %v %v, want denied", when, tok.name, m.Name, err)
		}
	}
}

func checkOddTokensVoided(t *testing.T, db *sql.DB, when string) {
	t.Helper()
	for _, table := range []string{"api_tokens", "join_tokens"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table + ` WHERE NOT (typeof(revoked_at) = 'integer' AND revoked_at > 0)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s: %d rows of %s not voided", when, n, table)
		}
	}
}

func TestOddTokenValuesNeverOpen(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	r.Log = quiet
	api, join := oddTokens(t, r.db)
	checkOddTokensClosed(t, r, "before VoidTokens", api, join)
	if _, err := r.VoidTokens(ctx, OriginLocal, "test"); err != nil {
		t.Fatal(err)
	}
	checkOddTokensVoided(t, r.db, "after VoidTokens")
	checkOddTokensClosed(t, r, "after VoidTokens", api, join)
}

func TestRestoredOddTokensVoided(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	r.Log = quiet
	// The staged copy is voided at the real time.
	r.now = time.Now
	file := filepath.Join(t.TempDir(), "hub.db")
	if err := r.Backup(ctx, file); err != nil {
		t.Fatal(err)
	}
	bdb, err := sql.Open("sqlite", sqliteURI(file, nil))
	if err != nil {
		t.Fatal(err)
	}
	bdb.SetMaxOpenConns(1)
	api, join := oddTokens(t, bdb)
	if err := bdb.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := StageRestore(ctx, dir, file); err != nil {
		t.Fatal(err)
	}
	if _, applied, err := ApplyRestore(dir, time.Now()); err != nil || !applied {
		t.Fatalf("apply: %v %v", applied, err)
	}
	// Opened as the hub would, before its VoidTokens.
	back, err := OpenRegistry(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	back.Log = quiet
	checkOddTokensVoided(t, back.db, "staged")
	checkOddTokensClosed(t, back, "restored", api, join)
}
