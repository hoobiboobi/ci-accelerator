package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// testNow is the pages' clock: the recorded builds were made the evening
// before.
var testNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

var ctx = context.Background()

const evil = `<script>alert(1)</script><img src=x onerror=alert(2)>`

// console serves the pages over a store holding the history package's
// recorded builds. Job names, commit titles, a target and a step name
// carry markup, which must reach the pages as text.
func console(t *testing.T) (*httptest.Server, *store.Store, map[string]int64) {
	ts, st, ids, _ := consoleServer(t)
	return ts, st, ids
}

func consoleServer(t *testing.T) (*httptest.Server, *store.Store, map[string]int64, *Server) {
	return consoleWith(t, Options{})
}

func consoleWith(t *testing.T, opt Options) (*httptest.Server, *store.Store, map[string]int64, *Server) {
	t.Helper()
	return consoleBuild(t, func(*store.Store) Options { return opt }, httptest.NewServer)
}

// consoleBuild is consoleWith for options that need the store (sign-in),
// served by start (plain or TLS).
func consoleBuild(t *testing.T, options func(*store.Store) Options, start func(http.Handler) *httptest.Server) (*httptest.Server, *store.Store, map[string]int64, *Server) {
	t.Helper()
	st, err := store.Open(ctx, t.TempDir(), store.Options{Now: func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dirs, err := filepath.Glob("../history/testdata/records/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	cfg := history.Config{Instances: map[string]bool{history.InstancePrefix("https://gitlab.example.com/"): true}, SecretKey: st.SecretKey()}
	read := func(d, n string) []byte {
		b, err := os.ReadFile(filepath.Join(d, n))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return b
	}
	var bs []*history.Build
	for _, d := range dirs {
		rec, err := history.ParseRecord(read(d, "record.json"))
		if err != nil {
			t.Fatal(err)
		}
		progress, err := history.DecodeProgress(bytes.NewReader(read(d, "logs.bin")))
		if err != nil {
			t.Fatal(err)
		}
		b, err := history.NewBuild(rec, progress, read(d, "provenance.json"), read(d, "trace.json"), cfg)
		if err != nil {
			t.Fatal(err)
		}
		b.Daemon = "daemon-a"
		bs = append(bs, b)
	}
	sort.Slice(bs, func(i, j int) bool { return bs[i].Started.Before(bs[j].Started) })
	// The newest build's last step gets markup in its name: the name is
	// what a Dockerfile author wrote.
	last := bs[len(bs)-1]
	last.Steps[len(last.Steps)-1].Name = "RUN echo '" + evil + "'"
	history.Link(bs)
	for _, b := range bs {
		a := b.Attribution
		if a.State != history.Attributed {
			continue
		}
		err := st.SaveJob(ctx, store.Job{Instance: a.Instance, JobID: a.Job, PipelineID: a.Pipeline, ProjectID: 42,
			ProjectPath: "example/" + evil, Name: "build-" + b.Target + evil, Stage: "build", CommitTitle: "fix " + evil,
			CommitAuthor: evil, URL: "javascript:alert(3)", Ref: "main" + evil})
		if err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]int64{}
	for _, b := range bs {
		prev, _, err := st.Previous(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		var p *history.Build
		if prev != nil {
			p = prev.Build
		}
		r, _ := history.FirstRebuild(p, b)
		id, err := st.SaveBuild(ctx, store.Build{Build: b, Generation: 1, Reason: r})
		if err != nil {
			t.Fatal(err)
		}
		ids[b.Ref] = id
	}
	var logTo io.Writer = io.Discard
	if os.Getenv("WEB_TEST_LOG") != "" {
		logTo = os.Stderr
	}
	srv, err := New(st, slog.New(slog.NewTextHandler(logTo, nil)), options(st))
	if err != nil {
		t.Fatal(err)
	}
	// The pages' windows (the last 24 hours, 7 days) end when the
	// recorded builds were made.
	srv.now = func() time.Time { return testNow }
	mux := http.NewServeMux()
	srv.Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("GET /api/v1/builds", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "[]") })
	ts := start(Secure(srv.Gate(mux)))
	t.Cleanup(ts.Close)
	return ts, st, ids, srv
}

type page struct {
	status int
	header http.Header
	body   string
	url    string
}

func get(t *testing.T, ts *httptest.Server, path string) page {
	t.Helper()
	c := ts.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := c.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return page{res.StatusCode, res.Header, string(b), path}
}

var (
	scriptTag = regexp.MustCompile(`(?i)<script\b[^>]*>`)
	ownScript = regexp.MustCompile(`^<script src="/static/(console\.js\?v=[0-9a-f]{12}" defer|theme\.js\?v=[0-9a-f]{12}")>$`)
	// Style attributes carry numbers only: positions and sizes of bars.
	styleAttr = regexp.MustCompile(`\sstyle="([^"]*)"`)
	safeStyle = regexp.MustCompile(`^((left|right|width|height|top|flex): -?[0-9]+(\.[0-9]+)?(%|px)?(; )?)+$`)
	handlers  = regexp.MustCompile(`(?i)<[^>]*\son[a-z]+\s*=`)
	// A step's span in the steps table: "1.1 s–2.3 s after start".
	stepSpan = regexp.MustCompile(`· ([^<·–]+)–([^<·–]+) after start`)
	// Attribute values: quotes inside them are escaped, so this is exact.
	quoted = regexp.MustCompile(`"[^"]*"`)
	links  = regexp.MustCompile(`href="(/[^"#?]*)`)
)

// safe fails when markup from the data reached a page as markup: the only
// script is the console's own, no element has an event handler, and the
// javascript: URL did not survive.
func (p page) safe(t *testing.T) {
	t.Helper()
	for _, tag := range scriptTag.FindAllString(p.body, -1) {
		if !ownScript.MatchString(tag) {
			t.Errorf("%s: script tag from data: %s", p.url, tag)
		}
	}
	if m := handlers.FindString(quoted.ReplaceAllString(p.body, `""`)); m != "" {
		t.Errorf("%s: event handler in markup: %s", p.url, m)
	}
	if strings.Contains(p.body, "javascript:") {
		t.Errorf("%s: javascript: URL in the page", p.url)
	}
	for _, m := range styleAttr.FindAllStringSubmatch(p.body, -1) {
		if !safeStyle.MatchString(m[1]) {
			t.Errorf("%s: style attribute with more than numbers: %q", p.url, m[1])
		}
	}
	csp := p.header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self';") || !strings.Contains(csp, "default-src 'none'") ||
		strings.Count(csp, "unsafe") != 1 || !strings.Contains(csp, "style-src-attr 'unsafe-inline'") {
		t.Errorf("%s: CSP %q", p.url, csp)
	}
	if p.header.Get("X-Content-Type-Options") != "nosniff" || p.header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("%s: headers %v", p.url, p.header)
	}
}

// pipelines returns the pipelines of the recorded builds, newest first,
// with their builds.
func pipelines(t *testing.T, st *store.Store) (keys []string, builds map[string][]store.BuildRow) {
	t.Helper()
	inst := history.InstancePrefix("https://gitlab.example.com/")
	rows, err := st.Builds(ctx, store.BuildQuery{Project: store.ProjectKey{Instance: inst, ID: 42}})
	if err != nil {
		t.Fatal(err)
	}
	builds = map[string][]store.BuildRow{}
	for _, b := range rows {
		k := store.PipelineKey(b.Instance, b.Pipeline)
		if _, ok := builds[k]; !ok {
			keys = append(keys, k)
		}
		builds[k] = append(builds[k], b)
	}
	return keys, builds
}

// Every page reachable from the overview renders, is safe, and the data
// with markup shows up as text.
func TestPagesRenderAndEscape(t *testing.T) {
	ts, _, _ := console(t)
	seen := map[string]bool{}
	queue := []string{"/", "/builds?group=pipeline", "/images?window=7d", "/nodes"}
	kinds := map[string]int{}
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		if seen[path] {
			continue
		}
		seen[path] = true
		p := get(t, ts, path)
		if p.status == http.StatusFound || p.status == http.StatusMovedPermanently {
			queue = append(queue, p.header.Get("Location"))
			continue
		}
		if p.status != http.StatusOK {
			t.Errorf("%s: %d %.200s", path, p.status, p.body)
			continue
		}
		if strings.HasPrefix(path, "/static/") {
			continue
		}
		p.safe(t)
		kind := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)[0]
		kinds[kind]++
		for _, m := range links.FindAllStringSubmatch(p.body, -1) {
			if !seen[m[1]] {
				queue = append(queue, m[1])
			}
		}
		if path == "/builds/1" {
			// The panel the script loads for every step.
			for i := 0; ; i++ {
				panel := get(t, ts, fmt.Sprintf("/builds/1/steps/%d?part=panel", i))
				if panel.status == http.StatusNotFound {
					break
				}
				if panel.status != http.StatusOK || strings.Contains(panel.body, "<html") {
					t.Errorf("panel %d: %d, %.80q", i, panel.status, panel.body)
				}
				panel.safe(t)
				queue = append(queue, fmt.Sprintf("/builds/1/steps/%d", i))
			}
		}
	}
	if kinds[""] != 1 || kinds["pipelines"] < 8 || kinds["builds"] < 13 || kinds["images"] < 3 || kinds["nodes"] != 1 {
		t.Errorf("pages visited: %v", kinds)
	}
	all := ""
	for path := range seen {
		if !strings.HasPrefix(path, "/static/") {
			all += get(t, ts, path).body
		}
	}
	for _, want := range []string{
		"fix &lt;script&gt;alert(1)&lt;/script&gt;", // commit title
		"build-api&lt;script&gt;",                   // job name
		"main&lt;script&gt;",                        // branch
		"RUN echo &#39;&lt;script&gt;alert(1)&lt;/script&gt;&lt;img src=x onerror=alert(2)&gt;&#39;", // step name
	} {
		if !strings.Contains(all, want) {
			t.Errorf("escaped text %q not on any page", want)
		}
	}
}

func TestNotFoundAndBadPaths(t *testing.T) {
	ts, _, _ := console(t)
	for _, path := range []string{
		"/builds/99999", "/builds/abc", "/builds/1/steps/999", "/builds/1/steps/-1", "/builds/1/steps/x",
		"/pipelines/abcdef/1", "/pipelines/abcdef/x", "/jobs/abcdef/77", "/jobs/abcdef/x", "/images/no-such-image", "/nope",
	} {
		p := get(t, ts, path)
		if p.status != http.StatusNotFound {
			t.Errorf("%s: %d", path, p.status)
		}
		p.safe(t)
		if !strings.Contains(p.body, "with this address is stored here") {
			t.Errorf("%s: not the console's own page: %.80s", path, p.body)
		}
	}
	if p := get(t, ts, "/builds/99999?x="+url.QueryEscape(evil)); p.status != http.StatusNotFound {
		t.Errorf("unknown build with a query: %d", p.status)
	}
}

func TestStaticFilesAreVersioned(t *testing.T) {
	ts, _, _ := console(t)
	home := get(t, ts, "/")
	for _, name := range []string{"console.css", "console.js"} {
		m := regexp.MustCompile(`"(/static/` + regexp.QuoteMeta(name) + `\?v=[0-9a-f]+)"`).FindStringSubmatch(home.body)
		if m == nil {
			t.Fatalf("%s not linked with a version: %s", name, home.body[:400])
		}
		p := get(t, ts, m[1])
		if p.status != http.StatusOK || p.header.Get("Cache-Control") != "public, max-age=31536000, immutable" || len(p.body) < 1000 {
			t.Errorf("%s: %d %q %d bytes", m[1], p.status, p.header.Get("Cache-Control"), len(p.body))
		}
		if p := get(t, ts, "/static/"+name); p.header.Get("Cache-Control") != "no-cache" {
			t.Errorf("unversioned %s: %q", name, p.header.Get("Cache-Control"))
		}
	}
}

// The overview: one sentence, the reasons for rebuilds by the time they
// took, the images with their last builds.
func TestOverview(t *testing.T) {
	ts, st, _ := console(t)
	_, byPipe := pipelines(t, st)
	n, cached := 0, 0
	for _, bs := range byPipe {
		for _, b := range bs {
			n++
			if b.Built == 0 && b.Error == "" {
				cached++
			}
		}
	}
	p := get(t, ts, "/")
	p.safe(t)
	want := fmt.Sprintf("<b>%d</b> image builds in the last 24 hours, <b>%d%%</b> fully from the cache.", n, cached*100/n)
	if !strings.Contains(p.body, want) {
		t.Errorf("overview sentence: want %q in %s", want, p.body)
	}
	if !strings.Contains(p.body, "Why images were rebuilt") || strings.Count(p.body, `class="rrow"`) == 0 {
		t.Error("no reasons")
	}
	// Reasons come most time first.
	var lost []time.Duration
	for _, m := range regexp.MustCompile(`<span class="rl">([^<]+)</span>`).FindAllStringSubmatch(p.body, -1) {
		lost = append(lost, parseDur(t, m[1]))
	}
	if !sort.SliceIsSorted(lost, func(i, j int) bool { return lost[i] > lost[j] }) {
		t.Errorf("reasons not by time lost: %v", lost)
	}
	// What needs attention, not a wall of figures: the images are a page
	// of their own, where each strip has its legend.
	if strings.Contains(p.body, `class="sq st-`) || !strings.Contains(p.body, `href="/images"`) {
		t.Error("the overview draws image strips, or does not link the images")
	}
	img := get(t, ts, "/images")
	if strings.Count(img.body, `class="sq st-`) == 0 {
		t.Error("no image strips")
	}
	// Five statuses at most, and never colour alone: each has its word.
	for _, k := range statusKeys {
		if !strings.Contains(img.body, statuses[k].Label+"</span>") {
			t.Errorf("legend lacks %q", statuses[k].Label)
		}
	}
	// A week back the same builds; two days later none in the window.
	if w := get(t, ts, "/?window=7d"); !strings.Contains(w.body, "in the last 7 days") {
		t.Error("7-day window")
	}
}

// parseDur reads a duration as the pages print it.
func parseDur(t *testing.T, s string) time.Duration {
	t.Helper()
	s = strings.ReplaceAll(s, "\u00a0", " ")
	var d time.Duration
	for _, part := range []struct {
		re   string
		unit time.Duration
	}{{`([0-9.]+) h`, time.Hour}, {`([0-9.]+) min`, time.Minute}, {`([0-9.]+) s`, time.Second}} {
		if m := regexp.MustCompile(part.re).FindStringSubmatch(s); m != nil {
			f, _ := strconv.ParseFloat(m[1], 64)
			d += time.Duration(f * float64(part.unit))
		}
	}
	return d
}

// The build list: every build, newest first, with filters in the address
// and pages of fifty.
func TestBuildList(t *testing.T) {
	ts, st, _ := console(t)
	keys, byPipe := pipelines(t, st)
	all := 0
	for _, bs := range byPipe {
		all += len(bs)
	}
	rows := func(path string) []string {
		p := get(t, ts, path)
		p.safe(t)
		return regexp.MustCompile(`<a class="brow[^"]*" href="(/builds/\d+)"`).FindAllString(p.body, -1)
	}
	if n := len(rows("/builds")); n != all {
		t.Errorf("all builds: %d of %d", n, all)
	}
	rebuilt := rows("/builds?outcome=rebuilt")
	cached := rows("/builds?outcome=cached")
	if len(rebuilt)+len(cached) != all || len(rebuilt) == 0 || len(cached) == 0 {
		t.Errorf("rebuilt %d + cached %d != %d", len(rebuilt), len(cached), all)
	}
	sum := 0
	for _, o := range []string{"change", "farm", "unclear", "failed"} {
		got := rows("/builds?outcome=" + o)
		sum += len(got)
		body := get(t, ts, "/builds?outcome="+o).body
		if c := strings.Count(body, `class="stat st-`+o+`"`); o != "failed" && c != len(got) {
			t.Errorf("outcome %s: %d rows, %d of that status", o, len(got), c)
		}
	}
	if sum != len(rebuilt) {
		t.Errorf("statuses add up to %d, rebuilt %d", sum, len(rebuilt))
	}
	// Cached builds are in the list, quietly.
	if !strings.Contains(get(t, ts, "/builds").body, `class="brow quiet"`) {
		t.Error("cached builds are not quiet")
	}
	// One image only.
	img := get(t, ts, "/builds?image=api").body
	if n := strings.Count(img, `<span class="bn">api</span>`); n == 0 || n != strings.Count(img, `class="brow`) {
		t.Errorf("image filter: %d of %d", n, strings.Count(img, `class="brow`))
	}
	// Grouped by pipeline: a heading per pipeline, linking its page.
	g := get(t, ts, "/builds?group=pipeline").body
	if n := strings.Count(g, `class="boxh pgh"`); n != len(keys) {
		t.Errorf("%d groups for %d pipelines", n, len(keys))
	}
	// An unknown outcome is no filter.
	if n := len(rows("/builds?outcome=" + url.QueryEscape(evil))); n != all {
		t.Errorf("an unknown outcome filtered: %d", n)
	}
}

// Pages of the list: fifty builds, then an address for the older ones.
func TestBuildListPages(t *testing.T) {
	ts, st, _ := console(t)
	keys, byPipe := pipelines(t, st)
	base := byPipe[keys[0]][0]
	for i := 0; i < 60; i++ {
		b, err := st.BuildPage(ctx, base.ID)
		if err != nil {
			t.Fatal(err)
		}
		x := *b.Build
		x.Ref = fmt.Sprintf("page-%d", i)
		x.Started = testNow.Add(-time.Duration(i+1) * time.Minute)
		x.Completed = x.Started.Add(3 * time.Second)
		if _, err := st.SaveBuild(ctx, store.Build{Build: &x, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	first := get(t, ts, "/builds")
	if n := strings.Count(first.body, `class="brow`); n != pageSize {
		t.Fatalf("first page: %d builds", n)
	}
	m := regexp.MustCompile(`href="(/builds\?before=\d+)"`).FindStringSubmatch(first.body)
	if m == nil {
		t.Fatal("no address for older builds")
	}
	second := get(t, ts, strings.ReplaceAll(m[1], "&amp;", "&"))
	if n := strings.Count(second.body, `class="brow`); n == 0 || !strings.Contains(second.body, `href="/builds">Newest</a>`) {
		t.Errorf("second page: %d builds", n)
	}
	firstIDs := regexp.MustCompile(`href="/builds/(\d+)"`).FindAllString(first.body, -1)
	for _, id := range regexp.MustCompile(`href="/builds/(\d+)"`).FindAllString(second.body, -1) {
		for _, f := range firstIDs {
			if f == id {
				t.Errorf("%s on both pages", id)
			}
		}
	}
}

// A pipeline's page: its image builds, rebuilt first, one timeline, and
// how many other jobs it had.
func TestPipelinePage(t *testing.T) {
	ts, st, _ := console(t)
	keys, byPipe := pipelines(t, st)
	checked := 0
	for _, k := range keys {
		bs := byPipe[k]
		p := get(t, ts, "/pipelines/"+k)
		p.safe(t)
		if p.status != 200 {
			t.Fatalf("%s: %d", k, p.status)
		}
		// One job row per build on the timeline, opening into its steps.
		if n := len(regexp.MustCompile(`class="row k-job[^"]*"[^>]*data-link="/builds/\d+"`).FindAllString(p.body, -1)); n != len(bs) {
			t.Errorf("%s: timeline of %d builds for %d", k, n, len(bs))
		}
		if !strings.Contains(p.body, `data-wf`) || !strings.Contains(p.body, `class="row k-step`) {
			t.Errorf("%s: the timeline has no steps", k)
		}
		if !strings.Contains(p.body, fmt.Sprintf("%d image", len(bs))) && !strings.Contains(p.body, fmt.Sprintf("All %d image", len(bs))) {
			t.Errorf("%s: sentence does not count %d images", k, len(bs))
		}
		checked++
	}
	if checked < 8 {
		t.Errorf("%d pipelines", checked)
	}
	// Jobs that built nothing are one line, with GitLab's page.
	inst, id, _ := strings.Cut(keys[0], "/")
	pid, _ := strconv.ParseUint(id, 10, 64)
	if err := st.SaveJob(ctx, store.Job{Instance: inst, JobID: 9_100_001, PipelineID: pid, ProjectID: 42, Name: "unit-tests",
		URL: "https://gitlab.example.com/p/-/jobs/9100001"}); err != nil {
		t.Fatal(err)
	}
	p := get(t, ts, "/pipelines/"+keys[0])
	if !strings.Contains(p.body, "1 other job of this pipeline built no image here") ||
		!strings.Contains(p.body, `href="https://gitlab.example.com/p/-/pipelines/`+id+`"`) {
		t.Error("the other jobs are not one line with GitLab's link")
	}
	if strings.Contains(p.body, "unit-tests") {
		t.Error("a job without a build is listed")
	}
}

// A job's address leads to its last build; a job that built nothing gets
// a short page that says so.
func TestJobPages(t *testing.T) {
	f, asked := fakeGitLab(t, "Running with gitlab-runner\n$ make test\ntoken glpat-AbCdEfGhIjKlMnOpQrSt12 used\nok\n")
	ts, st, _, _ := consoleWith(t, Options{API: f, Token: gitlab.Token("t")})
	keys, byPipe := pipelines(t, st)
	b := byPipe[keys[0]][0]
	p := get(t, ts, fmt.Sprintf("/jobs/%s/%d", b.Instance, b.Job))
	if p.status != http.StatusFound || !strings.HasPrefix(p.header.Get("Location"), "/builds/") || !strings.HasSuffix(p.header.Get("Location"), "?via=log") {
		t.Fatalf("job redirect: %d to %q", p.status, p.header.Get("Location"))
	}
	// Arriving from the job log, the page says how, with the line.
	via := get(t, ts, p.header.Get("Location"))
	if !strings.Contains(via.body, "From the GitLab job log") || !strings.Contains(via.body, fmt.Sprintf("/jobs/%s/%d", b.Instance, b.Job)) {
		t.Errorf("no way back to the job log")
	}
	if strings.Contains(get(t, ts, strings.TrimSuffix(p.header.Get("Location"), "?via=log")).body, "From the GitLab job log") {
		t.Errorf("the job-log banner shows without the job log")
	}
	inst, id, _ := strings.Cut(keys[0], "/")
	pid, _ := strconv.ParseUint(id, 10, 64)
	if err := st.SaveJob(ctx, store.Job{Instance: inst, JobID: 9_100_002, PipelineID: pid, ProjectID: 42, Name: "lint" + evil,
		URL: "https://gitlab.example.com/p/-/jobs/9100002"}); err != nil {
		t.Fatal(err)
	}
	j := get(t, ts, fmt.Sprintf("/jobs/%s/9100002", inst))
	j.safe(t)
	if j.status != 200 || !strings.Contains(j.body, "This job built no image on the box.") || !strings.Contains(j.body, "lint&lt;script&gt;") ||
		!strings.Contains(j.body, `data-url="/jobs/`+inst+`/9100002/log"`) {
		t.Errorf("job page: %d", j.status)
	}
	// The log is read from GitLab only when asked, masked.
	if *asked != "" {
		t.Errorf("a log was read before anyone asked: %s", *asked)
	}
	lg := get(t, ts, fmt.Sprintf("/jobs/%s/9100002/log", inst))
	if lg.status != 200 || *asked != "/api/v4/projects/42/jobs/9100002/trace" || strings.Contains(lg.body, "glpat-") || !strings.Contains(lg.body, "[masked]") {
		t.Errorf("log: %d %q, asked %q", lg.status, lg.body, *asked)
	}
	// A job seen as a pod: its page says what the pod told (approximate),
	// and the pipeline's timeline links the job to this page, not to GitLab.
	if err := st.SaveJob(ctx, store.Job{Instance: inst, JobID: 9_100_003, PipelineID: pid, ProjectID: 42, Name: "unit-tests", Stage: "test",
		Node: "node-b", StartedAt: testNow.Add(-5 * time.Minute), URL: "https://gitlab.example.com/p/-/jobs/9100003"}); err != nil {
		t.Fatal(err)
	}
	j3 := get(t, ts, fmt.Sprintf("/jobs/%s/9100003", inst))
	for _, want := range []string{"<dt>Result</dt><dd>unknown", "<dt>Stage</dt><dd>test</dd>", "<dt>Node</dt><dd><span class=\"mono\">node-b</span>", "(from the job pod: approximate)", `href="https://gitlab.example.com/p/-/jobs/9100003" target="_blank"`} {
		if !strings.Contains(j3.body, want) {
			t.Errorf("job page without GitLab facts lacks %q", want)
		}
	}
	pp := get(t, ts, "/pipelines/"+keys[0])
	if !strings.Contains(pp.body, `href="/jobs/`+inst+`/9100003"`) || strings.Contains(pp.body, `href="https://gitlab.example.com/p/-/jobs/9100003"`) {
		t.Error("the timeline should link a job without a build to its console page, not to GitLab")
	}
}

// An image's page: its builds over a window, the steps that rebuild most,
// the reasons; no builds in a window is a page that says so.
func TestImagePage(t *testing.T) {
	ts, st, _ := console(t)
	_, byPipe := pipelines(t, st)
	n := 0
	for _, bs := range byPipe {
		for _, b := range bs {
			if b.Target == "api" {
				n++
			}
		}
	}
	p := get(t, ts, "/images/api?window=7d")
	p.safe(t)
	if p.status != 200 || !strings.Contains(p.body, fmt.Sprintf("of %d builds in the last 7 days", n)) {
		t.Errorf("image page: %d", p.status)
	}
	if strings.Count(p.body, `class="dot st-`) != n {
		t.Errorf("%d dots for %d builds", strings.Count(p.body, `class="dot st-`), n)
	}
	if !strings.Contains(p.body, "Steps that rebuild most") || strings.Count(p.body, `<div class="srow">`) == 0 {
		t.Error("no steps")
	}
	if steps := regexp.MustCompile(`(?s)<div class="srows">.*?</section>`).FindString(p.body); steps == "" || strings.Contains(steps, "FROM ") {
		t.Errorf("resolving a base image counted as a rebuilt step: %.300s", steps)
	}
	list := get(t, ts, "/images?window=7d")
	if !strings.Contains(list.body, `href="/images/api"`) {
		t.Error("images list lacks api")
	}

	// The year comes from the daily figures: finished days only.
	before := 0
	for _, bs := range byPipe {
		for _, b := range bs {
			if b.Target == "api" && b.Started.Before(testNow.Truncate(24*time.Hour)) {
				before++
			}
		}
	}
	if err := st.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	y := get(t, ts, "/images/api?window=1y")
	y.safe(t)
	switch {
	case y.status != 200:
		t.Errorf("year: %d", y.status)
	case before == 0:
		if !strings.Contains(y.body, "No build of api in the last year") {
			t.Errorf("year without finished days: %.300s", y.body)
		}
	default:
		if !strings.Contains(y.body, fmt.Sprintf("of %d builds in the last year", before)) || strings.Count(y.body, `class="wcol"`) < 52 {
			t.Errorf("year: %d builds before today, %d weeks", before, strings.Count(y.body, `class="wcol"`))
		}
	}
}

// A live page's stream says when its pipeline changed, and only its own;
// a project's page hears its project's pipelines and says something is
// new rather than reloading.
func TestEvents(t *testing.T) {
	ts, st, _, srv := consoleServer(t)
	srv.Heartbeat = 50 * time.Millisecond
	keys, byPipe := pipelines(t, st)
	key := keys[0]
	mine, other := byPipe[keys[0]][0], byPipe[keys[1]][0]
	home := get(t, ts, "/")
	pipe := get(t, ts, "/pipelines/"+key)
	project := "project:" + mine.Instance + "/42"
	if !strings.Contains(home.body, `data-live="`+project+`"`) || !strings.Contains(home.body, `data-live-notify="true"`) ||
		!strings.Contains(pipe.body, `data-live="`+key+`"`) || strings.Contains(pipe.body, `data-live-notify`) {
		t.Fatalf("live markers missing")
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stream := func(q string) (func(bool) string, func()) {
		req, _ := http.NewRequestWithContext(sctx, "GET", ts.URL+"/events?"+q, nil)
		res, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if res.Header.Get("Content-Type") != "text/event-stream" || res.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("headers %v", res.Header)
		}
		lines := make(chan string)
		go func() {
			sc := bufio.NewScanner(res.Body)
			for sc.Scan() {
				lines <- sc.Text()
			}
			close(lines)
		}()
		return func(skipPings bool) string {
			t.Helper()
			for {
				select {
				case l, ok := <-lines:
					if !ok {
						t.Fatal("stream closed")
					}
					if l == "" || strings.HasPrefix(l, "retry:") || (skipPings && l == ": ping") {
						continue
					}
					return l
				case <-sctx.Done():
					t.Fatal("no event")
				}
			}
		}, func() { res.Body.Close() }
	}
	next, done := stream("pipeline=" + url.QueryEscape(key))
	defer done()
	if l := next(false); l != ": ping" {
		t.Errorf("first line %q, want a heartbeat", l)
	}
	job := func(b store.BuildRow, project uint64, name string) {
		if err := st.SaveJob(ctx, store.Job{Instance: b.Instance, JobID: 9_000_000 + b.Pipeline, PipelineID: b.Pipeline, ProjectID: project, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	job(other, 42, "elsewhere")
	st.Announce("")
	job(mine, 42, "here")
	if l := next(true); l != "event: changed" {
		t.Errorf("got %q", l)
	}
	if l := next(true); l != "data: "+key {
		t.Errorf("got %q, want only the page's pipeline", l)
	}
	// The project's stream: its pipelines, not another project's.
	pnext, pdone := stream("project=" + url.QueryEscape(mine.Instance+"/42"))
	defer pdone()
	if err := st.SaveJob(ctx, store.Job{Instance: mine.Instance, JobID: 9_200_001, PipelineID: 9_200_000, ProjectID: 7, Name: "other project"}); err != nil {
		t.Fatal(err)
	}
	job(other, 42, "again")
	if l := pnext(true); l != "event: changed" {
		t.Errorf("project stream got %q", l)
	}
	if l := pnext(true); l != "data: "+store.PipelineKey(other.Instance, other.Pipeline) {
		t.Errorf("project stream got %q, want its own pipeline only", l)
	}
}

type live []collector.DaemonState

func (l live) Daemons() []collector.DaemonState { return l }

// The nodes page shows each daemon's cache and running builds.
func TestNodes(t *testing.T) {
	started := testNow.Add(-42 * time.Second)
	daemons := live{{Name: "box-buildkit-abcde", Connected: true, InspectedAt: testNow,
		Inspection: collector.Inspection{Node: "node-1", Version: "v0.33.1",
			GC: []collector.GCRule{{Filters: []string{"type==exec.cachemount"}, Keep: 336 * time.Hour}, {MaxUsed: 40 << 30, MinFree: 16 << 30}},
			Usage: collector.Usage{Total: 300 << 20, Images: 200 << 20, Layers: 90 << 20, Mounts: 10 << 20,
				ImageSizes: map[string]int64{"python:3.13-slim": 200 << 20, "golang:1.27": 300 << 20},
				MountList:  []collector.Mount{{Description: "cached mount /root/.cache/pip from exec x", Size: 10 << 20, LastUsed: testNow}}}},
		Active: []collector.Active{{Daemon: "box-buildkit-abcde", Target: "api" + evil, Started: started}}},
		{Name: "box-buildkit-fghij", Connected: false, Inspection: collector.Inspection{Usage: collector.Usage{ImageSizes: map[string]int64{"python:3.13-slim": 200 << 20}}}}}
	ts, _, _, _ := consoleWith(t, Options{Live: daemons, Release: "box"})
	nodes := get(t, ts, "/nodes")
	nodes.safe(t)
	for _, want := range []string{"1 ready of 2", "on node-1", "300 MB", "/root/.cache/pip cache mount", "Duplicate cache",
		"python:3.13-slim", "Keep cache up to", "40.0 GB per node", "14 days after last use", "Running now", "Unreachable", "release <code>box</code>",
		"replaced since"} {
		if !strings.Contains(nodes.body, want) {
			t.Errorf("nodes page lacks %q", want)
		}
	}
	if strings.Contains(nodes.body, "golang:1.27") {
		t.Errorf("an image on one node listed as duplicate")
	}
	if !strings.Contains(get(t, ts, "/").body, `class="wdot"`) {
		t.Errorf("no warning dot for the unreachable daemon")
	}
}

// rewrite sends every request to the test server, whatever host it names.
type rewrite struct{ to *url.URL }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.to.Scheme, r.to.Host
	return http.DefaultTransport.RoundTrip(req)
}

// The build page reads the job's times from GitLab once, and says what
// the job spent besides building.
func TestGitLabAPIOnOpen(t *testing.T) {
	var calls int
	var mu sync.Mutex
	var ids []uint64
	var starts []time.Time
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		var out []map[string]any
		for i, id := range ids {
			out = append(out, map[string]any{"id": id, "name": "x", "status": "success",
				"started_at": starts[i].Add(-5 * time.Second).Format(time.RFC3339Nano), "finished_at": starts[i].Add(time.Minute).Format(time.RFC3339Nano),
				"duration": 65.0, "queued_duration": 3.0, "pipeline": map[string]any{"status": "success", "web_url": "https://gitlab.example.com/p/-/pipelines/1"}})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer gl.Close()
	to, _ := url.Parse(gl.URL)
	client := gitlab.New([]string{"https://gitlab.example.com/"})
	client.HTTP = &http.Client{Transport: rewrite{to}}
	f := &gitlab.Fetcher{Client: client}
	ts, st, _, srv := consoleWith(t, Options{API: f, Token: gitlab.Token("t")})
	f.Store = st
	srv.APIWait = 3 * time.Second
	keys, byPipe := pipelines(t, st)
	builds := byPipe[keys[0]]
	mu.Lock()
	seen := map[uint64]bool{}
	for i := len(builds) - 1; i >= 0; i-- {
		b := builds[i]
		if !seen[b.Job] {
			seen[b.Job] = true
			ids, starts = append(ids, b.Job), append(starts, b.Started)
		}
	}
	mu.Unlock()
	page := get(t, ts, fmt.Sprintf("/builds/%d", builds[0].ID))
	if !strings.Contains(page.body, "1\u00a0min 5\u00a0s in all") || !strings.Contains(page.body, "Waited for a runner") {
		t.Errorf("the build page did not use GitLab's job times (%d calls)", calls)
	}
	get(t, ts, fmt.Sprintf("/builds/%d", builds[0].ID))
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("%d calls to GitLab for a finished pipeline", calls)
	}
}

// A reason that names a daemon that is gone says so.
func TestGoneDaemon(t *testing.T) {
	ts, st, _, _ := consoleWith(t, Options{Live: live{{Name: "daemon-a", Connected: true}}})
	keys, byPipe := pipelines(t, st)
	row := byPipe[keys[0]][0]
	p, err := st.BuildPage(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	b := p.Build
	err = st.SetReason(ctx, b.Ref, b.Daemon, history.Reason{Kind: history.CacheElsewhere, Step: b.Steps[len(b.Steps)-1],
		Old: "box-buildkit-old12", New: "daemon-a"})
	if err != nil {
		t.Fatal(err)
	}
	if page := get(t, ts, fmt.Sprintf("/builds/%d", row.ID)); !strings.Contains(page.body, "<b>old12</b> is gone") {
		t.Errorf("no note that the daemon is gone")
	}
}

// The build page: the verdict, what changed since the previous build of
// the target (only what differs), the steps.
func TestBuildPage(t *testing.T) {
	ts, st, _ := console(t)
	keys, byPipe := pipelines(t, st)
	compared := 0
	for _, k := range keys {
		for _, b := range byPipe[k] {
			p := get(t, ts, fmt.Sprintf("/builds/%d", b.ID))
			p.safe(t)
			st := rowStatus(b)
			if !strings.Contains(p.body, `class="stat st-`+st.Key+`"`) || !strings.Contains(p.body, st.Long+"</span>") {
				t.Errorf("build %d: status %s not shown", b.ID, st.Key)
			}
			if strings.Contains(p.body, "What changed since the previous build") && strings.Contains(p.body, `<dl class="chg">`) {
				compared++
			}
			if !strings.Contains(p.body, fmt.Sprintf("<b>%d</b> of ", b.Built)) {
				t.Errorf("build %d: steps run not said", b.ID)
			}
		}
	}
	if compared == 0 {
		t.Error("no build page says what changed")
	}
}

// Every link in a step's history opens that same step in the other build,
// which may hold it at another index (a Dockerfile that gained a step, a
// build that received steps from another one).
func TestStepHistoryLinksOpenTheSameStep(t *testing.T) {
	ts, st, ids := console(t)
	// A later build of the newest image with one more step in front: every
	// step of the earlier builds sits one index further here.
	var newest *store.BuildPage
	for _, id := range ids {
		p, err := st.BuildPage(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if newest == nil || p.Build.Started.After(newest.Build.Started) {
			newest = p
		}
	}
	grown := *newest.Build
	grown.Ref, grown.Started = "grownref", grown.Started.Add(time.Hour)
	grown.Steps = append([]history.Step{{Key: "[extra 1/1]", Name: "RUN echo extra", State: history.Built}}, grown.Steps...)
	gid, err := st.SaveBuild(ctx, store.Build{Build: &grown, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	all := append([]int64{gid}, slices.Collect(maps.Values(ids))...)
	histLink := regexp.MustCompile(`<a href="(/builds/[0-9]+/steps/[0-9]+)" class="[^"]*" data-hist`)
	command := regexp.MustCompile(`<h2 id="drT"[^>]*>([^<]*)</h2>`)
	links, moved := 0, 0
	for _, id := range all {
		for i := 0; ; i++ {
			panel := get(t, ts, fmt.Sprintf("/builds/%d/steps/%d?part=panel", id, i))
			if panel.status == http.StatusNotFound {
				break
			}
			want := command.FindStringSubmatch(panel.body)
			for _, m := range histLink.FindAllStringSubmatch(panel.body, -1) {
				links++
				if !strings.HasSuffix(m[1], fmt.Sprintf("/steps/%d", i)) {
					moved++
				}
				other := get(t, ts, m[1]+"?part=panel")
				got := command.FindStringSubmatch(other.body)
				if other.status != http.StatusOK || want == nil || got == nil {
					t.Errorf("build %d step %d -> %s: %d, %q", id, i, m[1], other.status, got)
					continue
				}
				// The fixtures rename one step for the escaping checks after
				// its identity was computed; everywhere else the same
				// identity means the same command.
				if got[1] != want[1] && !strings.Contains(got[1]+want[1], "alert(1)") {
					t.Errorf("build %d step %d -> %s: %q, not %q", id, i, m[1], got[1], want[1])
				}
			}
		}
	}
	if links == 0 || moved == 0 {
		t.Errorf("%d history links, %d to another index: the fixtures do not exercise this", links, moved)
	}
}

// The console's version sits at the bottom of the sidebar on every page.
func TestVersionInTheSidebar(t *testing.T) {
	ts, _, _, _ := consoleWith(t, Options{Version: "v9.9.9 (abc123def456)"})
	for _, path := range []string{"/", "/builds", "/nodes"} {
		if p := get(t, ts, path); !strings.Contains(p.body, `<div class="side-note side-ver" title="ci-accelerator console v9.9.9 (abc123def456)">console v9.9.9 (abc123def456)</div>`) {
			t.Errorf("%s: no version in the sidebar", path)
		}
	}
}

// A step's own page has the app shell and a way back; the panel the script
// loads has neither.
func TestStepPageAndPanel(t *testing.T) {
	ts, _, _ := console(t)
	full := get(t, ts, "/builds/1/steps/0")
	panel := get(t, ts, "/builds/1/steps/0?part=panel")
	if !strings.Contains(full.body, `<aside class="side"`) || !strings.Contains(full.body, `href="/builds/1" aria-label="Back to the build"`) {
		t.Errorf("step page: %.300s", full.body)
	}
	if strings.Contains(panel.body, `<aside class="side"`) || !strings.Contains(panel.body, `data-close`) {
		t.Errorf("panel: %.300s", panel.body)
	}
}

// A build that failed: the step that failed says so (not "built"), with
// BuildKit's message in its panel; a build that failed before any step
// shows no empty steps table.
func TestFailedBuild(t *testing.T) {
	ts, st, _ := console(t)
	dirs, _ := filepath.Glob("../history/testdata/records/*")
	read := func(n string) []byte { b, _ := os.ReadFile(filepath.Join(dirs[0], n)); return b }
	rec, err := history.ParseRecord(read("record.json"))
	if err != nil {
		t.Fatal(err)
	}
	progress, err := history.DecodeProgress(bytes.NewReader(read("logs.bin")))
	if err != nil {
		t.Fatal(err)
	}
	b, err := history.NewBuild(rec, progress, nil, nil, history.Config{SecretKey: st.SecretKey()})
	if err != nil {
		t.Fatal(err)
	}
	b.Ref, b.Daemon = "failedref", "daemon-a"
	last := len(b.Steps) - 1
	b.Steps[last].Error = `process "/bin/sh -c exit 3" did not complete successfully: exit code: 3`
	b.Error = b.Steps[last].Error
	id, err := st.SaveBuild(ctx, store.Build{Build: b, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	empty := *b
	empty.Ref, empty.Steps, empty.Error = "emptyref", nil, "dockerfile parse error on line 1: unknown instruction: FROMM"
	eid, err := st.SaveBuild(ctx, store.Build{Build: &empty, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}

	p := get(t, ts, fmt.Sprintf("/builds/%d", id))
	p.safe(t)
	row := regexp.MustCompile(`(?s)<tr class="st[^"]*" data-of="\d+" data-step="` + strconv.Itoa(last) + `".*?</tr>`).FindString(p.body)
	if !strings.Contains(row, `chip failed`) || strings.Contains(row, `chip built`) {
		t.Errorf("failed step row: %s", row)
	}
	if !strings.Contains(p.body, `class="stat st-failed"`) || !strings.Contains(p.body, "The build failed.") {
		t.Error("failed build: not shown failed")
	}
	panel := get(t, ts, fmt.Sprintf("/builds/%d/steps/%d?part=panel", id, last))
	if !strings.Contains(panel.body, `class="errbox"`) || !strings.Contains(panel.body, "exit code: 3") ||
		!strings.Contains(panel.body, "Failed after") || strings.Contains(panel.body, "Dockerfile view") {
		t.Errorf("failed step panel: %s", panel.body)
	}
	// A canceled build is not a failed one.
	stopped := *b
	stopped.Ref, stopped.Error = "canceledref", "context canceled"
	stopped.Steps = append([]history.Step(nil), b.Steps...)
	stopped.Steps[last].Error = "context canceled"
	cid, err := st.SaveBuild(ctx, store.Build{Build: &stopped, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	c := get(t, ts, fmt.Sprintf("/builds/%d", cid))
	c.safe(t)
	crow := regexp.MustCompile(`(?s)<tr class="st[^"]*" data-of="\d+" data-step="` + strconv.Itoa(last) + `".*?</tr>`).FindString(c.body)
	if !strings.Contains(c.body, "The build was canceled.") || strings.Contains(c.body, "The build failed.") ||
		!strings.Contains(crow, "chip canceled") || !strings.Contains(c.body, ">Canceled<") {
		t.Errorf("canceled build shown as failed: %s", crow)
	}
	e := get(t, ts, fmt.Sprintf("/builds/%d", eid))
	e.safe(t)
	if strings.Contains(e.body, "<table><thead><tr><th class=\"num\">Line") || strings.Contains(e.body, `data-act="toggleall"`) ||
		!strings.Contains(e.body, "No step ran") {
		t.Error("a build without steps shows an empty steps table")
	}
}

// A job that ran two builds names the other on each build's page.
func TestJobWithTwoBuilds(t *testing.T) {
	ts, st, _ := console(t)
	keys, byPipe := pipelines(t, st)
	bs := byPipe[keys[0]]
	if len(bs) < 2 {
		t.Fatal("the fixtures lost the case this test needs: a pipeline whose job ran two builds")
	}
	a, err := st.BuildPage(ctx, bs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.BuildPage(ctx, bs[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	b := *second.Build
	b.Ref, b.Attribution.Job, b.Target = "secondref", a.Build.Attribution.Job, "second<i>"
	if _, err := st.SaveBuild(ctx, store.Build{Build: &b, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	pg := get(t, ts, fmt.Sprintf("/builds/%d", bs[0].ID))
	pg.safe(t)
	if !strings.Contains(pg.body, "This job ran ") || !strings.Contains(pg.body, ">second&lt;i&gt;</a>") {
		t.Error("the job's second build is not named on the page")
	}
}

// fakeGitLab answers a job's log with logBody and every other API call
// with no jobs; it records the last log asked for.
func fakeGitLab(t *testing.T, logBody string) (*gitlab.Fetcher, *string) {
	t.Helper()
	asked := new(string)
	var mu sync.Mutex
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/trace") {
			mu.Lock()
			*asked = r.URL.Path
			mu.Unlock()
			fmt.Fprint(w, logBody)
			return
		}
		w.Write([]byte("[]"))
	}))
	t.Cleanup(gl.Close)
	to, _ := url.Parse(gl.URL)
	client := gitlab.New([]string{"https://gitlab.example.com/"})
	client.HTTP = &http.Client{Transport: rewrite{to}}
	return &gitlab.Fetcher{Client: client}, asked
}

// With Assets the pages come from a directory, read again for every page:
// an edited template shows on the next request.
func TestDevAssets(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"templates", "static"} {
		entries, err := files.ReadDir(sub)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			data, _ := files.ReadFile(sub + "/" + e.Name())
			if err := os.WriteFile(filepath.Join(dir, sub, e.Name()), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	ts, _, _, _ := consoleWith(t, Options{Assets: os.DirFS(dir)})
	if p := get(t, ts, "/"); p.status != 200 || strings.Contains(p.body, "edited-on-disk") {
		t.Fatalf("before the edit: %d", p.status)
	}
	path := filepath.Join(dir, "templates", "overview.html")
	data, _ := os.ReadFile(path)
	edited := strings.Replace(string(data), "<h1 class=\"t\" tabindex=\"-1\">Overview</h1>", "<h1 class=\"t\" tabindex=\"-1\">Overview edited-on-disk</h1>", 1)
	if edited == string(data) {
		t.Fatal("the overview's title is not where the test looks for it")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := get(t, ts, "/"); !strings.Contains(p.body, "edited-on-disk") {
		t.Error("an edited template does not show without a restart")
	}
}

// The pipelines page: one card per pipeline, newest first, with its jobs
// by stage on a time axis, and how long they waited for a runner as
// GitLab says (read for the pipelines on the page, once when finished).
func TestPipelinesPage(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	jobsOf := map[string][]map[string]any{}
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		parts := strings.Split(r.URL.Path, "/") // /api/v4/projects/42/pipelines/<id>/jobs
		id := ""
		for i, p := range parts {
			if p == "pipelines" && i+1 < len(parts) {
				id = parts[i+1]
			}
		}
		out := jobsOf[id]
		if out == nil {
			out = []map[string]any{}
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer gl.Close()
	to, _ := url.Parse(gl.URL)
	client := gitlab.New([]string{"https://gitlab.example.com/"})
	client.HTTP = &http.Client{Transport: rewrite{to}}
	f := &gitlab.Fetcher{Client: client}
	ts, st, _, _ := consoleWith(t, Options{API: f, Token: gitlab.Token("t")})
	f.Store = st
	keys, byPipe := pipelines(t, st)
	mu.Lock()
	for _, k := range keys {
		_, id, _ := strings.Cut(k, "/")
		for _, b := range byPipe[k] {
			jobsOf[id] = append(jobsOf[id], map[string]any{"id": b.Job, "name": "build " + b.Target, "stage": "build", "status": "success",
				"started_at": b.Started.Add(-2 * time.Second).Format(time.RFC3339Nano), "finished_at": b.Completed.Add(time.Second).Format(time.RFC3339Nano),
				"duration": 9.0, "queued_duration": 4.0, "pipeline": map[string]any{"status": "success", "web_url": "https://gitlab.example.com/p/-/pipelines/" + id}})
		}
	}
	mu.Unlock()
	p := get(t, ts, "/pipelines")
	p.safe(t)
	if p.status != 200 || strings.Count(p.body, `<article class="pcard">`) != min(len(keys), pipePage) {
		t.Fatalf("pipelines page: %d, %d cards for %d pipelines", p.status, strings.Count(p.body, `<article class="pcard">`), len(keys))
	}
	if !strings.Contains(p.body, "waiting for a runner: usually <b>4.0 s</b>") || !strings.Contains(p.body, `class="q r0"`) {
		t.Error("no runner waits from GitLab on the cards")
	}
	if !strings.Contains(p.body, `class="pg-lane`) || !strings.Contains(p.body, ">build</span>") || !strings.Contains(p.body, "Passed") {
		t.Error("no lanes by stage, or no pipeline status")
	}
	if !strings.Contains(p.body, `href="/pipelines/`+keys[0]+`"`) {
		t.Error("a card does not open its pipeline")
	}
	mu.Lock()
	first := calls
	mu.Unlock()
	get(t, ts, "/pipelines")
	mu.Lock()
	if calls != first {
		t.Errorf("finished pipelines read from GitLab again: %d calls, then %d", first, calls)
	}
	mu.Unlock()
	pp := get(t, ts, "/pipelines/"+keys[0])
	if !strings.Contains(pp.body, "waited 4.0 s") || !strings.Contains(pp.body, `class="pstate ps-passed"`) {
		t.Error("the pipeline page does not show the runner wait or the pipeline's status")
	}
}

// Small things the pre-release review found: no directory listing of the
// static files, the project switcher stays in its section, and the search
// knows every section.
func TestReviewFixes(t *testing.T) {
	ts, _, _ := console(t)
	if p := get(t, ts, "/static/"); p.status != http.StatusNotFound {
		t.Errorf("/static/ answered %d: a listing of the static files", p.status)
	}
	if p := get(t, ts, "/pipelines"); !regexp.MustCompile(`href="/projects/[0-9a-f]+/\d+\?to=(/|%2f)pipelines"`).MatchString(p.body) {
		t.Error("the project switcher on the pipelines page leaves the section")
	}
	m := regexp.MustCompile(`href="(/projects/[0-9a-f]+/\d+)\?to=`).FindStringSubmatch(get(t, ts, "/pipelines").body)
	if m != nil {
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := c.Get(ts.URL + m[1] + "?to=%2fpipelines")
		if err != nil || res.Header.Get("Location") != "/pipelines" {
			t.Errorf("switching project from the pipelines page: %v %v", err, res.Header.Get("Location"))
		} else {
			res.Body.Close()
		}
	}
	if p := get(t, ts, "/"); strings.Contains(p.body, `?to=`) {
		t.Error("the project switcher on the overview names a page")
	}
	idx := get(t, ts, "/search-index")
	if !strings.Contains(idx.body, `"url":"/pipelines"`) {
		t.Errorf("the search lacks the pipelines section: %.200s", idx.body)
	}
}
