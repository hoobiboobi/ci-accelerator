package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// fixBuild is a small build of one step, of a job when job is not 0.
func fixBuild(ref string, started time.Time, pipeline, job uint64) *history.Build {
	b := &history.Build{Ref: ref, Daemon: "d", Target: "api", Started: started, Completed: started.Add(time.Minute),
		Steps: []history.Step{{Key: "[1/1]", Name: "RUN make", State: history.Built, Started: started, Completed: started.Add(time.Minute)}}}
	if job != 0 {
		b.Attribution = history.Attribution{State: history.Attributed, Instance: "000001", Pipeline: pipeline, Job: job}
	}
	return b
}

func saveFix(t *testing.T, s *Store, b *history.Build, gen int64) int64 {
	t.Helper()
	id, err := s.SaveBuild(ctx, Build{Build: b, Generation: gen})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A record that reuses a stored build's ref (the ref is the client's
// choice) does not replace that build: neither its content nor its job.
func TestSaveBuildRefReused(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), &clock{now})
	if err := s.SaveJob(ctx, Job{Instance: "000001", JobID: 1011, PipelineID: 108, ProjectID: 42, ProjectPath: "g/victim", Name: "build"}); err != nil {
		t.Fatal(err)
	}
	victim := fixBuild("victimref", now.Add(-time.Hour), 108, 1011)
	id := saveFix(t, s, victim, 1)
	for name, forged := range map[string]*history.Build{
		"its own job":                 fixBuild("victimref", now.Add(-time.Minute), 900, 9001),
		"no job":                      fixBuild("victimref", now.Add(-time.Minute), 0, 0),
		"the victim's job":            fixBuild("victimref", now.Add(-time.Minute), 108, 1011),
		"the same start, another job": fixBuild("victimref", victim.Started, 900, 9001),
	} {
		forged.Target = "forged"
		for _, gen := range []int64{1, 2} {
			if got := saveFix(t, s, forged, gen); got != id {
				t.Errorf("%s: id %d, want %d", name, got, id)
			}
			st, err := s.Load(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if st.Build.Target != "api" || st.Build.Attribution.Job != 1011 || st.Build.Attribution.Pipeline != 108 {
				t.Errorf("%s, generation %d: the stored build became %q of job %d", name, gen, st.Build.Target, st.Build.Attribution.Job)
			}
		}
	}
	// The same record again, its trace attached since: it takes the job.
	late := fixBuild("lateref", now.Add(-time.Hour), 0, 0)
	lid := saveFix(t, s, late, 1)
	late = fixBuild("lateref", late.Started, 108, 1011)
	if got := saveFix(t, s, late, 2); got != lid {
		t.Fatalf("id %d, want %d", got, lid)
	}
	if st, err := s.Load(ctx, lid); err != nil || st.Build.Attribution.Job != 1011 {
		t.Errorf("the trace attached later: %+v %v", st.Build.Attribution, err)
	}
}

// Once a pipeline's project is known, a build of another project or of no
// known job that names the pipeline is no witness for its builds.
func TestPipelinePool(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), &clock{now})
	if err := s.SaveJob(ctx, Job{Instance: "000001", JobID: 1011, PipelineID: 108, ProjectID: 42, ProjectPath: "g/victim", Name: "build"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveJob(ctx, Job{Instance: "000001", JobID: 6601, PipelineID: 66, ProjectID: 66, ProjectPath: "g/other", Name: "build"}); err != nil {
		t.Fatal(err)
	}
	saveFix(t, s, fixBuild("victim", now.Add(-time.Hour), 108, 1011), 1)
	saveFix(t, s, fixBuild("nojob", now.Add(-time.Hour), 108, 999999), 1)
	saveFix(t, s, fixBuild("other", now.Add(-time.Hour), 108, 6601), 1)
	got, err := s.Pipeline(ctx, "000001", 108)
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, st := range got {
		refs = append(refs, st.Build.Ref)
	}
	if strings.Join(refs, ",") != "victim" {
		t.Errorf("pipeline 108's builds: %v, want its project's only", refs)
	}
	// A pipeline no job of which is known keeps all its builds.
	saveFix(t, s, fixBuild("a", now.Add(-time.Hour), 500, 501), 1)
	saveFix(t, s, fixBuild("b", now.Add(-time.Hour), 500, 502), 1)
	if got, err := s.Pipeline(ctx, "000001", 500); err != nil || len(got) != 2 {
		t.Errorf("a pipeline of no known project: %d builds, %v", len(got), err)
	}
}

// A build argument's value that holds a token or a password is masked
// whatever the argument's name.
func TestBuildArgsMasked(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), &clock{now})
	b := fixBuild("args", now.Add(-time.Hour), 0, 0)
	b.BuildArgs = map[string]string{"GL_PAT": "glpat-AbCdEfGhIjKlMnOpQrSt", "REGISTRY": "https://deploy:Sup3rS3cret@registry.example",
		"NPM_TOKEN": "abcd", "VERSION": "1.2.3"}
	id := saveFix(t, s, b, 1)
	st, err := s.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"GL_PAT": history.Masked, "REGISTRY": "https://deploy:" + history.Masked + "@registry.example",
		"NPM_TOKEN": history.Masked, "VERSION": "1.2.3"}
	for k, v := range want {
		if st.Build.BuildArgs[k] != v {
			t.Errorf("%s = %q, want %q", k, st.Build.BuildArgs[k], v)
		}
	}
}

// The store's files are its owner's only.
func TestStoreFilesOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "console.db-wal"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	s := open(t, dir, &clock{now})
	b := fixBuild("out", now.Add(-time.Hour), 0, 0)
	b.Steps[0].Output = "hello\n"
	saveFix(t, s, b, 1)
	names, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, name := range names {
		fi, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s: mode %s", filepath.Base(name), fi.Mode().Perm())
		}
		if strings.HasPrefix(filepath.Base(name), "output-") {
			n++
		}
	}
	if n == 0 {
		t.Errorf("no output file in %v", names)
	}
}

// Ids no GitLab issues (2^63 and above) read as nothing, as everywhere
// else, rather than fail in the driver.
func TestIDsOutOfRange(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), &clock{now})
	big := uint64(1) << 63
	if _, _, err := s.ProjectCache(ctx, ProjectKey{Instance: "000001", ID: big}); err != nil {
		t.Errorf("ProjectCache: %v", err)
	}
	if _, err := s.JobRoute(ctx, "000001", big, "d", now); err != nil {
		t.Errorf("JobRoute: %v", err)
	}
	if _, err := s.SaveAPIPipeline(ctx, APIPipeline{Instance: "000001", ID: 5, ProjectID: 42, Jobs: []APIJob{{ID: big, Name: "x"}}, Read: now}); err != nil {
		t.Errorf("SaveAPIPipeline: %v", err)
	}
	objs := []CacheObject{{Store: "s", ProjectID: big, Key: "k", Bytes: 1, Modified: now}, {Store: "s", ProjectID: 42, Key: "k", Bytes: 1, Modified: now}}
	if err := s.ReplaceCacheScan(ctx, "s", "000001", now, objs, nil, nil); err != nil {
		t.Errorf("ReplaceCacheScan: %v", err)
	}
	if got, _, err := s.ProjectCache(ctx, ProjectKey{Instance: "000001", ID: 42}); err != nil || len(got) != 1 {
		t.Errorf("the other project's archive: %v %v", got, err)
	}
	err := s.SaveSession(ctx, Session{ID: []byte("x"), Instance: "h", UserID: big, Username: "u", Token: []byte("t"), Created: now, Seen: now})
	if err == nil || !strings.HasPrefix(err.Error(), "store:") {
		t.Errorf("SaveSession: %v, want the store's refusal", err)
	}
}

// Search ignores case in any script.
func TestPipelineSearchCase(t *testing.T) {
	s := open(t, t.TempDir(), &clock{time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)})
	for i, title := range []string{"Fix the Build", "Исправить сборку", "Ärger mit Cache", "100%_done\\x"} {
		if err := s.SaveJob(ctx, Job{Instance: "000001", JobID: uint64(100 + i), PipelineID: uint64(10 + i), ProjectID: 42, ProjectPath: "g/p", Name: "build", CommitTitle: title, Ref: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	for q, want := range map[string]string{"FIX": "10", "исправить": "11", "ИСПРАВИТЬ": "11", "ärger": "12", "ÄRGER": "12",
		"%": "13", "_": "13", "\\": "13", "сборку": "11", "ничего": ""} {
		rows, err := s.PipelineList(ctx, PipeQuery{Project: ProjectKey{Instance: "000001", ID: 42}, Search: q})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range rows {
			ids = append(ids, fmt.Sprint(r.ID))
		}
		if strings.Join(ids, ",") != want {
			t.Errorf("search %q: %v, want %s", q, ids, want)
		}
	}
}

// A reader that took a day's output file before the hourly prune closed it
// finds nothing kept, not an error.
func TestOutputPrunedWhileRead(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), &clock{now})
	b := fixBuild("out", now.Add(-time.Hour), 0, 0)
	b.Steps[0].Output = "hello\n"
	id := saveFix(t, s, b, 1)
	f, err := s.output(ctx, day(b.Completed), false)
	if err != nil {
		t.Fatal(err)
	}
	// What the prune does to it while the reader waits for its lock.
	if err := f.close(); err != nil {
		t.Fatal(err)
	}
	if out, err := s.Output(ctx, id, 0); err != nil || out != "" {
		t.Errorf("output of a pruned file: %q %v", out, err)
	}
}

// Retention keeps a job, its project and a Dockerfile while a kept build
// needs them, judges a build by its end, and forgets projects nothing is
// kept of.
func TestPruneKeeps(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	c := &clock{now}
	s := open(t, t.TempDir(), c)
	cutoff := now.Add(-30 * 24 * time.Hour)
	// A long build at the edge of retention: started before the cutoff,
	// ended after it.
	edge := fixBuild("edge", cutoff.Add(-time.Hour), 7, 70)
	edge.Steps[0].Completed = cutoff.Add(time.Hour)
	edge.Completed = cutoff.Add(time.Hour)
	if err := s.SaveJob(ctx, Job{Instance: "000001", JobID: 70, PipelineID: 7, ProjectID: 42, ProjectPath: "g/kept", Name: "build", StartedAt: edge.Started}); err != nil {
		t.Fatal(err)
	}
	saveFix(t, s, edge, 1)
	// A project whose only build is past retention, seen back then.
	c.t = cutoff.Add(-48 * time.Hour)
	if err := s.SaveJob(ctx, Job{Instance: "000001", JobID: 80, PipelineID: 8, ProjectID: 77, ProjectPath: "g/gone", Name: "build", StartedAt: c.t}); err != nil {
		t.Fatal(err)
	}
	saveFix(t, s, fixBuild("old", c.t, 8, 80), 1)
	c.t = now
	// A kept build whose Dockerfile was last seen long ago.
	ta2 := loadOrder(t, "tradingagents-2")
	ta2.Started, ta2.Completed = now.Add(-2*time.Hour), now.Add(-time.Hour)
	ta := saveFix(t, s, ta2, 1)
	if _, err := s.db.ExecContext(ctx, "UPDATE dockerfiles SET last_seen = 0"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if j, err := s.Job(ctx, "000001", 70); err != nil || j == nil {
		t.Errorf("the kept build's job: %v %v", j, err)
	}
	ids, err := s.JobBuilds(ctx, "000001", 70)
	if err != nil || len(ids) != 1 {
		t.Errorf("the build that ended after the cutoff: %v %v", ids, err)
	}
	projects := func() string {
		t.Helper()
		ps, err := s.Projects(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, p := range ps {
			names = append(names, p.Path)
		}
		return strings.Join(names, ",")
	}
	// The project's daily figures are kept a year: so is the project.
	if got := projects(); got != "g/kept,g/gone" {
		t.Errorf("projects after retention: %v", got)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM image_days WHERE project_id = 77"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if got := projects(); got != "g/kept" {
		t.Errorf("projects once nothing of one is kept: %v", got)
	}
	p, err := s.BuildPage(ctx, ta)
	if err != nil {
		t.Fatal(err)
	}
	if p.Dockerfile == "" {
		t.Errorf("a kept build lost its Dockerfile")
	}
}

// A failing rollup (a full volume) still lets retention delete the output
// files past retention and the builds of the days summed up before.
func TestPruneWhenRollupFails(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	c := &clock{now.Add(-20 * 24 * time.Hour)}
	dir := t.TempDir()
	s := open(t, dir, c)
	if err := s.SaveJob(ctx, Job{Instance: "000001", JobID: 70, PipelineID: 7, ProjectID: 42, ProjectPath: "g/p", Name: "build"}); err != nil {
		t.Fatal(err)
	}
	saveFix(t, s, fixBuild("old", now.Add(-40*24*time.Hour), 7, 70), 1)
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	c.t = now
	old := filepath.Join(dir, outputName(day(now.Add(-10*24*time.Hour))))
	if err := os.WriteFile(old, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER no_room BEFORE INSERT ON image_days BEGIN SELECT RAISE(FAIL, 'no room'); END`); err != nil {
		t.Fatal(err)
	}
	saveFix(t, s, fixBuild("recent", now.Add(-2*24*time.Hour), 7, 70), 1)
	n, err := s.Prune(ctx)
	if err == nil {
		t.Errorf("the rollup's error is not reported")
	}
	if n != 1 {
		t.Errorf("%d builds deleted, want the one of a day summed up", n)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the output file past retention: %v", err)
	}
}

// Days summed up out of turn (a settled rebuild) do not make Rollup skip
// the days before them.
func TestRollupAfterSettle(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	c := &clock{now.Add(-9 * 24 * time.Hour)}
	s := open(t, t.TempDir(), c)
	if _, err := s.db.Exec("INSERT INTO daemons (id, name) VALUES (1, 'd')"); err != nil {
		t.Fatal(err)
	}
	ins := func(id int, daysAgo int, kind string) {
		ts := now.Add(-time.Duration(daysAgo) * 24 * time.Hour).UnixMilli()
		if _, err := s.db.Exec("INSERT INTO builds (id, daemon_id, ref, generation, attribution, instance, project_id, target, started_at, completed_at, reason_kind, built) VALUES (?, 1, ?, 1, 1, 1, 42, 'api', ?, ?, ?, 1)",
			id, fmt.Sprint("r", id), ts, ts+1000, kind); err != nil {
			t.Fatal(err)
		}
	}
	ins(1, 10, "")
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	// The controller was away; the daemons' history brings the builds of
	// the days since, and one of yesterday's settles before the rollup.
	c.t = now
	for d := 9; d >= 1; d-- {
		ins(10+d, d, "cache-elsewhere")
	}
	if err := s.SettleElsewhere(ctx, []Settled{{ID: 11, Kind: history.FilesChanged, Paths: []string{"a"}, Via: "gitlab a..b"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	days, err := s.ImageDays(ctx, ProjectKey{Instance: "000001", ID: 42}, "api", now.Add(-20*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 10 {
		var got []string
		for _, d := range days {
			got = append(got, d.Day.Format("01-02"))
		}
		t.Errorf("daily figures of %d days, want 10: %v", len(days), got)
	}
}

// On a full volume (STORE_FULL_DIR: a small tmpfs, on the lab machine)
// retention still makes room, and the store opens again.
func TestStoreFullVolume(t *testing.T) {
	root := os.Getenv("STORE_FULL_DIR")
	if root == "" {
		t.Skip("STORE_FULL_DIR is not set")
	}
	dir := filepath.Join(root, "store")
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	c := &clock{now.Add(-20 * 24 * time.Hour)}
	s, err := Open(ctx, dir, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO daemons (id, name) VALUES (1, 'd')"); err != nil {
		t.Fatal(err)
	}
	// Builds of 40 days ago, summed up by an earlier rollup.
	for b := 1; b <= 3000; b++ {
		ts := now.Add(-40 * 24 * time.Hour).Add(time.Duration(b) * time.Second).UnixMilli()
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec("INSERT INTO builds (id, daemon_id, ref, generation, attribution, instance, project_id, target, started_at, completed_at) VALUES (?, 1, ?, 1, 1, 1, 42, 'api', ?, ?)",
			b, fmt.Sprint("r", b), ts, ts+1000)
		for i := 0; err == nil && i < 10; i++ {
			_, err = tx.Exec("INSERT INTO steps (build_id, idx, key, name, state, start_ms, dur_ms) VALUES (?, ?, 'k', ?, 1, 0, 1000)", b, i, strings.Repeat("RUN step text ", 8))
		}
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		if err != nil {
			t.Fatalf("build %d: %v", b, err)
		}
	}
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	c.t = now
	old := filepath.Join(dir, outputName(day(now.Add(-10*24*time.Hour))))
	if err := os.WriteFile(old, make([]byte, 64<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	fill := func() {
		t.Helper()
		f, err := os.OpenFile(filepath.Join(root, fmt.Sprintf("filler-%d", time.Now().UnixNano())), os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		buf := make([]byte, 4<<10)
		for {
			if _, err := f.Write(buf); err != nil {
				return
			}
		}
	}
	fill()
	if err := s.SaveJob(ctx, Job{Instance: "000001", JobID: 9, PipelineID: 9, ProjectID: 42, ProjectPath: "g/p", Name: "b"}); err == nil {
		t.Log("a small write still fits")
	}
	n, err := s.Prune(ctx)
	t.Logf("Prune on the full volume: %d deleted, %v", n, err)
	if n == 0 {
		t.Errorf("no build deleted on a full volume")
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the output file past retention: %v", err)
	}
	if err := s.LowSpace(); err == nil || !strings.Contains(err.Error(), "nearly full") {
		t.Errorf("LowSpace does not say the volume is nearly full: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Logf("Close: %v", err)
	}
	fill()
	s, err = Open(ctx, dir, Options{Now: c.now})
	if err != nil {
		t.Fatalf("Open on the full volume: %v", err)
	}
	s.Close()
}

// The pipelines page's bounds: Until is not included, and a limit over
// the most a page holds takes the default.
func TestPipelineListBounds(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	s := open(t, t.TempDir(), &clock{now})
	for i := 0; i < 25; i++ {
		created := now.Add(-time.Duration(i) * time.Hour)
		if err := s.SaveJob(ctx, Job{Instance: "000001", JobID: uint64(100 + i), PipelineID: uint64(10 + i), ProjectID: 42, ProjectPath: "g/p", Name: "build", PipelineCreatedAt: created}); err != nil {
			t.Fatal(err)
		}
	}
	p := ProjectKey{Instance: "000001", ID: 42}
	rows, err := s.PipelineList(ctx, PipeQuery{Project: p, Limit: 1000})
	if err != nil || len(rows) != 20 {
		t.Errorf("a limit of 1000: %d rows, %v; want the default 20", len(rows), err)
	}
	rows, err = s.PipelineList(ctx, PipeQuery{Project: p, Since: now.Add(-2 * time.Hour), Until: now})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, fmt.Sprint(r.ID))
	}
	if strings.Join(ids, ",") != "11,12" {
		t.Errorf("since 2 h ago until now: %v, want 11,12 (Since included, Until not)", ids)
	}
}

// Retention's delete reads the builds by their start through its index,
// not by a scan of every build.
func TestPruneUsesStartIndex(t *testing.T) {
	s := open(t, t.TempDir(), &clock{time.Now()})
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+pruneBuildsQuery, 1, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if got := strings.Join(plan, " | "); !strings.Contains(got, "builds_started") {
		t.Errorf("plan: %s", got)
	}
}
