package store

import (
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/cacheuse"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// records makes the disk usage a daemon would list after builds: one
// layer per step each build executed, created as the step completed.
func records(bs []*history.Build) []cacheuse.Record {
	var out []cacheuse.Record
	for _, b := range bs {
		for i, st := range b.Steps {
			if st.State != history.Built {
				continue
			}
			d := cacheuse.Describe(st.Key, st.Name)
			if d == "" {
				continue
			}
			out = append(out, cacheuse.Record{ID: b.Ref[:6] + "-" + string(rune('a'+i)), Description: d, Type: "regular",
				Size: 1000, Created: st.Completed})
		}
	}
	return out
}

func TestCacheSnapshot(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	// The records exist before the builds are stored (a listing during
	// the builds): nobody owns them yet.
	recs := records(bs)
	if len(recs) < 5 {
		t.Fatalf("only %d layers", len(recs))
	}
	c.t = recs[0].Created.Add(30 * time.Minute)
	pool := []cacheuse.Record{
		{ID: "base1", Description: "pulled from docker.io/library/python:3.13-slim@sha256:x", Type: "regular", Size: 50000, Created: c.t},
		{ID: "ctx1", Description: "local source for context", Type: "source.local", Size: 300, Created: c.t, InUse: true},
		{ID: "mnt1", Description: "cached mount /root/.cache/pip from exec /bin/sh -c pip install with id \"//root/.cache/pip\"", Type: "exec.cachemount", Size: 7000, Created: c.t},
		{ID: "stray", Description: "mount / from exec /bin/sh -c nothing ran this", Type: "regular", Size: 11, Created: c.t},
	}
	all := append(append([]cacheuse.Record(nil), recs...), pool...)
	if err := s.CacheSnapshot(ctx, "node-1", all); err != nil {
		t.Fatal(err)
	}
	u, err := s.CacheUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := u["node-1"]; len(n.Projects) != 0 || n.Unknown != int64(1000*len(recs)+11) || n.Base != 50000 || n.Contexts != 300 || n.Mounts != 7000 {
		t.Fatalf("before the builds: %+v", n)
	}
	// The builds are stored; the daemon's node is known.
	for _, b := range bs {
		if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetDaemonNode(ctx, "daemon-a", "node-1"); err != nil {
		t.Fatal(err)
	}
	// Tried again a minute later, not at once.
	if err := s.CacheSnapshot(ctx, "node-1", all); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.CacheUsage(ctx); len(u["node-1"].Projects) != 0 {
		t.Fatalf("tried again within a minute: %+v", u["node-1"])
	}
	c.t = c.t.Add(2 * time.Minute)
	if err := s.CacheSnapshot(ctx, "node-1", all); err != nil {
		t.Fatal(err)
	}
	u, _ = s.CacheUsage(ctx)
	n := u["node-1"]
	if len(n.Projects) != 1 || n.Projects[0].Project != 42 || n.Projects[0].Bytes != int64(1000*len(recs)) || n.Unknown != 11 {
		t.Fatalf("after the builds: %+v", n)
	}
	if got := n.Attributed(); got < 0.99 || got >= 1 {
		t.Errorf("attributed %.4f", got)
	}
	key := ProjectKey{Instance: bs[0].Attribution.Instance, ID: 42}
	ids, bytes, err := s.ProjectRecords(ctx, "node-1", key)
	if err != nil || len(ids) != len(recs) || bytes != int64(1000*len(recs)) {
		t.Fatalf("project records %d, %d bytes, %v", len(ids), bytes, err)
	}

	// The next listing: a layer gone, another grown and in use, a node
	// that is another's.
	next := append([]cacheuse.Record(nil), all[1:]...)
	next[0].Size, next[0].InUse = 5000, true
	if err := s.CacheSnapshot(ctx, "node-1", next); err != nil {
		t.Fatal(err)
	}
	if err := s.CacheSnapshot(ctx, "node-2", pool[:1]); err != nil {
		t.Fatal(err)
	}
	u, _ = s.CacheUsage(ctx)
	if n := u["node-1"]; n.Projects[0].Bytes != int64(1000*(len(recs)-2)+5000) || u["node-2"].Base != 50000 || u["node-2"].Total != 50000 {
		t.Fatalf("after the next listing: %+v, %+v", n, u["node-2"])
	}
	// In use: never removed.
	if ids, _, _ := s.ProjectRecords(ctx, "node-1", key); len(ids) != len(recs)-2 {
		t.Errorf("records to remove: %d, want %d (one gone, one in use)", len(ids), len(recs)-2)
	}
	// A layer's owner stays when its build is past retention.
	c.t = c.t.Add(s.opts.Keep + 24*time.Hour)
	if _, err := s.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.CacheSnapshot(ctx, "node-1", next); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.CacheUsage(ctx); len(u["node-1"].Projects) != 1 {
		t.Errorf("owner lost with the builds: %+v", u["node-1"])
	}
	if err := s.SaveCacheAction(ctx, CacheAction{At: c.t, Who: "alice", Node: "node-1", Project: key, Records: 3, Bytes: 3000}); err != nil {
		t.Fatal(err)
	}
	if as, err := s.CacheActions(ctx, 10); err != nil || len(as) != 1 || as[0].Who != "alice" || as[0].Project != key {
		t.Errorf("actions %+v %v", as, err)
	}
}
