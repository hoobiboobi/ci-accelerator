package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/route"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// roles lets "web" and "router" tokens through in their roles.
func roles(r *http.Request) (Role, error) {
	switch bearer(r) {
	case "web":
		return RoleWeb, nil
	case "router":
		return RoleRouter, nil
	}
	return "", ErrUnauthenticated
}

func TestRoutesForRoutersOnly(t *testing.T) {
	st := newStore(t)
	live := daemons{
		{Name: "bk-1", Connected: true, Inspection: collector.Inspection{Node: "n1"}, Active: []collector.Active{{Ref: "a"}, {Ref: "b"}}},
		{Name: "bk-2", Connected: false, Inspection: collector.Inspection{Node: "n2"}, Active: []collector.Active{{Ref: "c"}}},
	}
	now := time.Now()
	rt := &Routes{Store: st, Live: live, Every: time.Hour, Now: func() time.Time { return now }}
	srv := &Server{Local: &Local{Store: st}, Authorize: roles, Routes: rt}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	status := func(method, path, token string) int {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader("[]"))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for _, c := range []struct {
		method, path, token string
		want                int
	}{
		{"GET", "/internal/v1/route/warm", "", 401},
		{"GET", "/internal/v1/route/warm", "web", 403},
		{"POST", "/internal/v1/route/decisions", "web", 403},
		{"POST", "/internal/v1/call/Projects", "router", 403},
		{"GET", "/internal/v1/events", "router", 403},
		{"GET", "/internal/v1/config", "router", 403},
		{"GET", "/internal/v1/route/warm", "router", 200},
		{"POST", "/internal/v1/route/decisions", "router", 204},
	} {
		if got := status(c.method, c.path, c.token); got != c.want {
			t.Errorf("%s %s as %q: %d, want %d", c.method, c.path, c.token, got, c.want)
		}
	}

	rc := &RouterClient{URL: ts.URL, Token: func() (string, error) { return "router", nil }}
	w, changed, err := rc.Warm(ctx)
	if err != nil || !changed || len(w.Projects) != 0 || w.Active["n1"] != 2 || w.Active["n2"] != 0 || w.Version == "" {
		t.Fatalf("first view %+v changed=%v %v", w, changed, err)
	}
	if _, changed, err := rc.Warm(ctx); err != nil || changed {
		t.Fatalf("the same view again: changed=%v %v", changed, err)
	}
	p := route.Project{Instance: "0a1b2c", ID: 7}
	if err := rc.Report(ctx, []store.Decision{{Project: p, JobID: 5, Node: "n1", Daemon: "bk-1", Reason: route.ZoneHash, At: now.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	w, changed, err = rc.Warm(ctx)
	if err != nil || !changed || len(w.Projects) != 1 || w.Projects[0].Project != p || w.Projects[0].Activity[0].Node != "n1" {
		t.Fatalf("after a decision %+v changed=%v %v", w, changed, err)
	}
	// A router's clock ahead of the controller's does not keep a node warm
	// into the future.
	if at := w.Projects[0].Activity[0].At; at.After(now) {
		t.Errorf("decision dated %s, after the controller's now %s", at, now)
	}
	got, err := st.JobRoutes(ctx, "0a1b2c", 5)
	if err != nil || len(got) != 1 || got[0].Daemon != "bk-1" {
		t.Fatalf("stored %+v %v", got, err)
	}
	for _, bad := range []string{`[{"node":"n1","daemon":"bk"}]`, `{"not":"a list"}`} {
		req, _ := http.NewRequest("POST", ts.URL+"/internal/v1/route/decisions", strings.NewReader(bad))
		req.Header.Set("Authorization", "Bearer router")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Errorf("%s: %d", bad, res.StatusCode)
		}
	}
	// The controller away: the last view stays.
	ts.Close()
	w2, changed, err := rc.Warm(ctx)
	if err == nil || changed || w2.Version != w.Version {
		t.Errorf("controller gone: %+v changed=%v %v", w2.Version, changed, err)
	}
	if err := rc.Report(ctx, nil); err == nil {
		t.Error("a report reached a controller that is gone")
	}
}
