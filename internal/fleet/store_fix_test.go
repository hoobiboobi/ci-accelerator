package fleet

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A removal takes the name's unused join tokens and its kept settings
// and secrets with it, on every path (the API, the pages, a command), also
// for a name that only waits to join.
func TestRemoveEndsJoinTokensAndSettings(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	_, secret, err := r.IssueJoin(ctx, "gitlab:g/1", "prod", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Registered another way meanwhile (by key), then removed.
	_, pub, _ := GenerateKey()
	if _, err := r.Add(ctx, "token:t", OriginAPI, "prod", string(pub)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO cluster_settings (member, params, version, created_at, created_by, updated_at, updated_by) VALUES ('prod', '{}', 1, 1, 'x', 1, 'x')`,
		`INSERT INTO cluster_secrets (member, param, sealed, updated_at, updated_by) VALUES ('prod', 'runner_token', x'00', 1, 'x')`,
	} {
		if _, err := r.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Remove(ctx, "token:t", "prod"); err != nil {
		t.Fatal(err)
	}
	_, attacker, _ := GenerateKey()
	if _, err := r.Join(ctx, "prod", secret, string(attacker)); !errors.Is(err, ErrDenied) {
		t.Errorf("the old join token after the removal: %v, want denied", err)
	}
	var n int
	r.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM cluster_settings WHERE member = 'prod') + (SELECT count(*) FROM cluster_secrets WHERE member = 'prod')`).Scan(&n)
	if n != 0 {
		t.Errorf("settings and secrets kept after the removal: %d rows", n)
	}

	// A name that only waits to join: no member, its token ended all the
	// same.
	_, pending, _ := r.IssueJoin(ctx, "gitlab:g/1", "pend", 0)
	if err := r.Remove(ctx, "token:t", "pend"); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing a name without a member: %v", err)
	}
	if _, err := r.Join(ctx, "pend", pending, string(attacker)); !errors.Is(err, ErrDenied) {
		t.Errorf("a pending name's token after the removal: %v, want denied", err)
	}
}

// A subject is stored as a signed-in person's subject is written: one with
// white space is refused, never stored as a binding that matches nobody.
func TestSubjectsAreExact(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	for _, bad := range []string{"gitlab:gitlab.example.com/5 ", " gitlab:gitlab.example.com/5", "gitlab:gitlab.example.com/5\n"} {
		if _, _, err := ParsePerson(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParsePerson(%q): %v", bad, err)
		}
		if _, err := r.Grant(ctx, "local", OriginLocal, bad, Owner); !errors.Is(err, ErrInvalid) {
			t.Errorf("Grant(%q): %v", bad, err)
		}
		if err := r.SyncOwners(ctx, []string{bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("SyncOwners(%q): %v", bad, err)
		}
	}
	if bs, _ := r.Bindings(ctx); len(bs) != 0 {
		t.Errorf("bindings stored: %+v", bs)
	}
}

// The only owner cannot leave the hub without an owner from the pages or
// the API; a command inside the hub's pod (the way back) can.
func TestLastOwnerStays(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	const ana, ben = "gitlab:gitlab.example.com/1", "gitlab:gitlab.example.com/2"
	if _, err := r.Grant(ctx, OriginLocal, OriginLocal, ana, Owner); err != nil {
		t.Fatal(err)
	}
	if err := r.Revoke(ctx, ana, ana); !errors.Is(err, ErrLocked) {
		t.Errorf("the only owner removes herself: %v", err)
	}
	if _, err := r.Grant(ctx, "token:t", OriginAPI, ana, Operator); !errors.Is(err, ErrLocked) {
		t.Errorf("the only owner made operator: %v", err)
	}
	if r.RoleOf(ana) != Owner {
		t.Fatalf("ana is %s", r.RoleOf(ana))
	}
	if _, err := r.Grant(ctx, ana, OriginUI, ben, Owner); err != nil {
		t.Fatal(err)
	}
	if err := r.Revoke(ctx, ana, ana); err != nil || r.RoleOf(ana) != Viewer {
		t.Errorf("an owner leaves while another stays: %v %s", err, r.RoleOf(ana))
	}
	if err := r.Revoke(ctx, OriginLocal, ben); err != nil {
		t.Errorf("hub revoke inside the pod: %v", err)
	}
}

// Granting a role the person has already answers the binding, also when
// another process (hub grant) wrote it and the maps have not read it yet.
func TestGrantSameRoleFromAnotherProcess(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	hub, err := OpenRegistry(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	cmd, err := OpenRegistry(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	const s = "gitlab:gitlab.example.com/5"
	if _, err := cmd.Grant(ctx, OriginLocal, OriginLocal, s, Operator); err != nil {
		t.Fatal(err)
	}
	cmd.Close()
	b, err := hub.Grant(ctx, "token:t", OriginAPI, s, Operator)
	if err != nil || b.Role != Operator || b.Origin != OriginLocal || b.AddedBy != OriginLocal || b.AddedAt.IsZero() {
		t.Errorf("the same role again: %+v %v", b, err)
	}
}

// A limit above the maximum gives the maximum.
func TestAuditLimitClamped(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 1005 {
		if err := audit(ctx, tx, r.now(), "x", "test", fmt.Sprint(i), ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ limit, want int }{{0, 100}, {1000, 1000}, {1001, 1000}, {5000, 1000}} {
		if es, _ := r.AuditQuery(ctx, AuditQuery{Limit: c.limit}); len(es) != c.want {
			t.Errorf("limit %d: %d records, want %d", c.limit, len(es), c.want)
		}
	}
}

// The join endpoint says nothing about a key to a caller without a token
// issued for the name.
func TestJoinChecksTheTokenFirst(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	_, pub, _ := GenerateKey()
	if _, err := r.Add(ctx, "token:t", OriginAPI, "secret-name", string(pub)); err != nil {
		t.Fatal(err)
	}
	_, other, _ := r.IssueJoin(ctx, "x", "eu", 0)
	for _, c := range []struct{ name, token string }{{"guess", ""}, {"secret-name", ""}, {"secret-name", "cia_join_x"}, {"guess", other}} {
		m, err := r.Join(ctx, c.name, c.token, string(pub))
		if !errors.Is(err, ErrDenied) || strings.Contains(fmt.Sprint(err), "secret-name") {
			t.Errorf("join %q with token %q: %+v %v, want denied", c.name, c.token, m, err)
		}
	}
}

// The token's use and the registration are one transaction: when the
// registration fails, the token is unused, whatever else fails then.
func TestJoinIsOneTransaction(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	_, secret, _ := r.IssueJoin(ctx, "x", "eu", 0)
	for _, q := range []string{
		// The registration fails...
		`CREATE TRIGGER no_members BEFORE INSERT ON members BEGIN SELECT RAISE(ABORT, 'disk full'); END`,
		// ...and so does any later write that would hand the token back
		// (a hub that stops in between writes nothing more).
		`CREATE TRIGGER no_reset BEFORE UPDATE OF used_at ON join_tokens WHEN NEW.used_at = 0 BEGIN SELECT RAISE(ABORT, 'stopped'); END`,
	} {
		if _, err := r.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	_, pub, _ := GenerateKey()
	if _, err := r.Join(ctx, "eu", secret, string(pub)); err == nil {
		t.Fatal("joined although the registration failed")
	}
	if js, _ := r.JoinTokens(ctx, "eu"); len(js) != 1 || !js[0].UsedAt.IsZero() {
		t.Errorf("a join that did not register spent its token: %+v", js)
	}
}

// A reload never installs an older read over a newer one: once Remove or
// Add returns, the key's state is the file's, whatever Refresh does
// meanwhile.
func TestReloadsDoNotGoBack(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					r.Refresh(ctx)
				}
			}
		}()
	}
	var stale []string
	for i := range 1500 {
		_, pub, _ := GenerateKey()
		m, err := r.Add(ctx, "x", OriginAPI, "m", string(pub))
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := r.Holder(m.Fingerprint); n != "m" {
			stale = append(stale, fmt.Sprintf("round %d: added key refused", i))
		}
		if err := r.Remove(ctx, "x", "m"); err != nil {
			t.Fatal(err)
		}
		if n, _ := r.Holder(m.Fingerprint); n != "" {
			stale = append(stale, fmt.Sprintf("round %d: removed key accepted", i))
		}
	}
	close(stop)
	wg.Wait()
	if len(stale) > 0 {
		t.Errorf("%d stale reads, first: %s", len(stale), stale[0])
	}
}

// Prune keeps what is still in its keep: recent audit records, a join
// token still valid; a negative keep deletes nothing still valid.
func TestPruneKeepsWhatIsLive(t *testing.T) {
	ctx := context.Background()
	r, now := openReg(t)
	_, secret, _ := r.IssueJoin(ctx, "x", "eu", 7*24*time.Hour)
	_, api, _ := r.IssueToken(ctx, "x", "ci", Operator, 7*24*time.Hour)
	*now = now.Add(time.Hour)
	if err := r.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if es, _ := r.AuditQuery(ctx, AuditQuery{}); len(es) != 2 {
		t.Errorf("recent audit records pruned: %d left", len(es))
	}
	r.KeepRemoved = -10 * 24 * time.Hour
	if err := r.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.TokenPrincipal(ctx, api); !ok {
		t.Error("a valid API token pruned")
	}
	_, pub, _ := GenerateKey()
	if _, err := r.Join(ctx, "eu", secret, string(pub)); err != nil {
		t.Errorf("a valid join token pruned: %v", err)
	}
	r.KeepRemoved = 30 * 24 * time.Hour
	*now = now.Add(r.KeepAudit + time.Hour)
	if err := r.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if es, _ := r.AuditQuery(ctx, AuditQuery{}); len(es) != 0 {
		t.Errorf("audit records past their keep: %d left", len(es))
	}
}

// A removed cluster's key joins again under another name (a rename with
// the box's kept key Secret).
func TestRemovedKeyTakenOver(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	_, pub, _ := GenerateKey()
	if _, err := r.Add(ctx, "x", OriginAPI, "prod-eu", string(pub)); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove(ctx, "x", "prod-eu"); err != nil {
		t.Fatal(err)
	}
	_, secret, _ := r.IssueJoin(ctx, "x", "prod-de", 0)
	m, err := r.Join(ctx, "prod-de", secret, string(pub))
	if err != nil || m.Name != "prod-de" {
		t.Fatalf("join under the new name with the kept key: %+v %v", m, err)
	}
	if n, _ := r.Holder(m.Fingerprint); n != "prod-de" {
		t.Errorf("holder %q", n)
	}
	// An active member's key stays its own.
	if _, err := r.Add(ctx, "x", OriginAPI, "prod-us", string(pub)); !errors.Is(err, ErrExists) {
		t.Errorf("an active member's key under another name: %v", err)
	}
}

// fleet.db and a backup are the hub user's only; a data directory or
// backup path with '?' or '#' is used as it is named.
func TestFilesModesAndOddPaths(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "odd?name#1 %41")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRegistry(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, pub, _ := GenerateKey()
	if _, err := r.Add(ctx, "x", OriginAPI, "eu", string(pub)); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "fleet.db"))
	if err != nil {
		t.Fatalf("fleet.db is not in the data directory: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("fleet.db mode %v", st.Mode().Perm())
	}
	if ents, _ := os.ReadDir(filepath.Dir(dir)); len(ents) != 1 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("files beside the data directory: %v", names)
	}
	backup := filepath.Join(t.TempDir(), "b?ck#up.db")
	if err := r.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(backup); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("backup: %v %v", st, err)
	}
	if err := StageRestore(ctx, t.TempDir(), backup); err != nil {
		t.Errorf("staging the backup: %v", err)
	}
}

// A backup holding a trigger or a view is refused: a restored file must
// not run statements of its own inside the hub's.
func TestRestoreRefusesTriggers(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	for i, q := range []string{
		`CREATE TRIGGER keep BEFORE UPDATE ON api_tokens BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE VIEW v AS SELECT * FROM members`,
	} {
		file := filepath.Join(t.TempDir(), "hub.db")
		if err := r.Backup(ctx, file); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
		db.Close()
		if err := StageRestore(ctx, t.TempDir(), file); err == nil {
			t.Errorf("case %d: a backup with %q staged", i, q)
		}
	}
}

// A relative data directory (a hub started by hand) works as before.
func TestRelativeDataDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir("data", 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRegistry(context.Background(), "data")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Record(context.Background(), "x", "test", "hub", ""); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir("."); len(ents) != 1 {
		t.Errorf("files beside the data directory: %d", len(ents))
	}
	if _, err := os.Stat(filepath.Join("data", "fleet.db-wal")); err != nil {
		t.Errorf("the journal is not in the data directory: %v", err)
	}
}

// A staged restore holds no live token: a hub that stops between the
// restore and its own void does not bring a revoked token back.
func TestStagedRestoreHoldsNoTokens(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	// The staged copy is voided at the real time.
	r.now = time.Now
	_, api, err := r.IssueToken(ctx, "x", "terraform", Owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, join, _ := r.IssueJoin(ctx, "x", "eu", 0)
	file := filepath.Join(t.TempDir(), "hub.db")
	if err := r.Backup(ctx, file); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := StageRestore(ctx, dir, file); err != nil {
		t.Fatal(err)
	}
	if _, applied, err := ApplyRestore(dir, time.Now()); err != nil || !applied {
		t.Fatalf("apply: %v %v", applied, err)
	}
	// Opened as the hub would, without its VoidTokens.
	back, err := OpenRegistry(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if _, ok := back.TokenPrincipal(ctx, api); ok {
		t.Error("the backup's API token opens before the hub voided it")
	}
	_, pub, _ := GenerateKey()
	if _, err := back.Join(ctx, "eu", join, string(pub)); !errors.Is(err, ErrDenied) {
		t.Errorf("the backup's join token: %v, want denied", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 3 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("files in the data directory: %v", names)
	}
}

// Staging a registry's file while it is open takes what its journal holds
// too; a valid SQLite file without the hub's tables is refused.
func TestStageRestoreFiles(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	r, err := OpenRegistry(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, pub, _ := GenerateKey()
	if _, err := r.Add(ctx, "x", OriginAPI, "eu", string(pub)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "fleet.db-wal")); err != nil {
		t.Fatalf("no journal: the test proves nothing: %v", err)
	}
	dir := t.TempDir()
	if err := StageRestore(ctx, dir, filepath.Join(src, "fleet.db")); err != nil {
		t.Fatal(err)
	}
	if _, applied, err := ApplyRestore(dir, time.Now()); err != nil || !applied {
		t.Fatalf("apply: %v %v", applied, err)
	}
	back, err := OpenRegistry(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if ms, _ := back.List(ctx); len(ms) != 1 || ms[0].Name != "eu" {
		t.Errorf("members of the staged open file: %+v", ms)
	}

	other := filepath.Join(t.TempDir(), "other.db")
	db, err := sql.Open("sqlite", other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE notes (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	empty := t.TempDir()
	if err := StageRestore(ctx, empty, other); err == nil {
		t.Error("a database without the hub's tables staged")
	}
	if ents, _ := os.ReadDir(empty); len(ents) != 0 {
		t.Errorf("a refused restore left %d files", len(ents))
	}
}

// ActiveJoins lists every name's join tokens that can still be traded.
func TestActiveJoins(t *testing.T) {
	ctx := context.Background()
	r, now := openReg(t)
	r.IssueJoin(ctx, "x", "b", 0)
	r.IssueJoin(ctx, "x", "b", 0) // revokes the first
	r.IssueJoin(ctx, "x", "a", 0)
	_, used, _ := r.IssueJoin(ctx, "x", "used", 0)
	_, pub, _ := GenerateKey()
	if _, err := r.Join(ctx, "used", used, string(pub)); err != nil {
		t.Fatal(err)
	}
	r.IssueJoin(ctx, "x", "old", time.Minute)
	*now = now.Add(30 * time.Minute)
	js, err := r.ActiveJoins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, j := range js {
		got = append(got, fmt.Sprintf("%s#%d", j.Member, j.ID))
	}
	if strings.Join(got, " ") != "a#3 b#2" {
		t.Errorf("active join tokens: %v", got)
	}
}

// The audit's actor and target filters read an index, not the table.
func TestAuditFiltersUseIndexes(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	for col, index := range map[string]string{"actor": "audit_actor", "target": "audit_target"} {
		var plan []string
		rows, err := r.db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT id FROM audit WHERE `+col+` = ? AND id < ? ORDER BY id DESC LIMIT 10`, "x", 5)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			rows.Scan(&id, &parent, &unused, &detail)
			plan = append(plan, detail)
		}
		rows.Close()
		if p := strings.Join(plan, "; "); !strings.Contains(p, index) {
			t.Errorf("%s filter: %s", col, p)
		}
	}
}

// TokenPrincipal logs what it cannot read or write instead of dropping it.
func TestTokenPrincipalLogs(t *testing.T) {
	ctx := context.Background()
	r, now := openReg(t)
	var buf bytes.Buffer
	r.Log = slog.New(slog.NewTextHandler(&buf, nil))
	_, secret, _ := r.IssueToken(ctx, "x", "terraform", Owner, 0)
	if _, err := r.db.ExecContext(ctx, `CREATE TRIGGER no_use BEFORE UPDATE OF used_at ON api_tokens BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	if _, ok := r.TokenPrincipal(ctx, secret); !ok {
		t.Fatal("a token whose use cannot be recorded stops working")
	}
	if !strings.Contains(buf.String(), "disk full") {
		t.Errorf("the failed write of the use is not logged: %q", buf.String())
	}
	buf.Reset()
	if _, err := r.db.ExecContext(ctx, `ALTER TABLE api_tokens RENAME TO gone`); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.TokenPrincipal(ctx, secret); ok {
		t.Error("a token opens without its table")
	}
	if !strings.Contains(buf.String(), "no such table") {
		t.Errorf("the failed read is not logged: %q", buf.String())
	}
}
