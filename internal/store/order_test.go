package store

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// The records in testdata/order are real builds of the 101-project lab run
// (2026-11-10, BuildKit v0.34.2): TradingAgents built cold, then after a
// one-line edit of main.py (its Dockerfile runs "pip install ." right
// after "COPY . ."); browser-use built cold, then after an edit of
// docker/smoke.py (its Dockerfile installs the dependencies with "uv sync
// --no-install-project" before "COPY . /app": the right order). The lab's
// GitLab address in them is replaced by the projects' GitHub ones, and two
// words the repository's commit hook refuses (a package name in pip's
// output, a .dockerignore entry) by others of the same length.
func loadOrder(t *testing.T, name string) *history.Build {
	t.Helper()
	d := filepath.Join("testdata", "order", name)
	read := func(f string) []byte {
		b, err := os.ReadFile(filepath.Join(d, f))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	rec, err := history.ParseRecord(read("record.json"))
	if err != nil {
		t.Fatal(err)
	}
	progress, err := history.DecodeProgress(bytes.NewReader(read("logs.bin")))
	if err != nil {
		t.Fatal(err)
	}
	b, err := history.NewBuild(rec, progress, [][]byte{read("provenance.json")}, nil, nil, history.Config{SecretKey: []byte("test")})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rebuilt is the second build's reason as GitLab's comparison settles it:
// the first rebuilt step and the changed files it copies.
func rebuilt(t *testing.T, prev, cur *history.Build, files ...string) history.Reason {
	t.Helper()
	r, ok := history.FirstRebuild(prev, cur)
	if !ok || r.Kind != history.CacheElsewhere {
		t.Fatalf("reason %+v, want cache-elsewhere before settling", r)
	}
	r.Kind, r.Paths = history.FilesChanged, files
	return r
}

const tradingagentsFragment = `COPY pyproject.toml setup.py* setup.cfg* /build/
RUN python -c "import pathlib, tomllib; f = pathlib.Path('pyproject.toml'); p = tomllib.loads(f.read_text()).get('project', {}) if f.exists() else {}; print('\n'.join(p.get('dependencies', [])))" > /tmp/requirements.txt \
 && pip install --no-cache-dir -r /tmp/requirements.txt
COPY . .
RUN pip install --no-cache-dir .`

func TestOrderHints(t *testing.T) {
	ta1, ta2 := loadOrder(t, "tradingagents-1"), loadOrder(t, "tradingagents-2")
	_, df := ta2.Dockerfile()
	r := rebuilt(t, ta1, ta2, "main.py")
	if r.Step.Key != "[builder 4/5]" || r.Step.Name != "COPY . ." {
		t.Fatalf("first rebuilt step %s %q", r.Step.Key, r.Step.Name)
	}
	hs := orderHints(ta2, r, "gitlab d97865d7..0612237d", df)
	if len(hs) != 1 {
		t.Fatalf("%d hints, want 1: %+v", len(hs), hs)
	}
	h := hs[0]
	if h.Key != "[builder 5/5]" || strings.Join(h.Managers, ",") != "pip" || h.CopyKey != "[builder 4/5]" {
		t.Errorf("hint %s %v after %s", h.Key, h.Managers, h.CopyKey)
	}
	// Measured in the lab: 30.4 s of the build's 36.8 s.
	if h.Took < 30*time.Second || h.Took > 31*time.Second {
		t.Errorf("took %s, want the step's 30.4 s", h.Took)
	}
	if strings.Join(h.Files, " ") != "pyproject.toml setup.py setup.cfg" {
		t.Errorf("files %v", h.Files)
	}
	if h.Fragment != tradingagentsFragment {
		t.Errorf("fragment:\n%s\nwant:\n%s", h.Fragment, tradingagentsFragment)
	}
	if len(h.Lines) == 0 || h.Lines[0].Start != 11 || len(h.CopyLines) == 0 || h.CopyLines[0].Start != 10 {
		t.Errorf("lines %v, copy lines %v: want 11 and 10", h.Lines, h.CopyLines)
	}

	// A step that is not right after the copy gets the hint without a
	// fragment: what lies between is not known to be safe to move.
	apart := *ta2
	apart.Steps = append([]history.Step(nil), ta2.Steps...)
	for i := range apart.Steps {
		if apart.Steps[i].Key == "[builder 5/5]" {
			apart.Steps[i].Key = "[builder 6/6]"
		}
	}
	if hs := orderHints(&apart, r, "gitlab a..b", df); len(hs) != 1 || hs[0].Fragment != "" {
		t.Errorf("a step apart from the copy: %+v", hs)
	}

	// What must not give the hint.
	none := map[string]struct {
		r   history.Reason
		via string
	}{
		"a dependency file changed":     {withPaths(r, "main.py", "pyproject.toml"), "gitlab a..b"},
		"in a sub-folder too":           {withPaths(r, "libs/x/setup.cfg"), "gitlab a..b"},
		"the changed files cut short":   {withPaths(r, "main.py", "21 more"), "gitlab a..b"},
		"GitLab's comparison cut short": {r, "gitlab aaaaaaaa..bbbbbbbb cut short"},
		"not settled by GitLab":         {r, ""},
		"settled by the same commit":    {r, "same commit 0612237d"},
		"an unclear rebuild":            {withKind(r, history.CacheElsewhere), "gitlab a..b"},
		"no changed files known":        {withPaths(r), "gitlab a..b"},
		"a rebuild from another step":   {withStep(r, ta2.Steps[0]), "gitlab a..b"},
	}
	for name, c := range none {
		if hs := orderHints(ta2, c.r, c.via, df); len(hs) != 0 {
			t.Errorf("%s: %d hints", name, len(hs))
		}
	}

	// The right order (uv sync --no-install-project before the copy): the
	// uv sync after it installs the project only.
	bu1, bu2 := loadOrder(t, "browser-use-1"), loadOrder(t, "browser-use-2")
	_, bdf := bu2.Dockerfile()
	br := rebuilt(t, bu1, bu2, "docker/smoke.py")
	if !strings.HasPrefix(br.Step.Name, "COPY . /app") {
		t.Fatalf("browser-use rebuilt from %q", br.Step.Name)
	}
	if hs := orderHints(bu2, br, "gitlab a..b", bdf); len(hs) != 0 {
		t.Errorf("browser-use: %d hints: %+v", len(hs), hs)
	}
}

func withPaths(r history.Reason, ps ...string) history.Reason { r.Paths = ps; return r }
func withKind(r history.Reason, k history.ReasonKind) history.Reason {
	r.Kind = k
	return r
}
func withStep(r history.Reason, s history.Step) history.Reason { r.Step = s; return r }

// Settling a rebuild keeps its order time; the build page, the image's
// figures and the Insights rows read it; a later rebuild whose
// dependencies came from the cache with another manager fixed nothing.
func TestOrderStored(t *testing.T) {
	c := &clock{time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	ta1, ta2 := loadOrder(t, "tradingagents-1"), loadOrder(t, "tradingagents-2")
	bu1, bu2 := loadOrder(t, "browser-use-1"), loadOrder(t, "browser-use-2")
	r2, _ := history.FirstRebuild(ta1, ta2)
	rb, _ := history.FirstRebuild(bu1, bu2)
	// ta3: the same rebuild once more, a minute later (a second change of
	// a code file); bu3: browser-use's rebuild once more, its uv install
	// not from the cache (its dependencies changed): nothing was fixed.
	ta3 := *ta2
	ta3.Ref, ta3.Started, ta3.Completed = "ta3", ta2.Started.Add(time.Minute), ta2.Completed.Add(time.Minute)
	bu3 := *bu2
	bu3.Ref, bu3.Started, bu3.Completed = "bu3", bu2.Started.Add(2*time.Minute), bu2.Completed.Add(2*time.Minute)
	bu3.Steps = append([]history.Step(nil), bu2.Steps...)
	for i, st := range bu3.Steps {
		if st.State == history.Cached && strings.Contains(st.Name, "uv sync") {
			bu3.Steps[i].State = history.Built
		}
	}
	ids := map[string]int64{}
	for name, x := range map[string]Build{"ta1": {Build: ta1, Generation: 1}, "ta2": {Build: ta2, Generation: 1, Reason: r2},
		"bu1": {Build: bu1, Generation: 1}, "bu2": {Build: bu2, Generation: 1, Reason: rb},
		"ta3": {Build: &ta3, Generation: 1, Reason: r2}, "bu3": {Build: &bu3, Generation: 1, Reason: rb}} {
		id, err := s.SaveBuild(ctx, x)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	// One image of one project: the browser-use builds stand for the same
	// image after its Dockerfile installs in the right order (they started
	// after the TradingAgents ones).
	if _, err := s.db.ExecContext(ctx, "UPDATE builds SET instance = 1, project_id = 5, target = ''"); err != nil {
		t.Fatal(err)
	}
	err := s.SettleElsewhere(ctx, []Settled{
		{ID: ids["ta2"], Kind: history.FilesChanged, Paths: []string{"main.py"}, Via: "gitlab d97865d7..0612237d"},
		{ID: ids["bu2"], Kind: history.FilesChanged, Paths: []string{"docker/smoke.py"}, Via: "gitlab a99dc9f0..b0b0b0b0"},
		{ID: ids["ta3"], Kind: history.FilesChanged, Paths: []string{"cli/main.py"}, Via: "gitlab 0612237d..1a1a1a1a"},
		{ID: ids["bu3"], Kind: history.FilesChanged, Paths: []string{"docker/smoke.py"}, Via: "gitlab b0b0b0b0..c1c1c1c1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ms := map[string]int64{}
	for name, id := range ids {
		var v int64
		if err := s.db.QueryRowContext(ctx, "SELECT order_ms FROM builds WHERE id = ?", id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		ms[name] = v
	}
	if ms["ta2"] < 30000 || ms["ta2"] > 31000 || ms["ta3"] != ms["ta2"] || ms["ta1"] != 0 || ms["bu1"] != 0 || ms["bu2"] != 0 || ms["bu3"] != 0 {
		t.Errorf("order_ms %v: want about 30400 for the TradingAgents rebuild only", ms)
	}
	p, err := s.BuildPage(ctx, ids["ta2"])
	if err != nil {
		t.Fatal(err)
	}
	// The window is the last 30 days, as for the cache-mount hints: the
	// later rebuild counts on the earlier one's page too.
	if len(p.Order) != 1 || p.Order[0].Runs != 2 || p.Order[0].RunTime != 2*p.Order[0].Took || p.OrderFixed != nil {
		t.Errorf("page: %+v, fixed %+v", p.Order, p.OrderFixed)
	}
	// The second rebuild counts both: twice in the image's builds.
	p3, err := s.BuildPage(ctx, ids["ta3"])
	if err != nil {
		t.Fatal(err)
	}
	if len(p3.Order) != 1 || p3.Order[0].Runs != 2 || p3.Order[0].RunTime != 2*p3.Order[0].Took {
		t.Errorf("second rebuild: %+v", p3.Order)
	}
	if b3, err := s.BuildPage(ctx, ids["bu3"]); err != nil || b3.OrderFixed != nil {
		t.Errorf("uv installed again, yet fixed: %+v %v", b3.OrderFixed, err)
	}
	q, err := s.BuildPage(ctx, ids["bu2"])
	if err != nil {
		t.Fatal(err)
	}
	// Its uv install came from the cache, but the earlier builds installed
	// with pip again: nothing of theirs was fixed (TestOrderFixedNeedsTheSameInstall
	// has a fixed one).
	if len(q.Order) != 0 || q.OrderFixed != nil {
		t.Fatalf("later build: order %+v, fixed %+v", q.Order, q.OrderFixed)
	}
	rows, err := s.Builds(ctx, BuildQuery{Project: ProjectKey{Instance: "000001", ID: 5}})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, b := range rows {
		if b.Order > 0 {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d rows with an order time, want 2", n)
	}
	api, err := s.BuildOrder(ctx, []int64{ids["ta1"], ids["ta2"], ids["bu2"], ids["ta3"]})
	if err != nil || len(api) != 2 || len(api[ids["ta2"]]) != 1 {
		t.Errorf("BuildOrder %v %v", api, err)
	}
}

func TestFrontendHasParents(t *testing.T) {
	for df, want := range map[string]bool{
		"FROM x\n":                                               true,
		"# syntax=docker/dockerfile:1\nFROM x\n":                 true,
		"# syntax=docker/dockerfile:1.20\nFROM x\n":              true,
		"# syntax = docker/dockerfile:1.29.0@sha256:abc\nFROM x": true,
		"# syntax=docker/dockerfile:1.7-labs\nFROM x\n":          true,
		"# syntax=docker.io/docker/dockerfile:latest\n":          true,
		"# syntax=docker/dockerfile:1.19\nFROM x\n":              false,
		"# syntax=docker/dockerfile:1.4\nFROM x\n":               false,
		"# syntax=docker/dockerfile:1.6-labs\nFROM x\n":          false,
		"# syntax=example.com/frontend:1\nFROM x\n":              false,
		"# a comment\n# check=skip=all\nFROM x\n":                true,
		"FROM x\n# syntax=docker/dockerfile:1.4\n":               true,
	} {
		if got := frontendHasParents(df); got != want {
			t.Errorf("%q: %v, want %v", df, got, want)
		}
	}
}

func TestStepOrderKeys(t *testing.T) {
	for _, c := range []struct {
		a, b       string
		same, next bool
	}{
		{"[builder 4/5]", "[builder 5/5]", true, true},
		{"[builder  9/16]", "[builder 10/16]", true, true},
		{"[3/5]", "[4/5]", true, true},
		{"[3/5]", "[5/5]", true, false},
		{"[builder 4/5]", "[stage-1 2/5]", false, false},
		{"[build 4/8]", "[builder 5/8]", false, false},
	} {
		if sameStage(c.a, c.b) != c.same || next(c.a, c.b) != c.next {
			t.Errorf("%s, %s: same %v next %v", c.a, c.b, sameStage(c.a, c.b), next(c.a, c.b))
		}
	}
}
