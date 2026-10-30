package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

var ctx = context.Background()

// clock is a settable time for retention tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func open(t *testing.T, dir string, c *clock) *Store {
	t.Helper()
	s, err := Open(ctx, dir, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// fixtures reads the history package's recorded builds (one daemon, one
// project), keyed with the store's secret key, linked, oldest first.
func fixtures(t *testing.T, key []byte) []*history.Build {
	t.Helper()
	dirs, err := filepath.Glob("../history/testdata/records/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	cfg := history.Config{Instances: map[string]bool{history.InstancePrefix("https://gitlab.example.com/"): true}, SecretKey: key}
	read := func(d, n string) []byte {
		b, err := os.ReadFile(filepath.Join(d, n))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return b
	}
	var out []*history.Build
	for _, d := range dirs {
		rec, err := history.ParseRecord(read(d, "record.json"))
		if err != nil {
			t.Fatal(err)
		}
		progress, err := history.DecodeProgress(bytes.NewReader(read(d, "logs.bin")))
		if err != nil {
			t.Fatal(err)
		}
		b, err := history.NewBuild(rec, progress, [][]byte{read(d, "provenance.json")}, read(d, "trace.json"), read(d, "error.bin"), cfg)
		if err != nil {
			t.Fatal(err)
		}
		b.Daemon = "daemon-a"
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	history.Link(out)
	return out
}

func saveJobs(t *testing.T, s *Store, bs []*history.Build) {
	t.Helper()
	for _, b := range bs {
		if b.Attribution.State != history.Attributed {
			continue
		}
		err := s.SaveJob(ctx, Job{Instance: b.Attribution.Instance, JobID: b.Attribution.Job, PipelineID: b.Attribution.Pipeline,
			ProjectID: 42, ProjectPath: "example/app", Name: "build-" + b.Target, Stage: "build", CommitTitle: "a change"})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func reasonView(r history.Reason) []any {
	return []any{r.Kind, r.Step.Key, r.Step.Identity, r.Old, r.New, r.Arg, r.Paths, r.By}
}

// Every build saved, then compared with its predecessor as the store gives
// it back: the reasons are the same as with the predecessor in memory.
func TestReasonsAgainstStoredBuilds(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	kinds := map[history.ReasonKind]bool{}
	for i, b := range bs {
		// The store compares only builds of a known project: attributed ones.
		var mem *history.Build
		for _, p := range bs[:i] {
			if p.Target == b.Target && p.Attribution.State == history.Attributed && b.Attribution.State == history.Attributed {
				mem = p
			}
		}
		want, _ := history.FirstRebuild(mem, b)
		stored, _, err := s.Previous(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		if (stored == nil) != (mem == nil) {
			t.Fatalf("%s: previous in the store %v, in memory %v", b.Ref, stored != nil, mem != nil)
		}
		var prev *history.Build
		if stored != nil {
			prev = stored.Build
			if prev.Ref != mem.Ref {
				t.Fatalf("%s: previous %s, want %s", b.Ref, prev.Ref, mem.Ref)
			}
		}
		got, _ := history.FirstRebuild(prev, b)
		if !reflect.DeepEqual(reasonView(got), reasonView(want)) {
			t.Errorf("%s: %v, want %v", b.Ref, reasonView(got), reasonView(want))
		}
		kinds[got.Kind] = true
		id, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1, Reason: got})
		if err != nil {
			t.Fatal(err)
		}
		// From the store alone: both builds restored.
		if stored != nil && b.Attribution.State == history.Attributed {
			cur, err := s.Load(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			again, _ := history.FirstRebuild(stored.Build, cur.Build)
			if !reflect.DeepEqual(reasonView(again), reasonView(want)) {
				t.Errorf("%s from the store alone: %v, want %v", b.Ref, reasonView(again), reasonView(want))
			}
		}
	}
	for _, k := range []history.ReasonKind{history.FirstBuild, history.StepChanged, history.ArgChanged, history.BaseImageChanged, history.CacheElsewhere} {
		if !kinds[k] {
			t.Errorf("no build gave %s", k)
		}
	}
	// Time saved: for each step a build did not execute, how long it took
	// when it was last built in the project.
	for i, b := range bs {
		if b.Attribution.State != history.Attributed {
			continue
		}
		var want int64
		for _, st := range b.Steps {
			if st.Identity == "" || st.State == history.Built {
				continue
			}
			for j := i - 1; j >= 0; j-- {
				if p := bs[j]; p.Attribution.State == history.Attributed {
					if ps := stepByIdentity(p, st.Identity); ps != nil && ps.State == history.Built {
						want += ps.Duration().Milliseconds()
						break
					}
				}
			}
		}
		var saved int64
		if err := s.db.QueryRow("SELECT saved_ms FROM builds WHERE ref = ?", b.Ref).Scan(&saved); err != nil {
			t.Fatal(err)
		}
		if saved != want {
			t.Errorf("%s saved %d ms, want %d", b.Ref, saved, want)
		}
	}
	// A step's name is kept once per identity.
	var dup int
	if err := s.db.QueryRow("SELECT count(*) FROM steps WHERE identity IS NOT NULL AND name IS NOT NULL").Scan(&dup); err != nil || dup != 0 {
		t.Errorf("%d steps repeat their definition's name (%v)", dup, err)
	}
}

func stepByIdentity(b *history.Build, id string) *history.Step {
	for i := range b.Steps {
		if b.Steps[i].Identity == id {
			return &b.Steps[i]
		}
	}
	return nil
}

// A build read back has the steps, states, lines, links and reason it was
// saved with.
func TestBuildsReadBack(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	// A step whose identity an earlier build stored under another name
	// keeps its own name.
	renamed := false
	last := bs[len(bs)-1]
	for i, st := range last.Steps {
		if st.Identity != "" && stepByIdentity(bs[0], st.Identity) != nil {
			last.Steps[i].Name += " # renamed"
			renamed = true
			break
		}
	}
	if !renamed {
		t.Fatal("no step shared with the first build")
	}
	for _, b := range bs {
		r := history.Reason{}
		if b.Target == "worker" {
			r = history.Reason{Kind: history.CacheElsewhere, Step: b.Steps[len(b.Steps)-1], Old: "daemon-x", New: "daemon-a", Paths: []string{"worker"}, By: bs[0].Ref}
		}
		if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1, Reason: r}); err != nil {
			t.Fatal(err)
		}
	}
	checked := 0
	for _, b := range bs {
		if b.Attribution.State != history.Attributed {
			continue
		}
		got, err := s.Pipeline(ctx, b.Attribution.Instance, b.Attribution.Pipeline)
		if err != nil {
			t.Fatal(err)
		}
		var st *Stored
		for _, g := range got {
			if g.Build.Ref == b.Ref {
				st = g
			}
		}
		if st == nil {
			t.Fatalf("%s not in its pipeline", b.Ref)
		}
		if len(st.Build.Steps) != len(b.Steps) {
			t.Fatalf("%s: %d steps, want %d", b.Ref, len(st.Build.Steps), len(b.Steps))
		}
		for i, want := range b.Steps {
			g := st.Build.Steps[i]
			if g.Key != want.Key || g.Name != want.Name || g.Identity != want.Identity || g.State != want.State ||
				!reflect.DeepEqual(g.Lines, want.Lines) || g.ReceivedFrom != want.ReceivedFrom ||
				g.Duration().Milliseconds() != want.Duration().Milliseconds() {
				t.Errorf("%s step %d:\n got %+v\nwant %+v", b.Ref, i, g, want)
			}
		}
		if b.Target == "worker" && (st.Reason.Kind != history.CacheElsewhere || st.Reason.Step.Key != b.Steps[len(b.Steps)-1].Key ||
			st.Reason.By != bs[0].Ref || strings.Join(st.Reason.Paths, ",") != "worker") {
			t.Errorf("%s reason: %+v", b.Ref, st.Reason)
		}
		checked++
	}
	if checked < 10 {
		t.Fatalf("checked %d builds", checked)
	}
}

func TestStepOutput(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	found := 0
	for _, b := range bs {
		id, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1})
		if err != nil {
			t.Fatal(err)
		}
		for i, st := range b.Steps {
			got, err := s.Output(ctx, id, i)
			if err != nil {
				t.Fatal(err)
			}
			if got != st.Output {
				t.Errorf("%s step %d: %q, want %q", b.Ref, i, got, st.Output)
			}
			if st.Output != "" {
				found++
			}
		}
	}
	if found == 0 {
		t.Fatal("no fixture step has output")
	}
}

// Output of one project compresses with a dictionary built from its earlier
// output, also across days, and reads back exactly.
func TestOutputDictionaries(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	s := open(t, dir, c)
	base := fixtures(t, s.SecretKey())[0]
	saveJobs(t, s, []*history.Build{base})
	pip := func(n int) string {
		var b strings.Builder
		for i := 0; i < 60; i++ {
			b.WriteString("Collecting package-" + string(rune('a'+i%26)) + "==1.2." + string(rune('0'+i%10)) + " (from -r requirements.txt (line 3))\n")
			b.WriteString("  Using cached package-" + string(rune('a'+i%26)) + "-1.2.0-py3-none-any.whl.metadata (4.4 kB)\n")
		}
		b.WriteString("Successfully installed build " + string(rune('A'+n%26)) + "\n")
		return b.String()
	}
	write := func(n int) (int64, string) {
		b := *base
		b.Ref = "ref-" + string(rune('a'+n))
		b.Started = base.Started.Add(time.Duration(n) * time.Minute)
		b.Completed = c.t
		b.Steps = append([]history.Step(nil), base.Steps...)
		out := pip(n)
		b.Steps[0].Output = out
		id, err := s.SaveBuild(ctx, Build{Build: &b, Generation: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id, out
	}
	sizes := func() (plain, dict int) {
		f, err := s.output(ctx, day(c.t), false)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := f.db.Query("SELECT dict_id, length(data) FROM outputs")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var d sql.NullInt64
			var n int
			rows.Scan(&d, &n)
			if d.Valid {
				dict += n
			} else {
				plain += n
			}
		}
		return
	}
	var written [][2]any
	for n := 0; n < 3; n++ {
		id, out := write(n)
		written = append(written, [2]any{id, out})
	}
	if plain, dict := sizes(); plain == 0 || dict != 0 {
		t.Fatalf("first day, first writes: plain %d, dict %d; want plain only", plain, dict)
	}
	c.t = c.t.Add(24 * time.Hour) // a new file, with a dictionary from yesterday's output
	for n := 3; n < 6; n++ {
		id, out := write(n)
		written = append(written, [2]any{id, out})
	}
	plain, dict := sizes()
	if plain != 0 || dict == 0 {
		t.Fatalf("second day: plain %d, dict %d; want dictionary blocks only", plain, dict)
	}
	one := len(pip(0))
	if dict/3 > one/12 {
		t.Errorf("with a dictionary a block is %d bytes for %d of output", dict/3, one)
	}
	s.Close()
	s = open(t, dir, c) // dictionaries are read back from the files
	for _, w := range written {
		got, err := s.Output(ctx, w[0].(int64), 0)
		if err != nil || got != w[1].(string) {
			t.Fatalf("build %d: %v, %d bytes, want %d", w[0], err, len(got), len(w[1].(string)))
		}
	}
}

func TestPrune(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	s := open(t, dir, c)
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	var withOutput int64
	for _, b := range bs {
		b.Completed = c.t // as if every build ended now
		id, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1})
		if err != nil {
			t.Fatal(err)
		}
		for _, st := range b.Steps {
			if st.Output != "" {
				withOutput = id
			}
		}
	}
	count := func(q string) (n int) {
		if err := s.db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return
	}
	c.t = c.t.AddDate(0, 0, 6)
	if _, err := s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "output-*.db")); len(files) != 1 {
		t.Errorf("after 6 days: %d output files, want 1", len(files))
	}
	c.t = c.t.AddDate(0, 0, 1)
	if _, err := s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "output-*.db*")); len(files) != 0 {
		t.Errorf("after 7 days: output files %v", files)
	}
	if out, err := s.Output(ctx, withOutput, 0); err != nil || out != "" {
		t.Errorf("output after its file is gone: %q, %v", out, err)
	}
	if n := count("SELECT count(*) FROM builds"); n != len(bs) {
		t.Errorf("after 7 days: %d builds, want %d", n, len(bs))
	}
	c.t = c.t.AddDate(0, 0, 84)
	deleted, err := s.Prune(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != int64(len(bs)) {
		t.Errorf("after 91 days: deleted %d builds, want %d", deleted, len(bs))
	}
	for _, table := range []string{"builds", "steps", "step_defs", "dockerfiles"} {
		if n := count("SELECT count(*) FROM " + table); n != 0 {
			t.Errorf("after 91 days: %d rows in %s", n, table)
		}
	}
}

func TestGenerationsAndSecrets(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	b := fixtures(t, s.SecretKey())[1]
	b.BuildArgs = map[string]string{"NPM_TOKEN": "s3cr3t", "VERSION": "2"}
	newer := *b
	newer.Target = "renamed"
	if _, err := s.SaveBuild(ctx, Build{Build: &newer, Generation: 2}); err != nil {
		t.Fatal(err)
	}
	id, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	var target, args string
	if err := s.db.QueryRow("SELECT target, build_args FROM builds WHERE id = ?", id).Scan(&target, &args); err != nil {
		t.Fatal(err)
	}
	if target != "renamed" {
		t.Errorf("an older generation replaced a newer one: target %q", target)
	}
	if strings.Contains(args, "s3cr3t") || !strings.Contains(args, `"VERSION":"2"`) {
		t.Errorf("stored build arguments: %s", args)
	}
	var ops []byte
	if err := s.db.QueryRow("SELECT group_concat(op) FROM step_defs").Scan(&ops); err != nil || len(ops) == 0 {
		t.Fatal(err)
	}
}

func TestKeyAndSchemaVersion(t *testing.T) {
	c := &clock{time.Now()}
	dir := t.TempDir()
	s := open(t, dir, c)
	key := s.SecretKey()
	if len(key) != 32 {
		t.Fatalf("key of %d bytes", len(key))
	}
	s.Close()
	s = open(t, dir, c)
	if !bytes.Equal(s.SecretKey(), key) {
		t.Error("the secret key changed on reopening")
	}
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(ctx, dir, Options{}); !errors.Is(err, ErrNewerSchema) {
		t.Errorf("a newer schema: %v", err)
	}
}

// Builds saved before their job was known get its project when it arrives.
func TestJobArrivesAfterItsBuild(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	var a, b *history.Build
	for _, x := range bs {
		if x.Target == "api" && x.Attribution.State == history.Attributed {
			if a == nil {
				a = x
			} else if b == nil {
				b = x
			}
		}
	}
	if _, err := s.SaveBuild(ctx, Build{Build: a, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if p, known, err := s.Previous(ctx, b); err != nil || p != nil || known {
		t.Fatalf("previous without a known project: %v, known %v, %v", p, known, err)
	}
	saveJobs(t, s, []*history.Build{a, b})
	p, known, err := s.Previous(ctx, b)
	if err != nil || !known || p == nil || p.Build.Ref != a.Ref {
		t.Fatalf("previous once the jobs are known: %+v, known %v, %v", p, known, err)
	}
	ids, err := s.JobBuilds(ctx, a.Attribution.Instance, a.Attribution.Job)
	if err != nil || len(ids) != 1 {
		t.Fatalf("builds of the job: %v, %v", ids, err)
	}
}

// Steps past MaxSteps keep no output, and asking for one never reads
// another build's block: the step index must not carry into the build id
// bits of the output key.
func TestOutputPastMaxSteps(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	base := fixtures(t, s.SecretKey())[0]
	saveJobs(t, s, []*history.Build{base})
	save := func(ref string, n int, out string) int64 {
		t.Helper()
		b := *base
		b.Ref = ref
		b.Completed = c.t
		b.Steps = make([]history.Step, n)
		for i := range b.Steps {
			b.Steps[i] = base.Steps[0]
			b.Steps[i].Key = fmt.Sprintf("[%d/%d]", i, n)
			b.Steps[i].Output = fmt.Sprintf("%s step %d", out, i)
		}
		id, err := s.SaveBuild(ctx, Build{Build: &b, Generation: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	if id := save("filler", 1, "F"); id%2 != 1 {
		t.Fatalf("filler build id %d, want odd", id)
	}
	big := save("big", MaxSteps+2, "A")
	save("next", 3, "V")
	p, err := s.BuildPage(ctx, big)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Printed[MaxSteps-1] || p.Printed[MaxSteps] || p.Printed[MaxSteps+1] {
		t.Errorf("printed %d/%d/%d, want true/false/false", MaxSteps-1, MaxSteps, MaxSteps+1)
	}
	var past int
	if err := s.db.QueryRow("SELECT count(*) FROM steps WHERE build_id = ? AND idx >= ? AND output_len > 0", big, MaxSteps).Scan(&past); err != nil || past != 0 {
		t.Errorf("%d steps past MaxSteps stored as printed, %v", past, err)
	}
	// Rows saved by earlier versions carry the length of every step.
	if _, err := s.db.Exec("UPDATE steps SET output_len = 9 WHERE build_id = ?", big); err != nil {
		t.Fatal(err)
	}
	if p, err = s.BuildPage(ctx, big); err != nil || p.Printed[MaxSteps] {
		t.Errorf("earlier rows: printed %d %v, %v", MaxSteps, p.Printed[MaxSteps], err)
	}
	for _, step := range []int{-1, MaxSteps, MaxSteps + 1} {
		if got, err := s.Output(ctx, big, step); err != nil || got != "" {
			t.Errorf("step %d: %q, %v; want nothing", step, got, err)
		}
	}
	if got, err := s.Output(ctx, big, MaxSteps-1); err != nil || got != fmt.Sprintf("A step %d", MaxSteps-1) {
		t.Errorf("last kept step: %q, %v", got, err)
	}
}
