package store

import (
	"errors"
	"testing"
	"time"
)

func TestCacheScans(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 22, 20, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	inst := "051b90"
	p := ProjectKey{Instance: inst, ID: 42}
	yes, no := true, false
	t0 := c.t.Add(-time.Hour)
	objs := []CacheObject{{ProjectID: 42, Key: "go-mod-main-protected", Bytes: 67128, Modified: t0}, {ProjectID: 42, Key: "tools", Bytes: 353, Modified: t0}, {ProjectID: 7, Key: "x", Bytes: 1, Modified: t0}}
	if err := s.ReplaceCacheScan(ctx, "s3.example.com/ci-cache/box/051b90", inst, c.t, objs, &no, nil); err != nil {
		t.Fatal(err)
	}
	got, stores, err := s.ProjectCache(ctx, p)
	if err != nil || len(got) != 2 || got[0].Key != "go-mod-main-protected" || got[0].Bytes != 67128 || !got[0].Modified.Equal(t0) {
		t.Fatalf("project cache %+v, %v", got, err)
	}
	if len(stores) != 1 || stores[0].Lifecycle == nil || *stores[0].Lifecycle || !stores[0].Scanned.Equal(c.t) || stores[0].Err != "" {
		t.Fatalf("stores %+v", stores)
	}
	// A failed listing keeps what was known and says why.
	if err := s.ReplaceCacheScan(ctx, "s3.example.com/ci-cache/box/051b90", inst, c.t.Add(time.Minute), nil, nil, errors.New("listing: 403")); err != nil {
		t.Fatal(err)
	}
	got, stores, _ = s.ProjectCache(ctx, p)
	if len(got) != 2 || stores[0].Err != "listing: 403" || stores[0].Lifecycle == nil {
		t.Errorf("after a failed listing: %+v %+v", got, stores)
	}
	// A new listing replaces the old one.
	if err := s.ReplaceCacheScan(ctx, "s3.example.com/ci-cache/box/051b90", inst, c.t.Add(2*time.Minute), objs[1:2], &yes, nil); err != nil {
		t.Fatal(err)
	}
	got, stores, _ = s.ProjectCache(ctx, p)
	if len(got) != 1 || got[0].Key != "tools" || !*stores[0].Lifecycle || stores[0].Err != "" {
		t.Errorf("after a new listing: %+v %+v", got, stores)
	}

}

// Runners with Shared = false keep a project's cache key once per runner
// (runner/<token>/project/...): every archive takes room, so one key
// counts all of them.
func TestCacheKeyOfSeveralRunners(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 22, 20, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	p := ProjectKey{Instance: "051b90", ID: 42}
	t0, t1 := c.t.Add(-2*time.Hour), c.t.Add(-time.Hour)
	objs := []CacheObject{{ProjectID: 42, Key: "default-protected", Bytes: 850, Modified: t1}, {ProjectID: 42, Key: "default-protected", Bytes: 900, Modified: t0}}
	if err := s.ReplaceCacheScan(ctx, "s3/b/x", p.Instance, c.t, objs, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.ProjectCache(ctx, p)
	if err != nil || len(got) != 1 || got[0].Bytes != 1750 || !got[0].Modified.Equal(t1) {
		t.Errorf("one key of two runners: %+v, %v", got, err)
	}
}

// A folder no longer configured is forgotten with its archives.
func TestKeepCacheStores(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 22, 20, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	p := ProjectKey{Instance: "051b90", ID: 42}
	for _, id := range []string{"s3/a/x", "s3/b/y"} {
		if err := s.ReplaceCacheScan(ctx, id, p.Instance, c.t, []CacheObject{{ProjectID: 42, Key: "k-" + id, Bytes: 1}}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.KeepCacheStores(ctx, []string{"s3/b/y"}); err != nil {
		t.Fatal(err)
	}
	objs, stores, err := s.ProjectCache(ctx, p)
	if err != nil || len(objs) != 1 || objs[0].Store != "s3/b/y" || len(stores) != 1 || stores[0].ID != "s3/b/y" {
		t.Errorf("after forgetting s3/a/x: %+v %+v %v", objs, stores, err)
	}
	if err := s.KeepCacheStores(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if objs, stores, _ := s.ProjectCache(ctx, p); len(objs)+len(stores) != 0 {
		t.Errorf("with no store configured: %+v %+v", objs, stores)
	}
}
