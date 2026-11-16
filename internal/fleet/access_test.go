package fleet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRoles(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	const mira, jonas, ana = "gitlab:gitlab.example.com/7", "gitlab:gitlab.example.com/8", "gitlab:gitlab.com/9"

	if r.RoleOf(mira) != Viewer {
		t.Fatal("nobody has a role before anything is given")
	}
	if err := r.SyncOwners(ctx, []string{mira}); err != nil {
		t.Fatal(err)
	}
	if r.RoleOf(mira) != Owner {
		t.Fatalf("an owner from the values: %s", r.RoleOf(mira))
	}
	// Locked: the values own it.
	if _, err := r.Grant(ctx, "token:t", OriginAPI, mira, Operator); !errors.Is(err, ErrLocked) {
		t.Errorf("grant over the values: %v", err)
	}
	if err := r.Revoke(ctx, "token:t", mira); !errors.Is(err, ErrLocked) {
		t.Errorf("revoke of the values: %v", err)
	}

	b, err := r.Grant(ctx, mira, OriginUI, jonas, Operator)
	if err != nil || b.Role != Operator || b.Origin != OriginUI || b.AddedBy != mira {
		t.Fatalf("grant: %+v %v", b, err)
	}
	if !r.RoleOf(jonas).Allows(PermMembersWrite) || r.RoleOf(jonas).Allows(PermRolesWrite) || r.RoleOf(jonas).Allows(PermTokensWrite) {
		t.Errorf("an operator's permissions are wrong")
	}
	if _, err := r.Grant(ctx, mira, OriginUI, jonas, Owner); err != nil || r.RoleOf(jonas) != Owner {
		t.Errorf("to owner: %v %s", err, r.RoleOf(jonas))
	}
	// Viewer is no binding.
	if _, err := r.Grant(ctx, mira, OriginUI, jonas, Viewer); err != nil || r.RoleOf(jonas) != Viewer {
		t.Errorf("to viewer: %v %s", err, r.RoleOf(jonas))
	}
	bs, _ := r.Bindings(ctx)
	if len(bs) != 1 || bs[0].Subject != mira {
		t.Errorf("bindings after the viewer: %+v", bs)
	}
	for _, bad := range []string{"gitlab:gitlab.com/0", "gitlab:gitlab.com/x", "token:x", "gitlab:/5", "jonas"} {
		if _, err := r.Grant(ctx, mira, OriginUI, bad, Operator); !errors.Is(err, ErrInvalid) {
			t.Errorf("grant %q: %v", bad, err)
		}
	}
	if _, err := r.Grant(ctx, mira, OriginUI, ana, Role("admin")); !errors.Is(err, ErrInvalid) {
		t.Errorf("an unknown role: %v", err)
	}

	// A person bound in the UI, then named in the values: the values take
	// it over; dropped from the values: gone.
	if _, err := r.Grant(ctx, mira, OriginUI, ana, Operator); err != nil {
		t.Fatal(err)
	}
	if err := r.SyncOwners(ctx, []string{mira, ana}); err != nil {
		t.Fatal(err)
	}
	if b, _ := r.binding(ctx, ana); b.Origin != OriginValues || b.Role != Owner {
		t.Errorf("taken over by the values: %+v", b)
	}
	if err := r.SyncOwners(ctx, []string{ana}); err != nil {
		t.Fatal(err)
	}
	if r.RoleOf(mira) != Viewer || r.RoleOf(ana) != Owner {
		t.Errorf("after the values dropped mira: %s %s", r.RoleOf(mira), r.RoleOf(ana))
	}

	// Names: noted for bound people only.
	if err := r.NoteName(ctx, ana, "Ana Ruiz @aruiz"); err != nil {
		t.Fatal(err)
	}
	if err := r.NoteName(ctx, mira, "Mira @mk"); err != nil {
		t.Fatal(err)
	}
	if r.Label(ana) != "Ana Ruiz @aruiz" || r.Label(mira) != mira {
		t.Errorf("labels: %q %q", r.Label(ana), r.Label(mira))
	}

	es, _ := r.AuditQuery(ctx, AuditQuery{Limit: 100})
	var got []string
	for _, e := range es {
		got = append(got, e.Actor+" "+e.Action+" "+e.Target)
	}
	want := []string{
		"values role.revoke " + mira,
		"values role.grant " + ana,
		mira + " role.grant " + ana,
		mira + " role.revoke " + jonas,
		mira + " role.grant " + jonas,
		mira + " role.grant " + jonas,
		"values role.grant " + mira,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("audit:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if es, _ := r.AuditQuery(ctx, AuditQuery{Actor: "values"}); len(es) != 3 {
		t.Errorf("by actor: %d", len(es))
	}
	if es, _ := r.AuditQuery(ctx, AuditQuery{Target: jonas, Limit: 1}); len(es) != 1 || es[0].Action != "role.revoke" {
		t.Errorf("by target, one: %+v", es)
	}
	first, _ := r.AuditQuery(ctx, AuditQuery{Limit: 3})
	next, _ := r.AuditQuery(ctx, AuditQuery{Limit: 100, Before: first[2].ID})
	if len(next) != len(want)-3 || next[0].ID >= first[2].ID {
		t.Errorf("the next page: %d", len(next))
	}
}

func TestTokens(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r, err := OpenRegistry(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	now := time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	tk, secret, err := r.IssueToken(ctx, "gitlab:g/1", "terraform", Owner, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, TokenPrefix) || len(secret) < 40 || tk.Hint != secret[len(secret)-4:] || !tk.ExpiresAt.Equal(now.Add(30*24*time.Hour)) {
		t.Fatalf("issued: %+v %q", tk, secret)
	}
	p, ok := r.TokenPrincipal(ctx, secret)
	if !ok || p.Subject != "token:terraform" || p.Role != Owner {
		t.Fatalf("principal: %+v %v", p, ok)
	}
	if ts, _ := r.Tokens(ctx); len(ts) != 1 || ts[0].UsedAt.IsZero() {
		t.Errorf("last use not written: %+v", ts)
	}
	for _, bad := range []string{"", secret + "x", strings.TrimPrefix(secret, TokenPrefix), "cia_pat_short"} {
		if _, ok := r.TokenPrincipal(ctx, bad); ok {
			t.Errorf("%q opens", bad)
		}
	}
	// The secret is nowhere in the file.
	r.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	for _, f := range []string{"fleet.db", "fleet.db-wal"} {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		if strings.Contains(string(b), secret[len(TokenPrefix):]) {
			t.Errorf("the secret is in %s", f)
		}
	}
	if _, _, err := r.IssueToken(ctx, "x", "terraform", Viewer, 0); !errors.Is(err, ErrExists) {
		t.Errorf("an active name again: %v", err)
	}
	for _, bad := range []string{"bootstrap", "Bad", "-x", ""} {
		if _, _, err := r.IssueToken(ctx, "x", bad, Viewer, 0); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q: %v", bad, err)
		}
	}

	// Expired: opens nothing; its name may be issued again.
	now = now.Add(30 * 24 * time.Hour)
	if _, ok := r.TokenPrincipal(ctx, secret); ok {
		t.Error("an expired token opens")
	}
	_, s2, err := r.IssueToken(ctx, "x", "terraform", Viewer, 0)
	if err != nil {
		t.Fatalf("the name of an expired token: %v", err)
	}
	if p, ok := r.TokenPrincipal(ctx, s2); !ok || p.Role != Viewer {
		t.Errorf("never-expiring viewer token: %+v %v", p, ok)
	}
	now = now.Add(10 * 365 * 24 * time.Hour)
	if _, ok := r.TokenPrincipal(ctx, s2); !ok {
		t.Error("a token that never expires expired")
	}
	if err := r.RevokeToken(ctx, "x", "terraform"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.TokenPrincipal(ctx, s2); ok {
		t.Error("a revoked token opens")
	}
	if err := r.RevokeToken(ctx, "x", "terraform"); err != nil {
		t.Errorf("revoking again: %v", err)
	}

	_, s3, _ := r.IssueToken(ctx, "x", "ci", Operator, 0)
	_, s4, _ := r.IssueToken(ctx, "x", "grafana", Viewer, time.Hour)
	n, err := r.VoidTokens(ctx, "local", "a backup was restored")
	if err != nil || n != 2 {
		t.Errorf("voided %d: %v", n, err)
	}
	for _, s := range []string{s3, s4} {
		if _, ok := r.TokenPrincipal(ctx, s); ok {
			t.Error("a voided token opens")
		}
	}
	// Ended tokens go with the removed members' keep.
	now = now.Add(r.KeepRemoved + time.Hour)
	if err := r.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if ts, _ := r.Tokens(ctx); len(ts) != 0 {
		t.Errorf("ended tokens kept: %+v", ts)
	}
}

// A command in the hub's pod writes the file; the hub sees it after
// Refresh.
func TestRefreshSeesAnotherProcess(t *testing.T) {
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
	if _, err := cmd.Grant(ctx, "local", OriginLocal, s, Owner); err != nil {
		t.Fatal(err)
	}
	cmd.Close()
	if hub.RoleOf(s) != Viewer {
		t.Fatal("seen before Refresh: the test proves nothing")
	}
	if err := hub.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if hub.RoleOf(s) != Owner {
		t.Error("not seen after Refresh")
	}
}

// A backup is a consistent copy; a restore comes back at the next start
// with every API token void (the copy holds tokens revoked since).
func TestBackupRestore(t *testing.T) {
	ctx := context.Background()
	dir, out := t.TempDir(), t.TempDir()
	r, err := OpenRegistry(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	_, pub, _ := GenerateKey()
	if _, err := r.Add(ctx, "token:t", OriginAPI, "eu", string(pub)); err != nil {
		t.Fatal(err)
	}
	_, secret, _ := r.IssueToken(ctx, "token:t", "terraform", Owner, 0)
	file := filepath.Join(out, "hub.db")
	if err := r.Backup(ctx, file); err != nil {
		t.Fatal(err)
	}
	if err := r.Backup(ctx, file); err == nil {
		t.Error("a backup over an existing file")
	}
	// After the backup: the token revoked, another member.
	r.RevokeToken(ctx, "token:t", "terraform")
	_, pub2, _ := GenerateKey()
	r.Add(ctx, "token:t", OriginAPI, "us", string(pub2))
	r.Close()

	junk := filepath.Join(out, "junk")
	os.WriteFile(junk, []byte("not a database"), 0o600)
	if err := StageRestore(ctx, dir, junk); err == nil {
		t.Error("junk staged")
	}
	if err := StageRestore(ctx, dir, file); err != nil {
		t.Fatal(err)
	}
	before, applied, err := ApplyRestore(dir, time.Date(2026, 11, 16, 1, 2, 3, 0, time.UTC))
	if err != nil || !applied || filepath.Base(before) != "fleet.db.before-restore-20261116-010203" {
		t.Fatalf("apply: %q %v %v", before, applied, err)
	}
	if _, err := os.Stat(before); err != nil {
		t.Errorf("the replaced file is not kept: %v", err)
	}
	r, err = OpenRegistry(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if ms, _ := r.List(ctx); len(ms) != 1 || ms[0].Name != "eu" {
		t.Errorf("members after the restore: %+v", ms)
	}
	// The staged copy's tokens were voided already; the hub's start voids
	// again and finds none.
	if n, err := r.VoidTokens(ctx, OriginLocal, "a backup was restored"); err != nil || n != 0 {
		t.Errorf("voided %d %v", n, err)
	}
	if _, ok := r.TokenPrincipal(ctx, secret); ok {
		t.Error("a token revoked after the backup opens again after the restore")
	}
	if _, applied, _ := ApplyRestore(dir, time.Now()); applied {
		t.Error("applied twice")
	}
}

func TestJoin(t *testing.T) {
	ctx := context.Background()
	r, now := openReg(t)
	_, pub, _ := GenerateKey()
	_, other, _ := GenerateKey()

	j, secret, err := r.IssueJoin(ctx, "gitlab:g/1", "ap", 0)
	if err != nil || !strings.HasPrefix(secret, JoinPrefix) || !j.ExpiresAt.Equal(now.Add(time.Hour)) || !j.Active(*now) {
		t.Fatalf("issue: %+v %v", j, err)
	}
	for _, bad := range []time.Duration{time.Second, 8 * 24 * time.Hour} {
		if _, _, err := r.IssueJoin(ctx, "x", "ap", bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("lifetime %s: %v", bad, err)
		}
	}
	if _, err := r.Join(ctx, "us", secret, string(pub)); !errors.Is(err, ErrDenied) {
		t.Errorf("for another name: %v", err)
	}
	if _, err := r.Join(ctx, "ap", secret+"x", string(pub)); !errors.Is(err, ErrDenied) {
		t.Errorf("a wrong secret: %v", err)
	}
	m, err := r.Join(ctx, "ap", secret, string(pub))
	if err != nil || m.Name != "ap" || m.Origin != OriginJoin || m.AddedBy != "member:ap" {
		t.Fatalf("join: %+v %v", m, err)
	}
	// Again with the same key (the box restarted): no error; with another
	// key: the token is used.
	if m, err := r.Join(ctx, "ap", secret, string(pub)); err != nil || m.Name != "ap" {
		t.Errorf("again, same key: %v", err)
	}
	if _, err := r.Join(ctx, "ap", secret, string(other)); !errors.Is(err, ErrDenied) {
		t.Errorf("used token, another key: %v", err)
	}
	if _, _, err := r.IssueJoin(ctx, "x", "ap", 0); !errors.Is(err, ErrExists) {
		t.Errorf("a token for a registered name: %v", err)
	}

	// A newer token replaces an unused one; an expired one is refused.
	_, s1, _ := r.IssueJoin(ctx, "x", "us", 0)
	_, s2, _ := r.IssueJoin(ctx, "x", "us", 2*time.Hour)
	if _, err := r.Join(ctx, "us", s1, string(other)); !errors.Is(err, ErrDenied) {
		t.Errorf("a replaced token: %v", err)
	}
	*now = now.Add(2*time.Hour + time.Second)
	if _, err := r.Join(ctx, "us", s2, string(other)); !errors.Is(err, ErrDenied) {
		t.Errorf("an expired token: %v", err)
	}
	// A name taken by a key of another member: the token stays unused.
	j3, s3, _ := r.IssueJoin(ctx, "x", "eu", 0)
	if _, err := r.Join(ctx, "eu", s3, string(pub)); !errors.Is(err, ErrExists) {
		t.Errorf("ap's key under eu: %v", err)
	}
	if js, _ := r.JoinTokens(ctx, "eu"); len(js) != 1 || !js[0].UsedAt.IsZero() {
		t.Errorf("a failed join used the token: %+v", js)
	}
	if err := r.RevokeJoin(ctx, "x", "eu", j3.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Join(ctx, "eu", s3, string(other)); !errors.Is(err, ErrDenied) {
		t.Errorf("a revoked token: %v", err)
	}
	_, s4, _ := r.IssueJoin(ctx, "x", "eu", 0)
	// Registered by key meanwhile (the API): the join fails and its token
	// is not spent.
	_, s5, _ := r.IssueJoin(ctx, "x", "ca", 0)
	_, pubCA, _ := GenerateKey()
	_, pubCA2, _ := GenerateKey()
	if _, err := r.Add(ctx, "x", OriginAPI, "ca", string(pubCA)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Join(ctx, "ca", s5, string(pubCA2)); !errors.Is(err, ErrExists) {
		t.Errorf("join of a name registered meanwhile: %v", err)
	}
	if js, _ := r.JoinTokens(ctx, "ca"); len(js) != 1 || !js[0].UsedAt.IsZero() {
		t.Errorf("a join that failed at registration spent its token: %+v", js)
	}
	if es, _ := r.AuditQuery(ctx, AuditQuery{Target: "us", Actor: "x"}); len(es) == 0 || es[len(es)-1].Action != "join-token.issue" ||
		es[len(es)-2].Action != "join-token.revoke" {
		t.Errorf("a replaced join token is not in the audit: %+v", es)
	}
	if _, err := r.VoidTokens(ctx, OriginLocal, "restore"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Join(ctx, "eu", s4, string(other)); !errors.Is(err, ErrDenied) {
		t.Errorf("a voided join token: %v", err)
	}
	es, _ := r.AuditQuery(ctx, AuditQuery{Target: "ap"})
	var acts []string
	for _, e := range es {
		acts = append(acts, e.Actor+" "+e.Action)
	}
	if strings.Join(acts, ",") != "member:ap member.join,member:ap member.add,gitlab:g/1 join-token.issue" {
		t.Errorf("audit of ap: %v", acts)
	}
}
