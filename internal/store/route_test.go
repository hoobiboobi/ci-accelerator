package store

import (
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/route"
)

// A store written by 0.2.x (schema 1) opens with its builds and gains
// the router's tables and the builds' warnings.
func TestMigrationKeepsBuilds(t *testing.T) {
	dir := t.TempDir()
	all := migrations
	migrations = all[:1]
	c := &clock{time.Now()}
	s, err := Open(ctx, dir, Options{Now: c.now})
	migrations = all
	if err != nil {
		t.Fatal(err)
	}
	// Rows as 0.2.x wrote them: only schema-1 columns.
	for _, q := range []string{
		"INSERT INTO daemons (id, name) VALUES (1, 'daemon-a')",
		"INSERT INTO projects (instance, project_id, path) VALUES (1, 42, 'example/app')",
		`INSERT INTO builds (daemon_id, ref, generation, attribution, instance, pipeline_id, job_id, project_id, target, started_at, completed_at)
			VALUES (1, 'ref-1', 1, 1, 1, 7, 8, 42, 'api', 1000, 2000)`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s = open(t, dir, c)
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("schema %d (%v), want %d", version, err, len(migrations))
	}
	after, err := s.Recent(ctx, 100)
	if err != nil || len(after) != 1 || after[0].Build.Ref != "ref-1" || len(after[0].Build.Warnings) != 0 {
		t.Fatalf("builds after the migration: %+v (%v)", after, err)
	}
	if err := s.SetDaemonNode(ctx, "daemon-a", "node-1", false); err != nil {
		t.Fatal(err)
	}
	w, err := s.Warm(ctx, time.UnixMilli(0), nil)
	if err != nil || len(w) != 1 {
		t.Fatalf("warmth %v, %v", w, err)
	}
}

func TestWarmAndDecisions(t *testing.T) {
	c := &clock{time.Now()}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	for _, b := range bs {
		if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	last := bs[len(bs)-1]
	inst := last.Attribution.Instance
	c.t = last.Started.Add(time.Hour)
	since := last.Started.Add(-24 * time.Hour)
	p := route.Project{Instance: inst, ID: 42}

	// The daemon's node is not known yet: no warmth.
	if w, err := s.Warm(ctx, since, nil); err != nil || len(w) != 0 {
		t.Fatalf("without nodes: %v, %v", w, err)
	}
	if err := s.SetDaemonNode(ctx, "daemon-a", "node-1", false); err != nil {
		t.Fatal(err)
	}
	w, err := s.Warm(ctx, since, nil)
	if err != nil || len(w[p]) != 1 || w[p][0].Node != "node-1" || !w[p][0].At.Equal(last.Started.Truncate(time.Millisecond)) {
		t.Fatalf("warmth %v, %v", w, err)
	}
	// The daemon was replaced (a new pod): its cache went with it unless
	// it persists on the node, as the daemon's worker label says.
	if w, _ := s.Warm(ctx, since, []string{"daemon-b"}); len(w[p]) != 0 {
		t.Errorf("an emptied cache still warm: %v", w[p])
	}
	if w, _ := s.Warm(ctx, since, []string{"daemon-a"}); len(w[p]) != 1 {
		t.Errorf("the live daemon's builds: %v", w[p])
	}
	if err := s.SetDaemonNode(ctx, "daemon-a", "node-1", true); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.Warm(ctx, since, []string{"daemon-b"}); len(w[p]) != 1 {
		t.Errorf("a cache on the node forgotten with its pod: %v", w[p])
	}
	// Warmth expires with the window.
	if w, _ := s.Warm(ctx, last.Started.Add(time.Minute), nil); len(w[p]) != 0 {
		t.Errorf("builds before the window still warm: %v", w[p])
	}

	// A decision warms its node at once, while its daemon runs.
	q := route.Project{Instance: inst, ID: 43}
	d := Decision{Project: q, JobID: 900, Node: "node-2", Daemon: "bk-x", Reason: route.ZoneHash, Cands: 3, JobNode: "node-1", At: c.t.Add(-time.Minute)}
	old := d
	old.JobID, old.At = 901, c.t.Add(-2*time.Hour)
	if err := s.SaveDecisions(ctx, []Decision{d, old}); err != nil {
		t.Fatal(err)
	}
	w, err = s.Warm(ctx, since, []string{"bk-x"})
	if err != nil || len(w[q]) != 1 || w[q][0].Node != "node-2" || !w[q][0].At.Equal(d.At.Truncate(time.Millisecond)) {
		t.Fatalf("decision warmth %v, %v", w[q], err)
	}
	if w, _ := s.Warm(ctx, since, []string{"bk-y"}); len(w[q]) != 0 {
		t.Errorf("a decision for a daemon replaced since still warm: %v", w[q])
	}
	got, err := s.JobRoutes(ctx, inst, 900)
	if err != nil || len(got) != 1 || got[0].Daemon != "bk-x" || got[0].Reason != route.ZoneHash || got[0].Cands != 3 || got[0].JobNode != "node-1" || got[0].Project != q {
		t.Fatalf("job routes %+v, %v", got, err)
	}

	// The Nodes page counts them per daemon and reason, within its window.
	counts, err := s.DaemonCounts(ctx, c.t.Add(-time.Hour), ProjectsIn{All: true})
	if err != nil || counts["bk-x"].Routed[string(route.ZoneHash)] != 1 || len(counts["bk-x"].Routed) != 1 {
		t.Errorf("routed in the last hour: %+v, %v", counts["bk-x"], err)
	}
	if counts, _ := s.DaemonCounts(ctx, time.Time{}, ProjectsIn{All: true}); counts["bk-x"].Routed[string(route.ZoneHash)] != 2 {
		t.Errorf("routed ever: %+v", counts["bk-x"])
	}
	// The projects each daemon built for, counted.
	all, _ := s.DaemonCounts(ctx, time.Time{}, ProjectsIn{All: true})
	n := 0
	for _, pb := range all["daemon-a"].Projects {
		n += pb.Builds
	}
	if len(all["daemon-a"].Projects) == 0 || n > all["daemon-a"].Builds {
		t.Errorf("projects of daemon-a: %+v (builds %d)", all["daemon-a"].Projects, all["daemon-a"].Builds)
	}

	// Decisions go with the builds' retention.
	c.t = c.t.Add(s.opts.Keep + time.Hour)
	if _, err := s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.JobRoutes(ctx, inst, 900); len(got) != 0 {
		t.Errorf("decisions kept past retention: %+v", got)
	}
}

func TestPercentileIsANearestRank(t *testing.T) {
	s := func(xs ...int) []time.Duration {
		var out []time.Duration
		for _, x := range xs {
			out = append(out, time.Duration(x)*time.Second)
		}
		return out
	}
	in := s(9, 1, 5, 3)
	for _, c := range []struct {
		ds   []time.Duration
		p    int
		want int
	}{{nil, 50, 0}, {s(4), 50, 4}, {s(4), 95, 4}, {in, 50, 3}, {s(11, 6, 5), 50, 6}, {in, 95, 9}, {in, 25, 1}, {s(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20), 95, 19}} {
		if got := Percentile(c.ds, c.p); got != time.Duration(c.want)*time.Second {
			t.Errorf("p%d of %v = %v, want %ds", c.p, c.ds, got, c.want)
		}
	}
	if in[0] != 9*time.Second {
		t.Error("the caller's slice was sorted")
	}
}

// A job's pod times arrive in two updates (created, then running) and
// neither erases the other.
func TestJobPodTimes(t *testing.T) {
	s := open(t, t.TempDir(), &clock{time.Now()})
	created := time.Date(2026, 10, 28, 1, 3, 36, 0, time.UTC)
	j := Job{Instance: "abcdef", JobID: 5, PipelineID: 7, ProjectID: 42, Name: "build", PodCreated: created}
	if err := s.SaveJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	j.PodRunning = created.Add(13 * time.Second)
	if err := s.SaveJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	j.PodCreated, j.PodRunning = time.Time{}, time.Time{}
	if err := s.SaveJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	got, err := s.Job(ctx, "abcdef", 5)
	if err != nil || got == nil || !got.PodCreated.Equal(created) || !got.PodRunning.Equal(created.Add(13*time.Second)) {
		t.Fatalf("pod times %+v, %v", got, err)
	}
}

// GitLab's comparisons are kept, looked up by the two commits, and go with
// retention.
func TestCompares(t *testing.T) {
	c := &clock{time.Now()}
	s := open(t, t.TempDir(), c)
	p := ProjectKey{Instance: "abcdef", ID: 42}
	if _, _, ok, err := s.Compared(ctx, p, "a", "b"); ok || err != nil {
		t.Fatalf("before: %v %v", ok, err)
	}
	if err := s.SaveCompare(ctx, p, "a", "b", []string{"api/main.go"}, true); err != nil {
		t.Fatal(err)
	}
	files, complete, ok, err := s.Compared(ctx, p, "a", "b")
	if !ok || err != nil || !complete || len(files) != 1 || files[0] != "api/main.go" {
		t.Fatalf("after: %v %v %v %v", files, complete, ok, err)
	}
	if _, _, ok, _ := s.Compared(ctx, p, "b", "a"); ok {
		t.Error("the reverse comparison found")
	}
	c.t = c.t.Add(s.opts.Keep + time.Hour)
	if _, err := s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.Compared(ctx, p, "a", "b"); ok {
		t.Error("kept past retention")
	}
}

// An answer is stored only on the question it answers: a build saved
// again since with other paths or another previous daemon stays open.
func TestSettleOnlyTheQuestionAsked(t *testing.T) {
	c := &clock{time.Now()}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	b := bs[len(bs)-1]
	var step history.Step
	for _, st := range b.Steps {
		if st.State == history.Built && st.StepID != "" {
			step = st
		}
	}
	id, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1, Reason: history.Reason{Kind: history.CacheElsewhere, Step: step, Old: "bk-x", Paths: []string{"api"}}})
	if err != nil {
		t.Fatal(err)
	}
	stale := Settled{ID: id, Kind: history.FilesChanged, Paths: []string{"web/x"}, Via: "gitlab a..b", Asked: true, Old: "bk-x", Open: []string{"web"}}
	if err := s.SettleElsewhere(ctx, []Settled{stale}); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.BuildPage(ctx, id); p.Via != "" {
		t.Errorf("an answer to another question stored: %q", p.Via)
	}
	right := stale
	right.Open, right.Paths = []string{"api"}, []string{"api/main.go"}
	if err := s.SettleElsewhere(ctx, []Settled{right}); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.BuildPage(ctx, id); p.Via != "gitlab a..b" || p.Reason.Kind != history.FilesChanged {
		t.Errorf("the answer to its question not stored: %q %s", p.Via, p.Reason.Kind)
	}
}
