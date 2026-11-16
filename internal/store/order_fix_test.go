package store

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// venv is the first RUN of the TradingAgents Dockerfile, before its "COPY
// . .": not an install. The tests below put an install of the same length
// in its place (the provenance is length-framed).
const venv = "python -m venv /opt/venv"

// loadOrderEdited is loadOrder with the first RUN's command replaced.
func loadOrderEdited(t *testing.T, name, with string) *history.Build {
	t.Helper()
	if len(with) != len(venv) {
		t.Fatalf("%q is not %d bytes long", with, len(venv))
	}
	d := filepath.Join("testdata", "order", name)
	read := func(f string) []byte {
		b, err := os.ReadFile(filepath.Join(d, f))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	prov := read("provenance.json")
	if bytes.Count(prov, []byte(venv)) != 1 {
		t.Fatalf("%s: the provenance does not hold %q once", name, venv)
	}
	prov = bytes.Replace(prov, []byte(venv), []byte(with), 1)
	rec, err := history.ParseRecord(read("record.json"))
	if err != nil {
		t.Fatal(err)
	}
	progress, err := history.DecodeProgress(bytes.NewReader(read("logs.bin")))
	if err != nil {
		t.Fatal(err)
	}
	b, err := history.NewBuild(rec, progress, [][]byte{prov}, nil, nil, history.Config{SecretKey: []byte("test")})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// saveSettled stores builds of one project and image and settles them by
// GitLab's comparison with the changed files given.
func saveSettled(t *testing.T, s *Store, builds []Build, paths [][]string) []int64 {
	t.Helper()
	ids := make([]int64, len(builds))
	for i, b := range builds {
		id, err := s.SaveBuild(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE builds SET instance = 1, project_id = 5, target = ''"); err != nil {
		t.Fatal(err)
	}
	var settled []Settled
	for i, p := range paths {
		if p != nil {
			settled = append(settled, Settled{ID: ids[i], Kind: history.FilesChanged, Paths: p, Via: "gitlab aaaaaaaa..bbbbbbbb"})
		}
	}
	if err := s.SettleElsewhere(ctx, settled); err != nil {
		t.Fatal(err)
	}
	return ids
}

func later(b *history.Build, ref string, d time.Duration) *history.Build {
	x := *b
	x.Ref, x.Started, x.Completed = ref, b.Started.Add(d), b.Completed.Add(d)
	return &x
}

// "Order fixed" says the dependencies came from the cache because the
// Dockerfile installs them before the code now: only an install that reads
// the project's files, of a manager the earlier build installed again.
func TestOrderFixedNeedsTheSameInstall(t *testing.T) {
	c := &clock{time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	ta1, ta2 := loadOrder(t, "tradingagents-1"), loadOrder(t, "tradingagents-2")
	// As stored: "cache elsewhere" until GitLab's comparison settles it.
	r2, _ := history.FirstRebuild(ta1, ta2)
	// A tool installed before the copy (named packages, no project file),
	// the order never fixed, a dependency file changed: pip install . ran
	// again for a good reason.
	tool := later(loadOrderEdited(t, "tradingagents-2", "uv pip install ruff mypy"), "tool", time.Hour)
	// The order fixed: the requirements installed before the copy, from the
	// cache.
	fixed := later(loadOrderEdited(t, "tradingagents-2", "pip install -r a/req.txt"), "fixed", 2*time.Hour)
	ids := saveSettled(t, s, []Build{{Build: ta1, Generation: 1}, {Build: ta2, Generation: 1, Reason: r2},
		{Build: tool, Generation: 1, Reason: r2}, {Build: fixed, Generation: 1, Reason: r2}},
		[][]string{nil, {"main.py"}, {"main.py", "pyproject.toml"}, {"main.py"}})
	p, err := s.BuildPage(ctx, ids[2])
	if err != nil {
		t.Fatal(err)
	}
	if p.OrderFixed != nil || len(p.Order) != 0 {
		t.Errorf("a tool from the cache, pip install . run again: order %+v, fixed %+v", p.Order, p.OrderFixed)
	}
	p, err = s.BuildPage(ctx, ids[3])
	if err != nil {
		t.Fatal(err)
	}
	f := p.OrderFixed
	if len(p.Order) != 0 || f == nil || strings.Join(f.Managers, ",") != "pip" || f.Before != ids[1] || f.Wasted < 30*time.Second {
		t.Fatalf("the requirements from the cache: order %+v, fixed %+v", p.Order, f)
	}
	// A hint that stands is never "fixed" as well.
	st, err := s.Load(ctx, ids[3])
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.orderFixed(ctx, st, []OrderHint{{}}); err != nil || got != nil {
		t.Errorf("fixed while a hint stands: %+v %v", got, err)
	}

	// A tool installed before the copy does not install the project's
	// dependencies: the install after the copy still gets its hint.
	poetry := loadOrderEdited(t, "tradingagents-2", "pip install poetry black")
	_, df := ta2.Dockerfile()
	if hs := orderHints(poetry, rebuilt(t, ta1, ta2, "main.py"), "gitlab a..b", df); len(hs) != 1 || strings.Join(hs[0].Managers, ",") != "pip" {
		t.Errorf("pip install poetry before the copy: %+v", hs)
	}
}

// A project with several images built without a target (each pushes its
// own repository): the order figures and the step runs are each image's.
func TestOrderAcrossImages(t *testing.T) {
	c := &clock{time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	ta1, ta2 := loadOrder(t, "tradingagents-1"), loadOrder(t, "tradingagents-2")
	bu1, bu2 := loadOrder(t, "browser-use-1"), loadOrder(t, "browser-use-2")
	cr1, cr2 := later(ta1, "cron1", time.Second), later(ta2, "cron2", time.Second)
	type item struct {
		name string
		b    *history.Build
		repo string
		prev *history.Build
	}
	items := []item{{"api-1", ta1, "api", nil}, {"api-2", ta2, "api", ta1}, {"cron-1", cr1, "cron", nil}, {"cron-2", cr2, "cron", cr1},
		{"worker-1", bu1, "worker", nil}, {"worker-2", bu2, "worker", bu1}}
	ids := map[string]int64{}
	for i, it := range items {
		it.b.Target = ""
		it.b.Images = []string{"registry.example.com/group/app/" + it.repo + ":latest"}
		it.b.Daemon = "d"
		it.b.Attribution = history.Attribution{State: history.Attributed, Instance: "000001", Pipeline: uint64(10 + i), Job: uint64(100 + i)}
		if err := s.SaveJob(ctx, Job{Instance: "000001", JobID: uint64(100 + i), PipelineID: uint64(10 + i), ProjectID: 5, ProjectPath: "group/app", Name: "build-" + it.repo}); err != nil {
			t.Fatal(err)
		}
		var r history.Reason
		if it.prev != nil {
			r, _ = history.FirstRebuild(it.prev, it.b)
		}
		id, err := s.SaveBuild(ctx, Build{Build: it.b, Generation: 1, Reason: r})
		if err != nil {
			t.Fatal(err)
		}
		ids[it.name] = id
	}
	err := s.SettleElsewhere(ctx, []Settled{
		{ID: ids["api-2"], Kind: history.FilesChanged, Paths: []string{"main.py"}, Via: "gitlab d97865d7..0612237d"},
		{ID: ids["cron-2"], Kind: history.FilesChanged, Paths: []string{"main.py"}, Via: "gitlab d97865d7..0612237d"},
		{ID: ids["worker-2"], Kind: history.FilesChanged, Paths: []string{"docker/smoke.py"}, Via: "gitlab a99dc9f0..b0b0b0b0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	api, err := s.BuildPage(ctx, ids["api-2"])
	if err != nil {
		t.Fatal(err)
	}
	if len(api.Order) != 1 || api.Order[0].Runs != 1 || api.Order[0].RunTime != api.Order[0].Took {
		t.Errorf("api's order hint counts another image's builds: %+v", api.Order)
	}
	for _, h := range api.Hints {
		if h.Name == "RUN pip install --no-cache-dir ." && h.Runs != 2 {
			t.Errorf("api's %q ran %d times, want 2 (its own builds)", h.Name, h.Runs)
		}
	}
	worker, err := s.BuildPage(ctx, ids["worker-2"])
	if err != nil {
		t.Fatal(err)
	}
	if worker.OrderFixed != nil {
		t.Errorf("worker, never in the wrong order, is 'fixed' from another image's build: %+v", worker.OrderFixed)
	}
}

// The fragment replaces the lines from the COPY to the install: only when
// nothing but blank lines and comments lies between them.
func TestOrderFragmentBetween(t *testing.T) {
	ta1, ta2 := loadOrder(t, "tradingagents-1"), loadOrder(t, "tradingagents-2")
	_, df := ta2.Dockerfile()
	r := rebuilt(t, ta1, ta2, "main.py")
	lines := strings.Split(df, "\n")
	if len(lines) < 11 || !strings.HasPrefix(lines[9], "COPY . .") || !strings.HasPrefix(lines[10], "RUN pip install") {
		t.Fatalf("Dockerfile lines 10-11: %q", lines[9:min(11, len(lines))])
	}
	moved := func(between ...string) (*history.Build, string) {
		b := *ta2
		b.Steps = append([]history.Step(nil), ta2.Steps...)
		for i := range b.Steps {
			if b.Steps[i].Key == "[builder 5/5]" {
				n := 11 + len(between)
				b.Steps[i].Lines = []history.LineRange{{Start: n, End: n}}
			}
		}
		text := append(append(append([]string(nil), lines[:10]...), between...), lines[10:]...)
		return &b, strings.Join(text, "\n")
	}
	b, text := moved("ENV NODE_ENV=production", "USER app")
	if hs := orderHints(b, r, "gitlab a..b", text); len(hs) != 1 || hs[0].Fragment != "" {
		t.Errorf("ENV and USER between the COPY and the install: %+v", hs)
	}
	b, text = moved("", "# install")
	if hs := orderHints(b, r, "gitlab a..b", text); len(hs) != 1 || hs[0].Fragment == "" {
		t.Errorf("a blank line and a comment between: %+v", hs)
	}
}

// What keeps the order hint from a false positive.
func TestOrderHintsNot(t *testing.T) {
	ta1, ta2 := loadOrder(t, "tradingagents-1"), loadOrder(t, "tradingagents-2")
	_, df := ta2.Dockerfile()
	r := rebuilt(t, ta1, ta2, "main.py")
	edit := func(f func(s *history.Step)) *history.Build {
		b := *ta2
		b.Steps = append([]history.Step(nil), ta2.Steps...)
		for i := range b.Steps {
			if b.Steps[i].Key == "[builder 5/5]" {
				f(&b.Steps[i])
			}
		}
		return &b
	}
	for name, b := range map[string]*history.Build{
		"an install under a second":   edit(func(s *history.Step) { s.Completed = s.Started.Add(MinOrderTook - time.Millisecond) }),
		"an install of another stage": edit(func(s *history.Step) { s.Key = "[stage-1 2/2]" }),
		"an install from the cache":   edit(func(s *history.Step) { s.State = history.Cached }),
	} {
		if hs := orderHints(b, r, "gitlab a..b", df); len(hs) != 0 {
			t.Errorf("%s: %+v", name, hs)
		}
	}
}

// A build whose reason is replaced since GitLab's comparison proved it
// no longer counts as installed again because of the order.
func TestOrderTimeStale(t *testing.T) {
	c := &clock{time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	ta1, ta2 := loadOrder(t, "tradingagents-1"), loadOrder(t, "tradingagents-2")
	r2, _ := history.FirstRebuild(ta1, ta2)
	ids := saveSettled(t, s, []Build{{Build: ta1, Generation: 1}, {Build: ta2, Generation: 1, Reason: r2}}, [][]string{nil, {"main.py"}})
	orderOf := func() (stored, shown int64) {
		t.Helper()
		if err := s.db.QueryRowContext(ctx, "SELECT order_ms FROM builds WHERE id = ?", ids[1]).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		rows, err := s.Builds(ctx, BuildQuery{Project: ProjectKey{Instance: "000001", ID: 5}})
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range rows {
			if b.ID == ids[1] {
				shown = b.Order.Milliseconds()
			}
		}
		return stored, shown
	}
	if stored, shown := orderOf(); stored < 30000 || shown != stored {
		t.Fatalf("settled: order_ms %d, shown %d", stored, shown)
	}
	if err := s.SetReason(ctx, ta2.Ref, ta2.Daemon, history.Reason{Kind: history.OtherNode, Step: r2.Step}); err != nil {
		t.Fatal(err)
	}
	if stored, shown := orderOf(); stored != 0 || shown != 0 {
		t.Errorf("after another reason: order_ms %d, shown %d", stored, shown)
	}
	// A release that does not know the column leaves it as it was.
	if _, err := s.db.ExecContext(ctx, "UPDATE builds SET order_ms = 30000 WHERE id = ?", ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, shown := orderOf(); shown != 0 {
		t.Errorf("a stale order time is shown: %d ms", shown)
	}
	// The record carries no instance: the test gave the row one.
	if _, err := s.db.ExecContext(ctx, "UPDATE builds SET instance = NULL WHERE id = ?", ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveBuild(ctx, Build{Build: ta2, Generation: 2}); err != nil {
		t.Fatal(err)
	}
	if stored, _ := orderOf(); stored != 0 {
		t.Errorf("stored again: order_ms %d", stored)
	}
}

// tick is a clock that moves on a millisecond each time it is read.
type tick struct {
	mu sync.Mutex
	t  time.Time
}

func (c *tick) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Millisecond)
	return c.t
}

// The builds of one BuildHints answer share their window, so that the step
// runs read for one build serve the next of the same image.
func TestBuildHintsOneWindow(t *testing.T) {
	c := &tick{t: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	s, err := Open(ctx, t.TempDir(), Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ta1, ta2 := loadOrder(t, "tradingagents-1"), loadOrder(t, "tradingagents-2")
	var ids []int64
	for _, b := range []*history.Build{ta1, ta2, later(ta2, "ta3", time.Minute)} {
		id, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE builds SET instance = 1, project_id = 5, target = ''"); err != nil {
		t.Fatal(err)
	}
	got, err := s.BuildHints(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	var since []time.Time
	for _, hs := range got {
		for _, h := range hs {
			since = append(since, h.Since)
		}
	}
	if len(since) < 2 {
		t.Fatalf("hints of %d builds, want at least 2", len(got))
	}
	for _, x := range since[1:] {
		if !x.Equal(since[0]) {
			t.Errorf("one answer, windows from %s and %s", since[0], x)
		}
	}
}

// A step's hint gives the middle of its runs as the median.
func TestHintMedian(t *testing.T) {
	c := &clock{time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	ta2 := loadOrder(t, "tradingagents-2")
	const step = "RUN pip install --no-cache-dir ."
	var last int64
	for i, d := range []time.Duration{10 * time.Second, 30 * time.Second, 20 * time.Second} {
		b := later(ta2, fmt.Sprint("run", i), time.Duration(i)*time.Minute)
		b.Steps = append([]history.Step(nil), b.Steps...)
		for k := range b.Steps {
			if b.Steps[k].Name == step {
				b.Steps[k].Started = b.Started
				b.Steps[k].Completed = b.Started.Add(d)
			}
		}
		last = saveFix(t, s, b, 1)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE builds SET instance = 1, project_id = 5, target = ''"); err != nil {
		t.Fatal(err)
	}
	st, err := s.Load(ctx, last)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := s.Hints(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		if h.Name == step {
			if h.Runs != 3 || h.Median != 20*time.Second || h.RunTime != time.Minute {
				t.Errorf("runs %d, median %s, total %s: want 3, 20s, 1m", h.Runs, h.Median, h.RunTime)
			}
			return
		}
	}
	t.Errorf("no hint for %q: %+v", step, hs)
}
