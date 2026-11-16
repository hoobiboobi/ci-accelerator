package web

import (
	"context"
	"strings"
	"testing"

	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// A COPY by glob without a COPY of the whole context: BuildKit keeps the
// glob as the follow path, and a file it matches is in the context.
func TestDecideFollowGlob(t *testing.T) {
	lc := func(follow ...string) *history.LocalContext { return &history.LocalContext{Follow: follow} }
	for _, c := range []struct {
		name    string
		e       store.Elsewhere
		changed string
	}{
		{"package*.json", store.Elsewhere{Folder: ".", Context: lc("package*.json"), Paths: []string{"package*.json"}}, "package.json"},
		{"src/*.go", store.Elsewhere{Folder: ".", Context: lc("go.mod", "src/*.go"), Paths: []string{"go.mod", "src/*.go"}}, "src/a.go"},
		{"folder svc, *.csproj", store.Elsewhere{Folder: "svc", Context: lc("*.csproj"), Paths: []string{"*.csproj"}}, "svc/h.csproj"},
		{"a glob folder", store.Elsewhere{Folder: ".", Context: lc("conf*"), Paths: []string{"conf*"}}, "config/a/b.yml"},
	} {
		c.e.ID, c.e.SHA, c.e.PrevSHA, c.e.Old = 1, "bbbbbbbbbbbb", "aaaaaaaaaaaa", "d1"
		s := decideByDiff(c.e, []string{c.changed}, true, nil)
		if s.Kind != history.FilesChanged || len(s.Paths) != 1 || s.Paths[0] != c.changed {
			t.Errorf("%s: %s changed -> %s %v %q, want files-changed", c.name, c.changed, s.Kind, s.Paths, s.Via)
		}
	}
	// A file no follow path takes in is still out of the context.
	e := store.Elsewhere{ID: 1, SHA: "b", PrevSHA: "a", Old: "d1", Folder: ".", Context: lc("package*.json"), Paths: []string{"package*.json"}}
	if s := decideByDiff(e, []string{"src/package.json"}, true, nil); s.Kind != history.OtherNode {
		t.Errorf("a file outside the follow glob: %s %v", s.Kind, s.Paths)
	}
}

// A change found in a comparison GitLab cut short is a change; that
// nothing else changed is not known, and the Via says so.
func TestDecideCutShortHit(t *testing.T) {
	e := store.Elsewhere{ID: 1, SHA: "bbbbbbbbbbbb", PrevSHA: "aaaaaaaaaaaa", Git: true, GitFolder: ".", Folder: ".", Paths: []string{"."}}
	full := decideByDiff(e, []string{"main.py"}, true, nil)
	cut := decideByDiff(e, []string{"main.py"}, false, nil)
	if full.Kind != history.FilesChanged || full.Via != "gitlab aaaaaaaa..bbbbbbbb" {
		t.Errorf("complete: %s %q", full.Kind, full.Via)
	}
	if cut.Kind != history.FilesChanged || cut.Via != "gitlab aaaaaaaa..bbbbbbbb cut short" || !viaVerified(cut.Via) {
		t.Errorf("cut short: %s %q", cut.Kind, cut.Via)
	}
}

// A rebuild of the same commit whose step also reads something fetched
// from the network stays unclear, as one of another commit does.
func TestSettleSameCommitRemote(t *testing.T) {
	p := store.ProjectKey{Instance: "abcdef", ID: 42}
	for _, src := range []string{"https://example.com/latest.tgz", "git://example.com/x.git#main"} {
		f := &fakeSettle{open: []store.Elsewhere{{ID: 1, SHA: "cccccccc", PrevSHA: "cccccccc", Old: "d1", Paths: []string{"src", src}}}}
		if _, err := SettleProject(ctx, f, nil, p, testNow, 0, 25); err != nil {
			t.Fatal(err)
		}
		if len(f.settled) != 1 || f.settled[0].Kind != history.CacheElsewhere || !strings.HasPrefix(f.settled[0].Via, "same commit") {
			t.Errorf("%s: %+v", src, f.settled)
		}
	}
	f := &fakeSettle{open: []store.Elsewhere{{ID: 1, SHA: "cccccccc", PrevSHA: "cccccccc", Old: "d1", Paths: []string{"src"}}}}
	SettleProject(ctx, f, nil, p, testNow, 0, 25)
	if len(f.settled) != 1 || f.settled[0].Kind != history.OtherNode {
		t.Errorf("same commit, repository files only: %+v", f.settled)
	}
}

// An edited .dockerignore of the context may let other files into what a
// COPY reads: no "nothing it copies changed".
func TestDecideDockerignoreEdit(t *testing.T) {
	lc := &history.LocalContext{Follow: []string{"src"}, Excludes: []string{"src/gen"}}
	for _, c := range []struct {
		folder, changed string
		unclear         bool
	}{
		{".", ".dockerignore", true},
		{".", "Dockerfile.dockerignore", true},
		{"svc", "svc/.dockerignore", true},
		{"svc", "other/.dockerignore", false},
		{".", "docs/readme.md", false},
	} {
		e := store.Elsewhere{ID: 1, SHA: "bbbbbbbbbbbb", PrevSHA: "aaaaaaaaaaaa", Old: "d1", Folder: c.folder, Context: lc, Paths: []string{"src"}}
		s := decideByDiff(e, []string{c.changed}, true, nil)
		if got := s.Kind == history.CacheElsewhere; got != c.unclear {
			t.Errorf("folder %s, %s changed: %s %q", c.folder, c.changed, s.Kind, s.Via)
		}
	}
}

// byTo answers GitLab's comparison by the later commit: an error or files.
type byTo struct {
	errs  map[string]error
	asked []string
}

func (b *byTo) Changed(_ context.Context, _ string, _ uint64, _, to string) ([]string, bool, error) {
	b.asked = append(b.asked, to)
	if err := b.errs[to]; err != nil {
		return nil, false, err
	}
	return []string{"src/x"}, true, nil
}

// One comparison that keeps failing holds back no other: the pass goes on
// with the project's other comparisons, and a second failure ends it.
func TestSettleSkipsAFailingComparison(t *testing.T) {
	p := store.ProjectKey{Instance: "abcdef", ID: 42}
	build := func(id int64, to string) store.Elsewhere {
		return store.Elsewhere{ID: id, SHA: to, PrevSHA: "prev0000", Paths: []string{"src"}, Git: true, GitFolder: ".", Folder: ".", Old: "d1"}
	}
	for _, c := range []struct {
		name string
		api  func(*byTo) Changer
		code int
	}{
		{"503 with the installation's token", func(b *byTo) Changer { return b }, 503},
		{"403 with the installation's token", func(b *byTo) Changer { return b }, 403},
		{"a reader's 404", func(b *byTo) Changer { return personal{b} }, 404},
	} {
		b := &byTo{errs: map[string]error{"bad00000": &gitlab.StatusError{Code: c.code, Status: "failed"}}}
		f := &fakeSettle{open: []store.Elsewhere{build(1, "bad00000"), build(2, "ok000000"), build(3, "ok111111")}}
		n, err := SettleProject(ctx, f, c.api(b), p, testNow, 0, 25)
		if n != 2 || err == nil {
			t.Errorf("%s: settled %d, err %v; want 2 and the error", c.name, n, err)
		}
		for _, s := range f.settled {
			if s.ID == 1 {
				t.Errorf("%s: the failing comparison was stored: %+v", c.name, s)
			}
		}
		// Two failing comparisons: GitLab (or the reader's access) is the
		// cause, the pass ends.
		b = &byTo{errs: map[string]error{"bad00000": &gitlab.StatusError{Code: c.code, Status: "failed"}, "bad11111": &gitlab.StatusError{Code: c.code, Status: "failed"}}}
		f = &fakeSettle{open: []store.Elsewhere{build(1, "bad00000"), build(2, "bad11111"), build(3, "ok000000")}}
		SettleProject(ctx, f, c.api(b), p, testNow, 0, 25)
		if len(b.asked) != 2 || len(f.settled) != 0 {
			t.Errorf("%s, two failing: asked %v, settled %+v", c.name, b.asked, f.settled)
		}
	}
}
