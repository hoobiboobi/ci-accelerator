package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/diskprobe"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// A duration is rounded once, then put in its form; a negative one is none.
func TestDurationRoundsOnce(t *testing.T) {
	for d, want := range map[time.Duration]string{
		9999 * time.Millisecond:    "10 s",
		59500 * time.Millisecond:   "1 min 0 s",
		119900 * time.Millisecond:  "2 min 0 s",
		3599600 * time.Millisecond: "1 h 0 min",
		-5 * time.Second:           "0.0 s",
		9940 * time.Millisecond:    "9.9 s",
	} {
		if got := strings.ReplaceAll(duration(d), " ", " "); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

// Names are cut by letters, never inside one.
func TestCutByLetters(t *testing.T) {
	if got := projInitial("группа/проект"); got != "П" {
		t.Errorf("projInitial: %q", got)
	}
	v := &PipelinesView{}
	pipeFilter(url.Values{"q": {strings.Repeat("ж", 300)}}, testNow, v, &store.PipeQuery{})
	if !utf8Valid(v.Q) || len([]rune(v.Q)) != 200 {
		t.Errorf("search cut to %d letters, valid %v", len([]rune(v.Q)), utf8Valid(v.Q))
	}
}

func utf8Valid(s string) bool {
	return !strings.ContainsRune(s, '�') && strings.ToValidUTF8(s, "�") == s
}

// emptyConsole is a console without sign-in over an empty store.
func emptyConsole(t *testing.T, o testOpts) (*httptest.Server, *store.Store) {
	t.Helper()
	testLogs = &lockedBuffer{}
	st, err := store.Open(ctx, t.TempDir(), store.Options{Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	backend, opt := o.wire(t, st)
	srv, err := New(backend, testLogger(), opt)
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return testNow }
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(AccessLog(testLogger(), Secure(Compress(srv.Gate(mux)))))
	t.Cleanup(ts.Close)
	return ts, st
}

// savePipeline stores one job of a pipeline of project 42 and its build.
func savePipeline(t *testing.T, st *store.Store, pipe uint64, start time.Time) {
	t.Helper()
	inst := history.InstancePrefix("https://gitlab.example.com/")
	b := &history.Build{Ref: fmt.Sprint("ref-", pipe), Daemon: "bk-daemon-aaaaa", Target: "api", Started: start, Completed: start.Add(5 * time.Second),
		Attribution: history.Attribution{State: history.Attributed, Instance: inst, Pipeline: pipe, Job: pipe * 10},
		Steps: []history.Step{{Key: "[1/2]", Name: "FROM docker.io/library/alpine:3.20", State: history.Cached, Started: start, Completed: start},
			{Key: "[2/2]", Name: "RUN make", State: history.Built, Started: start, Completed: start.Add(5 * time.Second), Line: 2}}}
	if err := st.SaveJob(ctx, store.Job{Instance: inst, JobID: pipe * 10, PipelineID: pipe, ProjectID: 42, ProjectPath: "group/app", Name: "b", Stage: "build",
		Ref: "main", SHA: "0123456789abcdef0123456789abcdef01234567", URL: fmt.Sprintf("https://gitlab.example.com/group/app/-/jobs/%d", pipe*10)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveBuild(ctx, store.Build{Build: b, Generation: 1}); err != nil {
		t.Fatal(err)
	}
}

// The pipelines list offers older pipelines only when there are some.
func TestPipelinesOlderOnlyWhenOlder(t *testing.T) {
	older := regexp.MustCompile(`href="(/pipelines\?[^"]*before=[^"]*)"`)
	for _, n := range []int{pipePage, pipePage + 1} {
		ts, st := emptyConsole(t, testOpts{})
		t0 := testNow.Add(-2 * time.Hour)
		for i := 0; i < n; i++ {
			savePipeline(t, st, uint64(600+i), t0.Add(time.Duration(i)*time.Minute))
		}
		p := get(t, ts, "/pipelines")
		m := older.FindStringSubmatch(p.body)
		if (m != nil) != (n > pipePage) {
			t.Errorf("%d pipelines: Older link %v", n, m)
			continue
		}
		if m != nil {
			next := get(t, ts, html.UnescapeString(m[1]))
			if !strings.Contains(next.body, "/pipelines/") {
				t.Errorf("%d pipelines: the Older page lists none", n)
			}
		}
	}
}

// A static file is kept for a year only under its current version.
func TestStaticImmutableOnlyForItsVersion(t *testing.T) {
	ts, _, _ := console(t)
	home := get(t, ts, "/")
	m := regexp.MustCompile(`"(/static/console\.js\?v=[0-9a-f]+)"`).FindStringSubmatch(home.body)
	if m == nil {
		t.Fatal("console.js not linked with a version")
	}
	if p := get(t, ts, m[1]); p.header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("current version: %q", p.header.Get("Cache-Control"))
	}
	for _, path := range []string{"/static/console.js?v=000000000000", "/static/og.png?v=x"} {
		if p := get(t, ts, path); p.header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: %q, want no-cache", path, p.header.Get("Cache-Control"))
		}
	}
}

// The steps that ran are counted one way in the head, the changes and the
// comparison: failed and canceled steps ran.
func TestStepsRan(t *testing.T) {
	b := &history.Build{Steps: []history.Step{
		{Key: "[1/4]", Name: "FROM alpine", State: history.Cached},
		{Key: "[2/4]", Name: "RUN a", State: history.Built},
		{Key: "[3/4]", Name: "RUN b", State: history.Built, Error: "context canceled"},
		{Key: "[4/4]", Name: "RUN c", State: history.Cached},
	}}
	if n := stepsRan(b); n != 2 {
		t.Errorf("stepsRan: %d, want 2", n)
	}
}

// A canceled build is said as such where builds are grouped.
func TestCanceledTexts(t *testing.T) {
	st := statuses["canceled"]
	b := store.BuildRow{Error: "context canceled", Completed: testNow}
	if text := reasonText(b, st); strings.Contains(text, "Failed") || !strings.Contains(text, "Canceled") {
		t.Errorf("reasonText: %q", text)
	}
	if fingerprint(b, st) == fingerprint(store.BuildRow{Error: "exit code: 1", Completed: testNow}, statuses["failed"]) {
		t.Error("a canceled build grouped with failed ones")
	}
	if fingerprint(b, st) != fingerprint(store.BuildRow{Error: "rpc error: code = Canceled desc = context canceled", Completed: testNow}, st) {
		t.Error("canceled builds with one text in two groups")
	}
	v := &PipelineView{Rebuilt: []BuildItem{{Status: st}}}
	if s := pipelineSentence(v); !strings.Contains(s, "canceled") || strings.Contains(s, "failed") {
		t.Errorf("one image: %q", s)
	}
	v = &PipelineView{Rebuilt: []BuildItem{{Status: st}, {Status: statuses["failed"]}, {Status: st}}}
	if s := pipelineSentence(v); !strings.Contains(s, "1 failed") || !strings.Contains(s, "2 canceled") {
		t.Errorf("three images: %q", s)
	}
}

// A bar whose times fall outside its axis ends at the axis' end.
func TestFitWidth(t *testing.T) {
	for _, c := range []struct{ l, w, want string }{
		{"84.75", "42.37", "15.25"}, {"10", "20", "20"}, {"100.00", "0.35", "0.00"}, {"x", "5", "5"},
	} {
		if got := fitWidth(c.l, c.w); got != c.want {
			t.Errorf("fitWidth(%s, %s): %s, want %s", c.l, c.w, got, c.want)
		}
	}
}

// The figures kept per project are bounded: stale ones go, then the
// oldest, never the one just read.
func TestEvictStats(t *testing.T) {
	now := testNow
	rows := func(n int) []store.BuildRow { return make([]store.BuildRow, n) }
	m := map[string]*cachedStats{
		"stale": {at: now.Add(-2 * statsFresh), builds: rows(10)},
		"old":   {at: now.Add(-30 * time.Second), builds: rows(statsRows / 2)},
		"new":   {at: now.Add(-10 * time.Second), builds: rows(statsRows / 2)},
		"keep":  {at: now, builds: rows(10)},
	}
	evictStats(m, "keep", now)
	if _, ok := m["stale"]; ok {
		t.Error("stale figures kept")
	}
	if _, ok := m["old"]; ok {
		t.Error("over the bound, the oldest kept")
	}
	if m["new"] == nil || m["keep"] == nil {
		t.Errorf("kept: %v", m)
	}
	small := map[string]*cachedStats{"stale": {at: now.Add(-2 * statsFresh), builds: rows(1)}, "keep": {at: now, builds: rows(1)}}
	if evictStats(small, "keep", now); small["stale"] != nil {
		t.Error("stale figures kept under the bound")
	}
	big := map[string]*cachedStats{"keep": {at: now, builds: rows(statsRows * 2)}}
	evictStats(big, "keep", now)
	if big["keep"] == nil {
		t.Error("the figures just read were dropped")
	}
}

// The Nodes page says a GC limit differs per node instead of one node's.
func TestGCFleet(t *testing.T) {
	a := []collector.GCRule{{All: true, MaxUsed: 75 << 30, MinFree: 30 << 30}}
	b := []collector.GCRule{{All: true, MaxUsed: 150 << 30, MinFree: 60 << 30}}
	rows, caps := gcFleet([][]collector.GCRule{a, a})
	if strings.Contains(fmt.Sprint(rows, caps), "differs") || len(caps) == 0 || !strings.Contains(caps[0].V, "per node") {
		t.Errorf("same everywhere: %v %v", rows, caps)
	}
	rows, caps = gcFleet([][]collector.GCRule{a, b})
	for _, kv := range caps {
		if kv.V != differs {
			t.Errorf("differing caps: %v", caps)
		}
	}
	if len(rows) != 1 || rows[0].Rule != differs {
		t.Errorf("differing rows: %v", rows)
	}
}

// A disk share is rounded down: "9 %" next to "Under 10 % available".
func TestDiskShareRoundsDown(t *testing.T) {
	v := diskView(collector.Inspection{Disk: diskOf(1000, 96, 96)}) // 9.6 %
	if v.Avail != "9 %" || v.Level != "crit" {
		t.Errorf("9.6 %% available: %q %q", v.Avail, v.Level)
	}
}

// A running build's step that started early and ends near the right edge
// has its label at the right edge, over its bar, not left of the lane.
func TestLiveLabelOfAnEarlyBar(t *testing.T) {
	_, _, _, srv := consoleServer(t)
	inst := history.InstancePrefix("https://gitlab.example.com/")
	t0 := testNow.Add(-620 * time.Second)
	lb := &collector.LiveBuild{Active: collector.Active{Daemon: "box-buildkit-abcde", Ref: "live-now", Started: t0, Instance: inst, Pipeline: 777, Job: 778},
		Steps: []history.LiveStep{
			{Key: "[1/3]", Name: "RUN make all", State: "built", Started: t0.Add(5 * time.Second), Completed: t0.Add(600 * time.Second)},
			{Key: "[2/3]", Name: "RUN make install", State: "running", Started: t0.Add(600 * time.Second)},
			{Key: "[3/3]", Name: "RUN true", State: "waiting"},
		}, Running: 1}
	v := srv.liveView(ctx, lb, inst, 42, "new-image", "node-1", time.UTC)
	for _, r := range v.Rows {
		if r.Key == "[1/3]" && r.LabelEnd != "0.00" {
			t.Errorf("label of the early bar ends %s%% from the right", r.LabelEnd)
		}
	}
}

// signedInWith is signedInServer with daemons' live state.
func signedInWith(t *testing.T, daemons live) (*httptest.Server, func(user string) *http.Client) {
	t.Helper()
	g := newSignInGitLab(t)
	to, _ := url.Parse(g.srv.URL)
	via := &http.Client{Transport: rewrite{to}, Timeout: 10 * time.Second}
	const gl = "https://gitlab.example.com/"
	ts, _, _, _ := consoleBuild(t, func(st *store.Store) testOpts {
		client := gitlab.New([]string{gl})
		client.HTTP = via
		a, err := auth.New(auth.Config{PublicURL: "https://127.0.0.1", Key: make([]byte, 32), HTTP: via, Now: g.now,
			Instances: []auth.Instance{{URL: gl, ClientID: "cid", Secret: "client-secret-value", Admins: []string{"id:1"}}}}, st, client, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		return testOpts{Auth: a, API: &gitlab.Fetcher{Client: client, Store: st}, Live: daemons}
	}, httptest.NewTLSServer)
	inst := history.InstancePrefix(gl)
	browser := func(user string) *http.Client {
		jar, _ := cookiejar.New(nil)
		c := new(http.Client)
		*c = *ts.Client()
		c.Jar = jar
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		if user == "" {
			return c
		}
		res, err := c.Get(ts.URL + "/auth/sign-in/" + inst + "?to=/")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		loc, _ := url.Parse(res.Header.Get("Location"))
		res, err = c.Get(ts.URL + "/auth/callback/" + inst + "?code=" + user + "&state=" + url.QueryEscape(loc.Query().Get("state")))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return c
	}
	return ts, browser
}

// With sign-in, what builds name on a node (running targets, cache
// mounts, base images) is shown to admins only; others see counts and
// sizes.
func TestNodeNamesForAdminsOnly(t *testing.T) {
	img := map[string]int64{"registry.example/secretteam/base:1": 100 << 20}
	daemons := live{
		{Name: "box-buildkit-abcde", Connected: true, InspectedAt: testNow, Active: []collector.Active{{Target: "secret-target", Started: testNow.Add(-time.Minute)}},
			Inspection: collector.Inspection{Usage: collector.Usage{ImageSizes: img, MountList: []collector.Mount{{Description: "cached mount /secretproj/.cache from exec", Size: 1 << 20}}}}},
		{Name: "box-buildkit-fghij", Connected: true, InspectedAt: testNow, Inspection: collector.Inspection{Usage: collector.Usage{ImageSizes: img}}},
	}
	ts, browser := signedInWith(t, daemons)
	secrets := []string{"secret-target", "secretproj", "secretteam"}
	for _, who := range []string{"bob", "alice", "root"} {
		c := browser(who)
		body := fetch(t, c, ts, "GET", "/nodes", nil).body + fetch(t, c, ts, "GET", "/nodes/box-buildkit-abcde", nil).body
		for _, s := range secrets {
			if shown := strings.Contains(body, s); shown != (who == "root") {
				t.Errorf("%s: %q shown %v", who, s, shown)
			}
		}
	}
}

// One person cannot hold every live stream.
func TestStreamsPerPerson(t *testing.T) {
	ts, browser := signedInWith(t, nil)
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	open := func(c *http.Client) int {
		req, _ := http.NewRequestWithContext(sctx, "GET", ts.URL+"/events", nil)
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			res.Body.Close()
		}
		return res.StatusCode
	}
	bob := browser("bob")
	for i := 0; i < StreamsPerPerson; i++ {
		if code := open(bob); code != http.StatusOK {
			t.Fatalf("stream %d: %d", i+1, code)
		}
	}
	if code := open(bob); code != http.StatusServiceUnavailable {
		t.Errorf("stream %d of one person: %d, want 503", StreamsPerPerson+1, code)
	}
	if code := open(browser("alice")); code != http.StatusOK {
		t.Errorf("another person's stream: %d", code)
	}
}

// The sign-in gate: an encoded slash is no static file; headers the
// pages depend on are there.
func TestGateDetails(t *testing.T) {
	ts, _, _, _, browser := signedIn(t)
	nobody := browser("")
	for _, path := range []string{"/static%2Fx", "/static%2F..%2Fbuilds%2F1", "/auth%2Fx"} {
		p := fetch(t, nobody, ts, "GET", path, nil)
		if p.status == http.StatusNotFound || strings.Contains(p.body, "console ") {
			t.Errorf("%s without sign-in: %d, %d bytes", path, p.status, len(p.body))
		}
	}
	if p := fetch(t, nobody, ts, "GET", "/events", nil); p.status != http.StatusUnauthorized || p.header.Get("Cache-Control") != "no-store" {
		t.Errorf("a 401: %d %q", p.status, p.header.Get("Cache-Control"))
	}
	alice := browser("alice")
	p := fetch(t, alice, ts, "GET", "/", map[string]string{"Accept-Encoding": "gzip"})
	vary := strings.Join(p.header.Values("Vary"), ",")
	if !strings.Contains(vary, "Cookie") || !strings.Contains(vary, "Accept-Encoding") {
		t.Errorf("Vary: %q", vary)
	}
	if p.header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("Referrer-Policy: %q", p.header.Get("Referrer-Policy"))
	}
	if p.header.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS on a loopback address")
	}
	inst := history.InstancePrefix("https://gitlab.example.com/")
	res := fetch(t, alice, ts, "GET", "/projects/"+inst+"/42", nil)
	cookie := strings.Join(res.header.Values("Set-Cookie"), "; ")
	if !strings.Contains(cookie, "project=") || !strings.Contains(cookie, "Secure") {
		t.Errorf("project cookie: %q", cookie)
	}
	if allowedHost("evillocalhost", "console.example.com") || !allowedHost("a.localhost", "console.example.com") {
		t.Error("allowedHost: a host ending in localhost is not a .localhost name")
	}
}

// HSTS where the console's address is https and not a loopback one.
func TestStrictTransport(t *testing.T) {
	for u, want := range map[string]bool{
		"https://console.example.com": true, "https://console.example.com:8443/": true,
		"https://127.0.0.1": false, "https://[::1]": false, "https://localhost": false, "https://x.localhost": false,
		"http://console.example.com": false, "": false,
	} {
		if got := strictTransport(u); got != want {
			t.Errorf("%q: %v, want %v", u, got, want)
		}
	}
}

// Wording: plurals, the memory step by its command, the window in a
// preview's address.
func TestWording(t *testing.T) {
	ts, _, _ := console(t)
	p := get(t, ts, "/insights?window=7d")
	if !regexp.MustCompile(`property="og:url" content="[^"]*/insights\?window=7d"`).MatchString(p.body) {
		t.Errorf("og:url without the window")
	}
}

// diskOf is a probe's reading: total, free and available bytes.
func diskOf(total, free, avail uint64) *diskprobe.Disk {
	return &diskprobe.Disk{Total: total, Free: free, Available: avail}
}
