package web

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// The Nodes page of a project shows the runner cache it has in S3: its
// size and keys, and warnings about the folders that hold it. A folder
// that holds none of its caches (another runner tier's) never warns on its
// page; with nothing stored anywhere, every folder's state is shown.
func TestProjectBucket(t *testing.T) {
	ts, st, _, srv := consoleServer(t)
	inst := history.InstancePrefix("https://gitlab.example.com/")
	at := time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)
	no, yes := false, true
	view := func(id uint64) *BucketView {
		t.Helper()
		v, err := srv.bucket(ctx, store.ProjectKey{Instance: inst, ID: id}, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	if v := view(42); v != nil {
		t.Errorf("nothing listed: %+v", v)
	}
	if p := get(t, ts, "/nodes?project="+inst+"/42"); !strings.Contains(p.body, "No S3 cache is listed for the runners.") {
		t.Error("nothing listed: the page's text is missing")
	}

	// Tier "big" holds project 42's caches and has a lifecycle rule; tier
	// "small" holds project 43's, has none, and its last listing failed.
	objs := []store.CacheObject{
		{ProjectID: 42, Key: "deps-1", Bytes: 3 << 20, Modified: at},
		{ProjectID: 42, Key: "deps-2", Bytes: 1 << 20, Modified: at},
	}
	if err := st.ReplaceCacheScan(ctx, "big", inst, at, objs, &yes, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceCacheScan(ctx, "small", inst, at, []store.CacheObject{{ProjectID: 43, Key: "x", Bytes: 1, Modified: at}}, &no, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceCacheScan(ctx, "small", inst, at.Add(time.Minute), nil, nil, errors.New("AccessDenied: listing small/")); err != nil {
		t.Fatal(err)
	}

	if v := view(42); v == nil || v.Keys != 2 || v.Bytes != 4<<20 || v.NoRule || v.Err != "" {
		t.Errorf("project 42 (its folder fine, another one not): %+v", v)
	}
	if v := view(43); v == nil || v.Keys != 1 || !v.NoRule || !strings.Contains(v.Err, "AccessDenied") {
		t.Errorf("project 43 (its folder warns): %+v", v)
	}
	if v := view(44); v == nil || v.Keys != 0 || !v.NoRule || !strings.Contains(v.Err, "AccessDenied") {
		t.Errorf("project 44 (nothing stored: every folder's state): %+v", v)
	}
	p := get(t, ts, "/nodes?project="+inst+"/42")
	if !strings.Contains(p.body, "in 2 keys") || strings.Contains(p.body, "Old caches are never deleted") || strings.Contains(p.body, "AccessDenied") {
		t.Error("project 42's page: its keys, and no other folder's warnings")
	}
}
