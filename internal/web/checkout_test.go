package web

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// The .dockerignore patterns and followed paths of real projects' builds
// (their records' provenance, lab run of 2026-11-10).
var (
	meiliCtx = &history.LocalContext{Excludes: []string{"target", "Dockerfile", ".dockerignore", ".gitignore"}}
	hugoCtx  = &history.LocalContext{Excludes: []string{"*.md", "*.log", "*.txt", ".git", ".github", ".circleci", "docs", "examples", "Dockerfile"}}
	// An allow-list: everything out, then some back in.
	excalidrawCtx = &history.LocalContext{Excludes: []string{"*", "!.env.development", "!.env.production", "!.eslintrc.json", "!.npmrc",
		"!.prettierrc", "!excalidraw-app", "!package.json", "!public", "!packages", "!scripts", "!tsconfig.json", "!yarn.lock",
		"**/build", "**/dist", "**/node_modules"}}
	// The frontend named the paths it needs: nothing else was sent.
	paperlessCtx = &history.LocalContext{
		Excludes: []string{"**/__pycache__", ".venv", "src-ui/node_modules", "src-ui/dist", ".git", "**/tests", "docs"},
		Follow:   []string{"docker/rootfs", "pyproject.toml", "src", "src-ui", "uv.lock"},
	}
)

func TestInContext(t *testing.T) {
	in := func(c *history.LocalContext, folder string) store.Elsewhere {
		return store.Elsewhere{Context: c, Folder: folder}
	}
	for _, c := range []struct {
		name string
		e    store.Elsewhere
		file string
		want bool
	}{
		{"meilisearch: its .git is sent", in(meiliCtx, "."), ".git", true},
		{"meilisearch: the Dockerfile is not", in(meiliCtx, "."), "Dockerfile", false},
		{"meilisearch: a source file is", in(meiliCtx, "."), "crates/meilisearch/src/main.rs", true},
		{"meilisearch: below an excluded folder", in(meiliCtx, "."), "target/release/x", false},
		{"hugo: .git excluded", in(hugoCtx, "."), ".git", false},
		{"hugo: a glob", in(hugoCtx, "."), "README.md", false},
		{"hugo: source", in(hugoCtx, "."), "main.go", true},
		{"allow-list: an allowed folder's file", in(excalidrawCtx, "."), "excalidraw-app/App.tsx", true},
		{"allow-list: .git", in(excalidrawCtx, "."), ".git", false},
		{"allow-list: a file not allowed", in(excalidrawCtx, "."), "README.md", false},
		{"allow-list: excluded again below an allowed folder", in(excalidrawCtx, "."), "packages/x/node_modules/y.js", false},
		{"followed: inside a followed path", in(paperlessCtx, "."), "src/paperless/version.py", true},
		{"followed: not followed", in(paperlessCtx, "."), "README.md", false},
		{"followed: followed but excluded", in(paperlessCtx, "."), "src-ui/node_modules/a.js", false},
		{"followed: .git", in(paperlessCtx, "."), ".git", false},
		{"a context folder: inside", in(meiliCtx, "svc"), "svc/main.rs", true},
		{"a context folder: outside", in(meiliCtx, "svc"), "other/main.rs", false},
		{"a context folder: excluded inside", in(meiliCtx, "svc"), "svc/Dockerfile", false},
		{"folder not known", in(meiliCtx, ""), "Dockerfile", true},
		{"context not known", in(nil, "."), "Dockerfile", true},
	} {
		if got := inContext(c.e, c.file); got != c.want {
			t.Errorf("%s: inContext(%q) = %v, want %v", c.name, c.file, got, c.want)
		}
	}
}

// A rebuild whose commit did not change what its step reads is the job's
// checkout when the step reads the whole context and .git is in it; files
// .dockerignore keeps out never count as the step's.
func TestDecideByDiffCheckout(t *testing.T) {
	e := func(c *history.LocalContext, folder string, paths ...string) store.Elsewhere {
		return store.Elsewhere{ID: 1, SHA: "b", PrevSHA: "a", Paths: paths, Folder: folder, Context: c}
	}
	for _, c := range []struct {
		name    string
		e       store.Elsewhere
		changed []string
		kind    history.ReasonKind
		paths   []string
	}{
		// meilisearch's ENV in the last stage: the Dockerfile is excluded, the
		// first rebuilt step is COPY . . — its .git changed, not the Dockerfile.
		{"excluded Dockerfile, .git sent", e(meiliCtx, ".", "."), []string{"Dockerfile"}, history.FilesChanged, []string{".git"}},
		{"a source file sent", e(meiliCtx, ".", "."), []string{"crates/meilisearch/src/main.rs"}, history.FilesChanged, []string{"crates/meilisearch/src/main.rs"}},
		{"excluded Dockerfile, .git excluded too", e(hugoCtx, ".", "."), []string{"Dockerfile"}, history.NotInCache, nil},
		{"the step reads a .git bind mount", e(meiliCtx, ".", ".git/"), []string{"Dockerfile"}, history.FilesChanged, []string{".git"}},
		{"the step reads only go.mod", e(meiliCtx, ".", "go.mod"), []string{"docs/x.md"}, history.NotInCache, nil},
		{"a context folder has no .git", e(meiliCtx, "svc", "."), []string{"docs/x.md"}, history.NotInCache, nil},
		// An older release stored no context: the verdict stays as before.
		{"context not known", e(nil, ".", "."), []string{"Dockerfile"}, history.FilesChanged, []string{"Dockerfile"}},
	} {
		got := decideByDiff(c.e, c.changed, true, nil)
		if got.Kind != c.kind || !slices.Equal(got.Paths, c.paths) {
			t.Errorf("%s: %s %v, want %s %v", c.name, got.Kind, got.Paths, c.kind, c.paths)
		}
		if c.kind == history.FilesChanged && slices.Equal(c.paths, []string{".git"}) && got.Via != viaCheckout {
			t.Errorf("%s: via %q", c.name, got.Via)
		}
	}
	// A git context is the commit's files: no .git, whatever the patterns.
	git := e(meiliCtx, ".", ".")
	git.Git = true
	if checkoutGit(git) {
		t.Error("a git context has no checkout")
	}
}

// Two builds of the same commit: the second rebuilt COPY . . because the
// job's .git differs (meilisearch's warm build), unless .git is excluded.
func TestSameCommitCheckout(t *testing.T) {
	sent := store.Elsewhere{ID: 1, SHA: "a", PrevSHA: "a", Paths: []string{"."}, Folder: ".", Context: meiliCtx}
	kept := store.Elsewhere{ID: 2, SHA: "a", PrevSHA: "a", Paths: []string{"."}, Folder: ".", Context: hugoCtx}
	f := &fakeSettle{open: []store.Elsewhere{sent, kept}}
	if _, err := SettleProject(ctx, f, nil, store.ProjectKey{Instance: "abcdef", ID: 1}, testNow, 0, 25); err != nil {
		t.Fatal(err)
	}
	got := map[int64]store.Settled{}
	for _, s := range f.settled {
		got[s.ID] = s
	}
	if s := got[1]; s.Kind != history.FilesChanged || !slices.Equal(s.Paths, []string{".git"}) || s.Via != viaCheckout {
		t.Errorf(".git sent: %+v", s)
	}
	if s := got[2]; s.Kind != history.NotInCache {
		t.Errorf(".git excluded: %+v", s)
	}
}

// The context's patterns go from a record's provenance through the store
// to settling: the fixtures' builds sent their folder with .git excluded.
func TestElsewhereCarriesTheContext(t *testing.T) {
	_, st, _ := console(t)
	ps, err := st.ElsewhereProjects(ctx, time.Time{})
	if err != nil || len(ps) == 0 {
		t.Fatalf("fixtures: no open rebuild (%v, %v)", ps, err)
	}
	seen := 0
	for _, p := range ps {
		es, err := st.Elsewhere(ctx, p, time.Time{}, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if e.Context != nil {
				seen++
				if !slices.Contains(e.Context.Excludes, ".git") {
					t.Errorf("build %d: excludes %v, want .git among them", e.ID, e.Context.Excludes)
				}
			}
		}
	}
	if seen == 0 {
		t.Error("no open rebuild carries its context")
	}
}

// The pages say what to do about a .git rebuild: the build's verdict and
// the lists' texts.
func TestCheckoutTexts(t *testing.T) {
	r := history.Reason{Kind: history.FilesChanged, Step: history.Step{Name: "[compiler 3/5] COPY . .", Line: 15}, Paths: []string{".git"}}
	v := withVia(Verdict{}, r, viaCheckout)
	if !strings.Contains(string(v.Title), ".git</code> changed") || !strings.Contains(string(v.Title), "line 15") ||
		!strings.Contains(string(v.Hint), ".dockerignore") || !strings.Contains(string(v.Hint), "context-source: git") {
		t.Errorf("verdict %+v", v)
	}
	row := store.BuildRow{Reason: history.FilesChanged, Via: viaCheckout, Paths: []string{".git"}, Built: 3, Completed: time.Now()}
	if w := whyShort(row); !strings.Contains(w, ".git") {
		t.Errorf("whyShort %q", w)
	}
	if r := reasonText(row, rowStatus(row)); !strings.Contains(r, ".dockerignore") {
		t.Errorf("reasonText %q", r)
	}
}
