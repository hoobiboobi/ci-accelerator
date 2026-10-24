package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

type fakeStore struct {
	builds []*store.Stored
	down   bool
	limit  int
}

func (f *fakeStore) Recent(_ context.Context, limit int) ([]*store.Stored, error) {
	f.limit = limit
	return f.builds, nil
}

func (f *fakeStore) Ping(context.Context) error {
	if f.down {
		return errors.New("closed")
	}
	return nil
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func TestBuilds(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	b := &history.Build{Ref: "r1", Daemon: "d1", Target: "api", Started: t0, Completed: t0.Add(time.Minute),
		Attribution: history.Attribution{State: history.Attributed, Instance: "abcdef", Pipeline: 7, Job: 70},
		Steps: []history.Step{{Key: "[a 1/3]", Name: "FROM alpine:3.22", State: history.Built},
			{Key: "[a 2/3]", State: history.Cached}, {Key: "[a 3/3]", State: history.Built}}}
	f := &fakeStore{builds: []*store.Stored{{ID: 1, Build: b, Reason: history.Reason{Kind: history.FilesChanged, Step: b.Steps[2], Paths: []string{"api"}}}}}
	h := Handler(f)
	w := get(t, h, "/api/v1/builds?limit=5")
	if w.Code != 200 || f.limit != 5 {
		t.Fatalf("status %d, limit %d", w.Code, f.limit)
	}
	var out struct{ Builds []Build }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	got := out.Builds[0]
	// The base-image step is "image", as on the pages, not a step that ran.
	if got.Job != 70 || got.Reason == nil || got.Reason.Kind != history.FilesChanged || got.Reason.Step != "[a 3/3]" ||
		got.Steps["cached"] != 1 || got.Steps["built"] != 1 || got.Steps["image"] != 1 || got.Completed == nil {
		t.Errorf("%+v", got)
	}
	for _, bad := range []string{"0", "1001", "x"} {
		if w := get(t, h, "/api/v1/builds?limit="+bad); w.Code != http.StatusBadRequest {
			t.Errorf("limit=%s: status %d", bad, w.Code)
		}
	}
	if w := httptest.NewRecorder(); true {
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/builds", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST: status %d", w.Code)
		}
	}
}

func TestHealth(t *testing.T) {
	f := &fakeStore{}
	if w := get(t, Handler(f), "/healthz"); w.Code != 200 {
		t.Errorf("healthy: %d", w.Code)
	}
	f.down = true
	if w := get(t, Handler(f), "/healthz"); w.Code != http.StatusServiceUnavailable || w.Body.String() != "store unavailable\n" {
		t.Errorf("store down: %d %q (the cause belongs in the log)", w.Code, w.Body.String())
	}
}
