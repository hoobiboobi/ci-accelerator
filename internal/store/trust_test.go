package store

import (
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// With TrustGitComparison off (the default), "GitLab shows no change in
// the copied files" settles nothing: the build stays unclear, keeping what
// the comparison showed. Switching it on or off later turns the stored
// verdicts over without asking GitLab again.
func TestGitComparisonTrust(t *testing.T) {
	dir := t.TempDir()
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	openWith := func(trust bool) *Store {
		t.Helper()
		s, err := Open(ctx, dir, Options{Now: c.now, TrustGitComparison: trust})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := openWith(false)
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	var id int64
	for _, b := range bs {
		prev, _, err := s.Previous(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		var p *history.Build
		if prev != nil {
			p = prev.Build
		}
		rr, _ := history.FirstRebuild(p, b)
		got, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1, Reason: rr})
		if err != nil {
			t.Fatal(err)
		}
		if rr.Kind == history.CacheElsewhere && id == 0 {
			id = got
		}
	}
	if id == 0 {
		t.Fatal("no fixture rebuilt with its cache elsewhere")
	}
	project := ProjectKey{Instance: bs[0].Attribution.Instance}
	if err := s.db.QueryRowContext(ctx, "SELECT project_id FROM builds WHERE id = ?", id).Scan(&project.ID); err != nil {
		t.Fatal(err)
	}
	settle := func(s *Store) {
		t.Helper()
		open, err := s.Elsewhere(ctx, project, c.t.Add(-31*24*time.Hour), 100)
		if err != nil {
			t.Fatal(err)
		}
		var out []Settled
		for _, e := range open {
			if e.ID == id {
				out = append(out, Settled{ID: id, Kind: history.NotInCache, Via: "same commit 0123abcd", Asked: true, Old: e.Old, Open: e.Paths})
			}
		}
		if len(out) != 1 {
			t.Fatalf("build %d not open for settling (%d open)", id, len(open))
		}
		if err := s.SettleElsewhere(ctx, out); err != nil {
			t.Fatal(err)
		}
	}
	verdict := func(s *Store) (history.ReasonKind, string) {
		t.Helper()
		st, err := s.Load(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return st.Reason.Kind, st.Via
	}
	settle(s)
	if k, via := verdict(s); k != history.CacheElsewhere || via != "same commit 0123abcd" {
		t.Fatalf("trust off: %s via %q, want cache-elsewhere via the comparison", k, via)
	}
	s.Close()

	s = openWith(true) // switched on: back to settling, then the farm's
	if k, via := verdict(s); k != history.CacheElsewhere || via != "" {
		t.Fatalf("after switching on: %s via %q, want open again", k, via)
	}
	settle(s)
	if k, _ := verdict(s); k != history.NotInCache {
		t.Fatalf("trust on: %s, want not-in-cache", k)
	}
	s.Close()

	s = openWith(false) // switched off again: unclear, the comparison kept
	defer s.Close()
	if k, via := verdict(s); k != history.CacheElsewhere || via != "same commit 0123abcd" {
		t.Fatalf("after switching off: %s via %q", k, via)
	}
}

// A store written by a release before the option existed trusted the
// comparison: opening it with the default turns those verdicts unclear.
func TestGitComparisonTrustUpgrade(t *testing.T) {
	dir := t.TempDir()
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s, err := Open(ctx, dir, Options{Now: c.now, TrustGitComparison: true})
	if err != nil {
		t.Fatal(err)
	}
	b := *fixtures(t, s.SecretKey())[0]
	id, err := s.SaveBuild(ctx, Build{Build: &b, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	// As 0.3.10 left it: a farm verdict from the comparison, no meta row.
	if _, err := s.db.ExecContext(ctx, "UPDATE builds SET reason_kind = 'other-node', reason_via = 'gitlab aaaa..bbbb', reason_old = 'bk-a' WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM meta WHERE name = 'trust_git_comparison'"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = open(t, dir, c)
	st, err := s.Load(ctx, id)
	if err != nil || st.Reason.Kind != history.CacheElsewhere || st.Via != "gitlab aaaa..bbbb" {
		t.Fatalf("after the upgrade: %+v via %q, %v", st.Reason.Kind, st.Via, err)
	}
}

// A build whose context was fetched from git is the repository at its
// commit: the comparison of the commits holds for it with the option off,
// and switching the option over leaves its verdict alone. Elsewhere
// offers its own context commit for the comparison.
func TestGitContextTrustsTheComparison(t *testing.T) {
	const (
		commit = "8c7c95c5d5dd6adf4182d398fcb87b970fdaf8dd"
		before = "2fa0dff0d54226d96776eb27cd5fd2ccc27d2e44"
	)
	dir := t.TempDir()
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	openWith := func(trust bool) *Store {
		t.Helper()
		s, err := Open(ctx, dir, Options{Now: c.now, TrustGitComparison: trust})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := openWith(false)
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	var id int64
	for _, b := range bs {
		prev, _, err := s.Previous(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		var p *history.Build
		if prev != nil {
			p = prev.Build
		}
		rr, _ := history.FirstRebuild(p, b)
		b.ContextCommit = before
		if rr.Kind == history.CacheElsewhere && id == 0 {
			b.ContextCommit, b.Context = commit, "services/api"
		}
		got, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1, Reason: rr})
		if err != nil {
			t.Fatal(err)
		}
		if rr.Kind == history.CacheElsewhere && id == 0 {
			id = got
		}
	}
	if id == 0 {
		t.Fatal("no fixture rebuilt with its cache elsewhere")
	}
	project := ProjectKey{Instance: bs[0].Attribution.Instance}
	if err := s.db.QueryRowContext(ctx, "SELECT project_id FROM builds WHERE id = ?", id).Scan(&project.ID); err != nil {
		t.Fatal(err)
	}
	open, err := s.Elsewhere(ctx, project, c.t.Add(-31*24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []Settled
	for _, e := range open {
		if e.ID == id {
			if e.SHA != commit || e.PrevSHA != before || e.GitFolder != "services/api" || e.Folder != "services/api" || !e.Git {
				t.Errorf("Elsewhere SHA %q, previous %q, folder %q; want the context commits and folder", e.SHA, e.PrevSHA, e.GitFolder)
			}
			out = append(out, Settled{ID: id, Kind: history.OtherNode, Via: "gitlab aaaa..bbbb", Asked: true, Old: e.Old, Open: e.Paths})
		}
	}
	if len(out) != 1 {
		t.Fatalf("build %d not open for settling", id)
	}
	if err := s.SettleElsewhere(ctx, out); err != nil {
		t.Fatal(err)
	}
	verdict := func(s *Store) history.ReasonKind {
		t.Helper()
		st, err := s.Load(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if st.Build.ContextCommit != commit {
			t.Errorf("context commit %q after a round trip", st.Build.ContextCommit)
		}
		return st.Reason.Kind
	}
	if k := verdict(s); k != history.OtherNode {
		t.Fatalf("trust off, git context: %s, want other-node", k)
	}
	s.Close()
	s = openWith(true)
	if k := verdict(s); k != history.OtherNode {
		t.Fatalf("switched on: %s", k)
	}
	s.Close()
	s = openWith(false)
	defer s.Close()
	if k := verdict(s); k != history.OtherNode {
		t.Fatalf("switched off again: %s, want other-node", k)
	}
}

// A comparison stored by an earlier release has its submodules unmarked:
// Compared still reads it, ComparedMarked does not, until it is saved again.
func TestComparedMarked(t *testing.T) {
	dir := t.TempDir()
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, dir, c)
	defer s.Close()
	p := ProjectKey{Instance: "abcdef", ID: 7}
	if err := s.SaveCompare(ctx, p, "a", "b", []string{"third_party/proto/"}, true); err != nil {
		t.Fatal(err)
	}
	if f, _, ok, err := s.ComparedMarked(ctx, p, "a", "b"); err != nil || !ok || len(f) != 1 || f[0] != "third_party/proto/" {
		t.Fatalf("marked: %v %v %v", f, ok, err)
	}
	// As 0.3.11 left it.
	if _, err := s.db.ExecContext(ctx, "UPDATE compares SET marks = 0, files = '[\"third_party/proto\"]'"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.ComparedMarked(ctx, p, "a", "b"); err != nil || ok {
		t.Errorf("an unmarked comparison read as marked: %v %v", ok, err)
	}
	if f, _, ok, err := s.Compared(ctx, p, "a", "b"); err != nil || !ok || f[0] != "third_party/proto" {
		t.Errorf("Compared: %v %v %v", f, ok, err)
	}
	if _, _, ok, _ := s.ComparedMarked(ctx, p, "x", "y"); ok {
		t.Error("a comparison never stored")
	}
	if err := s.SaveCompare(ctx, p, "a", "b", []string{"third_party/proto/"}, true); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.ComparedMarked(ctx, p, "a", "b"); !ok {
		t.Error("saved again, still unmarked")
	}
}

// A reason's changed files as an older release may have stored them, a
// submodule with its comparison's trailing slash, read as plain paths.
func TestReasonPathsReadPlain(t *testing.T) {
	dir := t.TempDir()
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, dir, c)
	defer s.Close()
	b := *fixtures(t, s.SecretKey())[0]
	id, err := s.SaveBuild(ctx, Build{Build: &b, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE builds SET reason_kind = 'files-changed', reason_paths = '["third_party/proto/","a.txt"]' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	st, err := s.Load(ctx, id)
	if err != nil || strings.Join(st.Reason.Paths, ",") != "third_party/proto,a.txt" {
		t.Errorf("paths %v, %v", st.Reason.Paths, err)
	}
}
