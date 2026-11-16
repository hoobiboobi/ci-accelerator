package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// A name that holds only a join token (issued through the API, settings
// not set yet) is among the clusters waiting to join on Members.
func TestMembersListsJoinTokenOnly(t *testing.T) {
	ctx := context.Background()
	ts, reg := hubAdmin(t, true)
	if _, _, err := reg.IssueJoin(ctx, "token:tf", "api-only", time.Hour); err != nil {
		t.Fatal(err)
	}
	m := as(t, ts, "operator", "GET", "/", nil)
	for _, want := range []string{"Waiting to join", `href="/clusters/api-only"`} {
		if !strings.Contains(m.body, want) {
			t.Errorf("Members: no %q", want)
		}
	}
}

// A build's page at a hub names no JSON API: the hub does not serve the
// clusters' /api/v1. A box's own page still names it.
func TestHubBuildPageNamesNoAPI(t *testing.T) {
	under := func(h http.Handler) *httptest.Server {
		mux := http.NewServeMux()
		mux.Handle("/m/eu/", http.StripPrefix("/m/eu", h))
		return httptest.NewServer(mux)
	}
	ts, _, ids, _ := consoleBuild(t, func(*store.Store) testOpts { return testOpts{Base: "/m/eu", Fleet: fakeFleet{current: "eu"}} }, under)
	var build int64
	for _, id := range ids {
		build = id
	}
	pg := get(t, ts, "/m/eu/builds/"+strconv.FormatInt(build, 10))
	if pg.status != 200 || strings.Contains(pg.body, "/api/v1/builds?limit") {
		t.Errorf("hub build page: %d, names the API: %v", pg.status, strings.Contains(pg.body, "/api/v1/builds?limit"))
	}
	box, _, ids := console(t)
	for _, id := range ids {
		build = id
	}
	if pg := get(t, box, "/builds/"+strconv.FormatInt(build, 10)); !strings.Contains(pg.body, "/api/v1/builds?limit") {
		t.Errorf("box build page: %d, the API not named", pg.status)
	}
}

// The built-in pages are parsed once and shared by every Server (a hub
// makes one per cluster); each still writes its own base. At a hub a
// cluster's page takes the static files from the root: one cached copy.
func TestPagesParsedOnce(t *testing.T) {
	a, err := New(Empty{}, testLogger(), Options{Base: "/m/eu", Fleet: fakeFleet{current: "eu"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(Empty{}, testLogger(), Options{Base: "/m/us", Fleet: fakeFleet{current: "us"}})
	if err != nil {
		t.Fatal(err)
	}
	if a.tmpl["builds"] == nil || a.tmpl["builds"] != b.tmpl["builds"] {
		t.Error("each Server parsed the pages again")
	}
	under := func(h http.Handler) *httptest.Server {
		mux := http.NewServeMux()
		mux.Handle("/m/eu/", http.StripPrefix("/m/eu", h))
		return httptest.NewServer(mux)
	}
	ts, _, _, _ := consoleBuild(t, func(*store.Store) testOpts { return testOpts{Base: "/m/eu", Fleet: fakeFleet{current: "eu"}} }, under)
	pg := get(t, ts, "/m/eu/builds")
	for _, want := range []string{`data-base="/m/eu"`, `href="/static/console.css?v=`, `src="/static/console.js?v=`} {
		if !strings.Contains(pg.body, want) {
			t.Errorf("a cluster's page at the hub: no %q", want)
		}
	}
	if strings.Contains(pg.body, "/m/eu/static/") {
		t.Error("a cluster's page at the hub takes its static files under its base")
	}
}
