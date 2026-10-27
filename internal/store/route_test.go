package store

import (
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/route"
)

// A store of the previous schema opens with its builds and gains the
// router's tables.
func TestMigrationKeepsBuilds(t *testing.T) {
	dir := t.TempDir()
	all := migrations
	migrations = all[:1]
	c := &clock{time.Now()}
	s, err := Open(ctx, dir, Options{Now: c.now})
	if err != nil {
		migrations = all
		t.Fatal(err)
	}
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	for _, b := range bs {
		if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1}); err != nil {
			migrations = all
			t.Fatal(err)
		}
	}
	before, err := s.Recent(ctx, 100)
	s.Close()
	migrations = all
	if err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, c)
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("schema %d (%v), want %d", version, err, len(migrations))
	}
	after, err := s.Recent(ctx, 100)
	if err != nil || len(after) != len(before) || len(after) == 0 {
		t.Fatalf("%d builds before, %d after (%v)", len(before), len(after), err)
	}
	if err := s.SetDaemonNode(ctx, "daemon-a", "node-1"); err != nil {
		t.Fatal(err)
	}
	w, err := s.Warm(ctx, c.t.Add(-365*24*time.Hour))
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

	// The daemon's node is not known yet: no warmth.
	if w, err := s.Warm(ctx, since); err != nil || len(w) != 0 {
		t.Fatalf("without nodes: %v, %v", w, err)
	}
	if err := s.SetDaemonNode(ctx, "daemon-a", "node-1"); err != nil {
		t.Fatal(err)
	}
	w, err := s.Warm(ctx, since)
	p := route.Project{Instance: inst, ID: 42}
	if err != nil || len(w[p]) != 1 || w[p][0].Node != "node-1" || !w[p][0].At.Equal(last.Started.Truncate(time.Millisecond)) {
		t.Fatalf("warmth %v, %v", w, err)
	}
	// A restarted daemon (new pod) on the same node: one more row, same node.
	if err := s.SetDaemonNode(ctx, "daemon-b", "node-1"); err != nil {
		t.Fatal(err)
	}

	// A decision warms its node at once, before a build is stored there.
	q := route.Project{Instance: inst, ID: 43}
	d := Decision{Project: q, JobID: 900, Node: "node-2", Daemon: "bk-x", Reason: route.ZoneHash, Cands: 3, JobNode: "node-1", At: c.t.Add(-time.Minute)}
	old := d
	old.JobID, old.At = 901, c.t.Add(-2*time.Hour)
	if err := s.SaveDecisions(ctx, []Decision{d, old}); err != nil {
		t.Fatal(err)
	}
	w, err = s.Warm(ctx, since)
	if err != nil || len(w[q]) != 1 || w[q][0].Node != "node-2" || !w[q][0].At.Equal(d.At.Truncate(time.Millisecond)) {
		t.Fatalf("decision warmth %v, %v", w[q], err)
	}
	got, err := s.JobRoutes(ctx, inst, 900)
	if err != nil || len(got) != 1 || got[0].Daemon != "bk-x" || got[0].Reason != route.ZoneHash || got[0].Cands != 3 || got[0].JobNode != "node-1" || got[0].Project != q {
		t.Fatalf("job routes %+v, %v", got, err)
	}
	nodes, err := s.DaemonNodes(ctx)
	if err != nil || nodes["daemon-a"] != "node-1" || nodes["daemon-b"] != "node-1" {
		t.Fatalf("nodes %v, %v", nodes, err)
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
