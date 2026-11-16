package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// fakeFleet is a hub's fleet for the pages: eu (these pages), us
// (connected), ap (offline).
type fakeFleet struct {
	current string
	hits    map[string][]FleetHit // by "p<id>" or "j<id>"
	silent  []string
}

func (f fakeFleet) Current() string { return f.current }
func (f fakeFleet) Members() []FleetMember {
	now := testNow
	return []FleetMember{
		{Name: "ap", State: "offline", SeenAt: now.Add(-14 * time.Minute), Fingerprint: "SHA256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Origin: "api", AddedBy: "token:bootstrap"},
		{Name: "eu", State: "connected", Version: "0.8.0", Since: now.Add(-time.Hour), Fingerprint: "SHA256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Origin: "api"},
		{Name: "old", State: "removed", Fingerprint: "SHA256:ooo", RemovedAt: now.Add(-48 * time.Hour)},
		{Name: "us", State: "connected", Version: "0.8.0", Since: now.Add(-time.Hour), Fingerprint: "SHA256:uuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuu", Origin: "api", StoreReplaced: now},
	}
}
func (f fakeFleet) Locate(ctx context.Context, instance string, pipeline, job uint64) ([]FleetHit, []string) {
	k := "p"
	id := pipeline
	if job != 0 {
		k, id = "j", job
	}
	return f.hits[k+strconv.FormatUint(id, 10)], f.silent
}

// Every address in a cluster's pages served by a hub is under its base,
// except the links to the hub itself.
func TestFleetBasePath(t *testing.T) {
	under := func(h http.Handler) *httptest.Server {
		mux := http.NewServeMux()
		mux.Handle("/m/eu/", http.StripPrefix("/m/eu", h))
		return httptest.NewServer(mux)
	}
	ts, _, ids, _ := consoleBuild(t, func(*store.Store) testOpts { return testOpts{Base: "/m/eu", Fleet: fakeFleet{current: "eu"}} }, under)
	root := regexp.MustCompile(`(?:href|src|action|data-url)="(/[^"]*)"`)
	var build int64
	for _, id := range ids {
		build = id
	}
	for _, p := range []string{"/m/eu/", "/m/eu/builds", "/m/eu/insights", "/m/eu/images", "/m/eu/nodes", "/m/eu/builds/" + strconv.FormatInt(build, 10), "/m/eu/builds/" + strconv.FormatInt(build, 10) + "/steps/1"} {
		pg := get(t, ts, p)
		if pg.status != 200 {
			t.Errorf("%s: %d", p, pg.status)
			continue
		}
		for _, m := range root.FindAllStringSubmatch(pg.body, -1) {
			u := m[1]
			if !strings.HasPrefix(u, "/m/eu/") && u != "/" && !strings.HasPrefix(u, "/m/us/") && !strings.HasPrefix(u, "/m/ap/") && !strings.HasPrefix(u, "/static/") {
				t.Errorf("%s: an address outside the base: %q", p, u)
			}
			if strings.HasPrefix(u, "/m/eu/m/") || strings.HasPrefix(u, "/m/eu/~") {
				t.Errorf("%s: a hub link moved under the base: %q", p, u)
			}
		}
	}
	pg := get(t, ts, "/m/eu/")
	for _, want := range []string{`data-base="/m/eu"`, `href="/m/us/"`, `href="/"`, ">ap<", "not connected", "2 of 3 connected"} {
		if !strings.Contains(pg.body, want) {
			t.Errorf("the cluster switcher: no %q", want)
		}
	}
	// The removed member is not in the switcher.
	if strings.Contains(pg.body, ">old<") {
		t.Error("a removed member in the switcher")
	}
	// The search index and a redirect.
	idx := get(t, ts, "/m/eu/search-index")
	var items struct{ Items []struct{ URL string } }
	json.Unmarshal([]byte(idx.body), &items)
	if len(items.Items) == 0 {
		t.Fatal("empty search index")
	}
	for _, it := range items.Items {
		if !strings.HasPrefix(it.URL, "/m/eu/") {
			t.Errorf("search index URL %q", it.URL)
		}
	}
	m := regexp.MustCompile(`href="(/m/eu/projects/[^"]+)"`).FindStringSubmatch(pg.body)
	if m == nil {
		t.Fatal("no project switch link")
	}
	sw := get(t, ts, strings.ReplaceAll(m[1], "&amp;", "&"))
	if sw.status != http.StatusFound || !strings.HasPrefix(sw.header.Get("Location"), "/m/eu/") {
		t.Errorf("project switch: %d %q", sw.status, sw.header.Get("Location"))
	}
	if c := sw.header.Get("Set-Cookie"); !strings.Contains(c, "Path=/m/eu/") {
		t.Errorf("project cookie %q", c)
	}
}

// The hub's own pages: the members, and addresses that name no cluster.
func TestFleetHubPages(t *testing.T) {
	f := fakeFleet{hits: map[string][]FleetHit{
		// Both clusters read the pipeline from GitLab and know its jobs; each
		// ran one of them (its pod).
		"j7": {{Member: "us", Job: &store.JobInfo{ID: 7}}, {Member: "eu", Job: &store.JobInfo{ID: 7, Node: "n1"}}},
		"p50": {{Member: "eu", Pipeline: &store.Pipeline{ID: 50, CommitTitle: "fix", ProjectPath: "g/p"}, Jobs: []store.JobInfo{
			{Instance: "0a1b2c", ID: 7, Name: "build-a", Stage: "build", Status: "success", Duration: 41 * time.Second, Node: "n1"},
			{Instance: "0a1b2c", ID: 8, Name: "build-b", Stage: "build"}, {Instance: "0a1b2c", ID: 9, Name: "lint", Stage: "test", Status: "success"}}},
			{Member: "us", Jobs: []store.JobInfo{{Instance: "0a1b2c", ID: 7, Name: "build-a", Stage: "build"},
				{Instance: "0a1b2c", ID: 8, Name: "build-b", Stage: "build", PodCreated: testNow}}}, {Member: "ap"}},
		"p60": {{Member: "us", Pipeline: &store.Pipeline{ID: 60}}},
	}}
	srv, err := New(Empty{}, testLogger(), Options{Fleet: f, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return testNow }
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(Secure(mux))
	defer ts.Close()
	pg := get(t, ts, "/")
	for _, want := range []string{"Members", "2 connected of 3", `href="/m/eu/"`, "not connected", "14 minutes ago", "Removed", ">old<"} {
		if !strings.Contains(pg.body, want) {
			t.Errorf("members page: no %q", want)
		}
	}
	// A store replaced is said on the cluster's page, not in its row.
	if strings.Contains(pg.body, "store replaced") {
		t.Error("a store replaced is a state of the Members row")
	}
	// An away cluster's address shows its pages as they were last seen.
	if !strings.Contains(pg.body, `href="/m/ap/"`) {
		t.Error("an offline member is not a link")
	}
	if j := get(t, ts, "/jobs/0a1b2c/7"); j.status != http.StatusFound || j.header.Get("Location") != "/m/eu/jobs/0a1b2c/7" {
		t.Errorf("job: %d %q", j.status, j.header.Get("Location"))
	}
	if j := get(t, ts, "/jobs/0a1b2c/9"); j.status != http.StatusNotFound {
		t.Errorf("unknown job: %d", j.status)
	}
	if p := get(t, ts, "/pipelines/0a1b2c/60"); p.status != http.StatusFound || p.header.Get("Location") != "/m/us/pipelines/0a1b2c/60" {
		t.Errorf("a pipeline of one cluster: %d %q", p.status, p.header.Get("Location"))
	}
	p := get(t, ts, "/pipelines/0a1b2c/50")
	if p.status != 200 {
		t.Fatalf("a pipeline across clusters: %d", p.status)
	}
	for _, want := range []string{"fix", "jobs in 2 clusters", `href="/m/eu/jobs/0a1b2c/7"`, `href="/m/us/jobs/0a1b2c/8"`, "build-b", "41s", "Other runners", ">lint<"} {
		if !strings.Contains(p.body, want) {
			t.Errorf("pipeline page: no %q", want)
		}
	}
	for _, wrong := range []string{`href="/m/us/jobs/0a1b2c/7"`, `href="/m/eu/jobs/0a1b2c/8"`} {
		if strings.Contains(p.body, wrong) {
			t.Errorf("pipeline page: a job under a cluster that did not run it: %s", wrong)
		}
	}
	f.silent = []string{"ap"}
	srv2, _ := New(Empty{}, testLogger(), Options{Fleet: f})
	srv2.now = func() time.Time { return testNow }
	mux2 := http.NewServeMux()
	srv2.Register(mux2)
	ts2 := httptest.NewServer(mux2)
	defer ts2.Close()
	if m := get(t, ts2, "/pipelines/0a1b2c/99"); m.status != 404 || !strings.Contains(m.body, "did not answer in time (ap)") {
		t.Errorf("silent member: %d", m.status)
	}
	// A cluster's address while it is away (try again), removed (gone) or
	// unknown.
	for _, c := range []struct {
		name, want, retry string
		status            int
	}{
		{"ap", "not connected to this hub now (last contact 14 minutes ago)", "10", http.StatusBadGateway},
		{"old", "was removed from this fleet on Oct 18, 2026 12:00 UTC", "", http.StatusGone},
		{"zz", "No cluster named zz", "", http.StatusNotFound},
		{"<b>", "No cluster of this name", "", http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		srv2.MemberAway(rec, httptest.NewRequest("GET", "/m/x/", nil), c.name)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.want) || rec.Header().Get("Retry-After") != c.retry {
			t.Errorf("away %s: %d retry %q, no %q", c.name, rec.Code, rec.Header().Get("Retry-After"), c.want)
		}
	}
}

// The console's script builds no address of its own pages from the root:
// in a fleet hub those are under the cluster's base.
func TestScriptAddressesUseTheBase(t *testing.T) {
	b, err := files.ReadFile("static/console.js")
	if err != nil {
		t.Fatal(err)
	}
	lit := regexp.MustCompile("(BASE \\+ )?[\"'`]/(builds|pipelines|events|search-index|insights|images|nodes|jobs|projects)\\b")
	for _, m := range lit.FindAllStringSubmatch(string(b), -1) {
		if m[1] == "" {
			t.Errorf("console.js: an address from the root: %q", m[0])
		}
	}
}
