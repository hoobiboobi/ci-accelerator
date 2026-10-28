package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
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
	"github.com/hoobiboobi/ci-accelerator/internal/diskprobe"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/route"
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
	return consoleWith(t, testOpts{})
}

func consoleWith(t *testing.T, opt testOpts) (*httptest.Server, *store.Store, map[string]int64, *Server) {
	t.Helper()
	return consoleBuild(t, func(*store.Store) testOpts { return opt }, httptest.NewServer)
}

// consoleBuild is consoleWith for options that need the store (sign-in),
// served by start (plain or TLS).
func consoleBuild(t *testing.T, options func(*store.Store) testOpts, start func(http.Handler) *httptest.Server) (*httptest.Server, *store.Store, map[string]int64, *Server) {
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
	// It also breaks one of BuildKit's build checks (on line 1).
	last.Warnings = []history.Warning{{Rule: "JSONArgsRecommended", Message: "JSON arguments recommended for CMD " + evil,
		Detail: "signals", URL: "https://docs.docker.com/go/dockerfile/rule/json-args-recommended/", File: "Dockerfile", Line: 1, EndLine: 1}}
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
	backend, opt := options(st).wire(t, st)
	srv, err := New(backend, slog.New(slog.NewTextHandler(logTo, nil)), opt)
	if err != nil {
		t.Fatal(err)
	}
	// The pages' windows (the last 24 hours, 7 days) end when the
	// recorded builds were made.
	srv.now = func() time.Time { return testNow }
	mux := http.NewServeMux()
	srv.Register(mux)
	ok := func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /livez", ok)
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
					t.Fatalf("panel %d: %d, %.80q", i, panel.status, panel.body)
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
	ts, st, _, _ := consoleWith(t, testOpts{API: f, Token: gitlab.Token("t")})
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
	// The part says where it sits, so the page can ask for the lines before
	// it or added since; a short log is whole: it starts at 0.
	size := strconv.Itoa(len("Running with gitlab-runner\n$ make test\ntoken glpat-AbCdEfGhIjKlMnOpQrSt12 used\nok\n"))
	if lg.header.Get("X-Log-Start") != "0" || lg.header.Get("X-Log-End") != size || lg.header.Get("X-Log-Size") != size || lg.header.Get("X-Log-Gap") != "" {
		t.Errorf("log part headers: %v", lg.header)
	}
	if again := get(t, ts, fmt.Sprintf("/jobs/%s/9100002/log?after=%s", inst, size)); again.status != 200 || again.body != "" || again.header.Get("X-Log-End") != size {
		t.Errorf("nothing new after the end: %d %q %v", again.status, again.body, again.header)
	}
	if two := get(t, ts, fmt.Sprintf("/jobs/%s/9100002/log?lines=2", inst)); two.body != "token [masked] used\nok\n" || two.header.Get("X-Log-Start") == "0" {
		t.Errorf("the last 2 lines: %q from %s", two.body, two.header.Get("X-Log-Start"))
	}
	if bad := get(t, ts, fmt.Sprintf("/jobs/%s/9100002/log?before=x", inst)); bad.status != http.StatusBadRequest {
		t.Errorf("a bad part: %d", bad.status)
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
	ts, st, _, _ := consoleWith(t, testOpts{Live: daemons, Release: "box"})
	var ds []store.Decision
	for i, r := range []route.Reason{route.LocalWarm, route.ZoneWarm, route.ZoneHash} {
		ds = append(ds, store.Decision{Project: route.Project{Instance: "abcdef", ID: 42}, JobID: uint64(900 + i), Node: "node-1",
			Daemon: "box-buildkit-abcde", Reason: r, Cands: 2, JobNode: "node-1", At: testNow.Add(-time.Hour)})
	}
	old := ds[0]
	old.JobID, old.At = 899, testNow.Add(-25*time.Hour) // outside the day
	if err := st.SaveDecisions(ctx, append(ds, old)); err != nil {
		t.Fatal(err)
	}
	nodes := get(t, ts, "/nodes")
	nodes.safe(t)
	for _, want := range []string{"1 ready of 2", "on node-1", "300 MiB", "/root/.cache/pip cache mount", "Duplicate cache",
		"python:3.13-slim", "Keep cache up to", "40.0 GiB per node", "14 days after last use", "Running now", "Unreachable", "release <code>box</code>",
		"replaced since", "<span>Sent here by the build router, 24 hours</span><span>3 — 2 to the project&#39;s cache, 1 by hash: no cache held the project</span>",
		"<span>Sent here by the build router, 24 hours</span><span>0</span>",
		"The build router, 24 hours: 3 build connections, 2 to a node whose cache held the project&#39;s layers (66%)."} {
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
	ts, st, _, srv := consoleWith(t, testOpts{API: f, Token: gitlab.Token("t")})
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
	ts, st, _, _ := consoleWith(t, testOpts{Live: live{{Name: "daemon-a", Connected: true}}})
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

// The Dockerfile build checks BuildKit reported: listed on the build page,
// and marked on their lines of the Dockerfile, each linking its rule.
func TestBuildCheckWarningsShown(t *testing.T) {
	ts, st, _ := console(t)
	recent, err := st.Recent(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	with, without := int64(0), int64(0)
	for _, b := range recent {
		if len(b.Build.Warnings) > 0 {
			with = b.ID
		} else if without == 0 {
			without = b.ID
		}
	}
	if with == 0 || without == 0 {
		t.Fatalf("fixtures: build with warnings %d, without %d", with, without)
	}
	page := get(t, ts, fmt.Sprintf("/builds/%d", with))
	page.safe(t)
	for _, want := range []string{"Dockerfile checks", ">JSONArgsRecommended</a>",
		`class="wtag" href="https://docs.docker.com/go/dockerfile/rule/json-args-recommended/"`} {
		if !strings.Contains(page.body, want) {
			t.Errorf("the build page lacks %q", want)
		}
	}
	if page := get(t, ts, fmt.Sprintf("/builds/%d", without)); strings.Contains(page.body, "Dockerfile checks") {
		t.Errorf("a build without warnings shows the checks section")
	}
}

// A build the router sent to its daemon says why there.
func TestRoutedBuild(t *testing.T) {
	ts, st, _ := console(t)
	keys, byPipe := pipelines(t, st)
	row := byPipe[keys[0]][0]
	p, err := st.BuildPage(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	b := p.Build
	a := b.Attribution
	if p.Route != nil {
		t.Fatalf("a route without a decision: %+v", p.Route)
	}
	d := store.Decision{Project: route.Project{Instance: a.Instance, ID: 42}, JobID: a.Job, Node: "node-2", Daemon: b.Daemon,
		Reason: route.ZoneWarm, Cands: 3, JobNode: "node-1", At: b.Started.Add(-time.Second)}
	other := d
	other.Daemon, other.Node = "another-daemon", "node-3"
	if err := st.SaveDecisions(ctx, []store.Decision{d, other}); err != nil {
		t.Fatal(err)
	}
	page := get(t, ts, fmt.Sprintf("/builds/%d", row.ID))
	page.safe(t)
	if !strings.Contains(page.body, "node-2, whose cache holds the project&#39;s most recently used layers (the job ran on node-1)") ||
		strings.Contains(page.body, "node-3") {
		t.Errorf("the build page does not say why the router chose its node")
	}
	// The Nodes page counts them, the page as a whole and per daemon.
	nodes := get(t, ts, "/nodes")
	nodes.safe(t)
	if !strings.Contains(nodes.body, "The build router, 24 hours: 2 build connections, 2 to a node whose cache held the project&#39;s layers (100%).") {
		t.Errorf("the Nodes page does not sum up the router's decisions")
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
			if panel.status != http.StatusOK {
				t.Fatalf("build %d step %d: %d", id, i, panel.status)
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
	ts, _, _, _ := consoleWith(t, testOpts{Version: "v9.9.9 (abc123def456)"})
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
	ts, _, _, _ := consoleWith(t, testOpts{Assets: os.DirFS(dir)})
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
	ts, st, _, _ := consoleWith(t, testOpts{API: f, Token: gitlab.Token("t")})
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

// The pipelines page finds pipelines by number, commit SHA, title, branch,
// author and job name, narrows them to a period, and keeps both in its
// page links.
func TestPipelinesSearch(t *testing.T) {
	ts, st, _ := console(t)
	inst := history.InstancePrefix("https://gitlab.example.com/")
	for _, j := range []store.Job{
		{JobID: 7001, PipelineID: 900001, Name: "lint-frontend", Ref: "feature/search", SHA: "abcdef1234567890abcdef1234567890abcdef12",
			CommitTitle: "Add a search box", CommitAuthor: "Alice Example <alice@example.com>", PipelineCreatedAt: testNow.Add(-2 * time.Hour)},
		{JobID: 7002, PipelineID: 900002, Name: "build-api", Ref: "main", SHA: "1234567deadbeef1234567deadbeef1234567dea",
			CommitTitle: "Bump 100% of deps_x", CommitAuthor: "Bob <bob@example.com>", PipelineCreatedAt: testNow.Add(-10 * 24 * time.Hour)},
	} {
		j.Instance, j.ProjectID, j.ProjectPath = inst, 42, "example/app"
		if err := st.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	a, b := `href="/pipelines/`+inst+`/900001"`, `href="/pipelines/`+inst+`/900002"`
	cards := func(p page) int { return strings.Count(p.body, `<article class="pcard">`) }
	find := func(q, extra string) page {
		t.Helper()
		p := get(t, ts, "/pipelines?q="+url.QueryEscape(q)+extra)
		p.safe(t)
		if p.status != 200 {
			t.Fatalf("search %q: %d", q, p.status)
		}
		return p
	}
	for q, want := range map[string]string{
		"900001": a, "#900002": b, "#9000": "", "abcdef1": a, "ABCDEF1": a, "1234567dead": b,
		"search box": a, "SEARCH": a, "feature/": a, "alice": a, "lint-front": a, "build-api": b, "100%": b, "deps_x": b,
		// LIKE's wildcards match only themselves.
		"%": b, "_": b,
	} {
		p := find(q, "")
		if want == "" { // both
			if !strings.Contains(p.body, a) || !strings.Contains(p.body, b) {
				t.Errorf("search %q: not both pipelines", q)
			}
			continue
		}
		other := a
		if want == a {
			other = b
		}
		if !strings.Contains(p.body, want) || strings.Contains(p.body, other) {
			t.Errorf("search %q: not just the expected pipeline", q)
		}
		if !strings.Contains(p.body, `value="`+html.EscapeString(q)+`"`) {
			t.Errorf("search %q: the box does not keep it", q)
		}
	}
	for _, q := range []string{"zz%zz", "a_b_c_d", "no such pipeline anywhere"} {
		if p := find(q, ""); cards(p) != 0 || !strings.Contains(p.body, "No pipeline matches.") {
			t.Errorf("search %q: %d cards, want none and the empty note", q, cards(p))
		}
	}
	if p := find("", "&period=24h"); !strings.Contains(p.body, a) || strings.Contains(p.body, b) {
		t.Error("last 24 hours: wrong pipelines")
	}
	if p := find("", "&period=30d"); !strings.Contains(p.body, a) || !strings.Contains(p.body, b) {
		t.Error("last 30 days: wrong pipelines")
	}
	day := func(d time.Duration) string { return testNow.Add(d).Format(time.DateOnly) }
	if p := find("", "&period=custom&from="+day(-11*24*time.Hour)+"&to="+day(-10*24*time.Hour)); strings.Contains(p.body, a) || !strings.Contains(p.body, b) {
		t.Error("a custom period: wrong pipelines (the To day is included)")
	}
	if p := find("", "&period=custom&from=2026-10-21&to=2026-10-19"); !strings.Contains(p.body, "ends before it starts") {
		t.Error("a period that ends before it starts is not explained")
	}
	if p := find("", "&period=custom&from=2000-01-01&to=2000-01-02"); cards(p) != 0 || !strings.Contains(p.body, `id="pFrom" value="2000-01-01"`) {
		t.Errorf("a period with no pipelines: %d cards, or no date fields", cards(p))
	}
	all := cards(get(t, ts, "/pipelines"))
	if p := find("", "&period=custom&from=yesterday"); !strings.Contains(p.body, `aria-invalid="true"`) || cards(p) != all {
		t.Error("a date that cannot be read is not flagged, or it filters")
	}
	if p := find("", "&period=bogus"); cards(p) != all {
		t.Errorf("an unknown period: %d cards, want %d", cards(p), all)
	}
	// Further pages keep the search and the period.
	v := &PipelinesView{Q: "a b", Period: "custom", From: "2026-10-01", Branch: "main"}
	link := pipeLink(v, url.Values{"before": {"1"}})
	for _, part := range []string{"q=a+b", "period=custom", "from=2026-10-01", "branch=main", "before=1"} {
		if !strings.Contains(link, part) {
			t.Errorf("page link %s lacks %s", link, part)
		}
	}
}

// The live indicator arrives in its final state: "Live" where the page
// updates itself, an invisible line of the same height elsewhere; Nodes
// subscribes to the daemons' state.
func TestLiveIndicator(t *testing.T) {
	ts, st, _ := console(t)
	keys, _ := pipelines(t, st)
	for path, want := range map[string]string{
		"/pipelines":            `<p class="side-live" id="live" role="status">Live</p>`,
		"/pipelines/" + keys[0]: `<p class="side-live" id="live" role="status">Live</p>`,
		"/nodes":                `<p class="side-live" id="live" role="status">Live</p>`,
	} {
		if p := get(t, ts, path); !strings.Contains(p.body, want) {
			t.Errorf("%s: no live indicator in its final state", path)
		}
	}
	if p := get(t, ts, "/nodes"); !strings.Contains(p.body, `data-live-nodes="true"`) {
		t.Error("Nodes does not ask for the daemons' live state")
	}
}

// An image's address names no project: opened while another project is
// chosen, it switches to the project that builds it; switching may lead
// only to the console's own pages.
func TestImageOfAnotherProject(t *testing.T) {
	ts, st, _ := console(t)
	inst := history.InstancePrefix("https://gitlab.example.com/")
	// Project 77 builds an image of its own.
	keys, byPipe := pipelines(t, st)
	b, err := st.BuildPage(ctx, byPipe[keys[0]][0].ID)
	if err != nil {
		t.Fatal(err)
	}
	x := *b.Build
	x.Target, x.Ref = "only-in-77", "other-1"
	x.Attribution = history.Attribution{State: history.Attributed, Instance: inst, Pipeline: 910001, Job: 7101}
	x.Started = testNow.Add(-time.Hour)
	x.Completed = x.Started.Add(3 * time.Second)
	if _, err := st.SaveBuild(ctx, store.Build{Build: &x, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveJob(ctx, store.Job{Instance: inst, JobID: 7101, PipelineID: 910001, ProjectID: 77, ProjectPath: "example/other", Name: "lint"}); err != nil {
		t.Fatal(err)
	}
	day := testNow.Add(-30 * 24 * time.Hour)
	of77, err := st.Targets(ctx, store.ProjectKey{Instance: inst, ID: 77}, day)
	if err != nil || len(of77) == 0 {
		t.Fatalf("project 77: targets %v, %v", of77, err)
	}
	of42, err := st.Targets(ctx, store.ProjectKey{Instance: inst, ID: 42}, day)
	if err != nil {
		t.Fatal(err)
	}
	only42 := ""
	for _, x := range of42 {
		if !slices.Contains(of77, x) {
			only42 = x
		}
	}
	if only42 == "" {
		t.Fatalf("fixtures: no image of project 42 alone (42 %v, 77 %v)", of42, of77)
	}
	img := "/images/" + url.PathEscape(imageKey(only42))
	c := ts.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, _ := http.NewRequest("GET", ts.URL+img, nil)
	req.AddCookie(&http.Cookie{Name: "project", Value: inst + "/77"})
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	want := "/projects/" + inst + "/42?to=" + url.QueryEscape(img)
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != want {
		t.Fatalf("image of project 42 seen from project 77: %d %q, want a switch to %q", res.StatusCode, res.Header.Get("Location"), want)
	}
	if p := get(t, ts, want); p.status != http.StatusFound || p.header.Get("Location") != img {
		t.Errorf("switching leads to %d %q, want the image", p.status, p.header.Get("Location"))
	}
	for _, to := range []string{"//evil.example/images/x", "https://evil.example/", "/images/x/../../nope", "/images/", "/builds/1"} {
		if p := get(t, ts, "/projects/"+inst+"/42?to="+url.QueryEscape(to)); p.header.Get("Location") != "/" {
			t.Errorf("to=%q leads to %q, want /", to, p.header.Get("Location"))
		}
	}
	if p := get(t, ts, "/images/no-such-image"); p.status != http.StatusNotFound || !strings.Contains(p.body, ">Overview</a>") {
		t.Errorf("an unknown image: %d", p.status)
	}
}

// A daemon's disk on the Nodes page: what fills it, and warnings against
// the GC's keep-free floor and the kubelet's eviction thresholds; a nearly
// full disk lights the Nodes item.
func TestNodesDisk(t *testing.T) {
	gc := []collector.GCRule{{All: true, MaxUsed: 80 << 30, MinFree: 30 << 30}}
	disk := func(free, avail uint64) *diskprobe.Disk {
		return &diskprobe.Disk{Total: 100 << 30, Free: free << 30, Available: avail << 30, Inodes: 1000, InodesFree: 900}
	}
	for _, c := range []struct {
		name       string
		d          *diskprobe.Disk
		level      string
		notes, err int
	}{
		{"roomy", disk(60, 55), "", 0, 0},
		{"under the GC floor", disk(25, 20), "warn", 1, 0},
		{"near eviction", disk(18, 12), "warn", 2, 0},
		{"evicting", disk(9, 6), "crit", 2, 0},
	} {
		v := diskView(collector.Inspection{GC: gc, Usage: collector.Usage{Total: 30 << 30}, Disk: c.d})
		if v == nil || v.Level != c.level || len(v.Notes) != c.notes {
			t.Errorf("%s: %+v", c.name, v)
		}
	}
	if v := diskView(collector.Inspection{}); v != nil {
		t.Error("a disk without a probe")
	}
	if v := diskView(collector.Inspection{DiskErr: "timeout"}); v == nil || v.Err == "" {
		t.Error("a probe that did not answer is not shown")
	}
	// The cache as BuildKit counts it never shows more than the used space.
	if v := diskView(collector.Inspection{Usage: collector.Usage{Total: 500 << 30}, Disk: disk(60, 55)}); v.Segs[0].Size != bytesText(40<<30) {
		t.Errorf("cache over the used space: %+v", v.Segs)
	}

	full := live{{Name: "daemon-a", Connected: true, InspectedAt: testNow, Inspection: collector.Inspection{GC: gc, Disk: disk(9, 6)}},
		{Name: "daemon-b", Connected: true, InspectedAt: testNow, Inspection: collector.Inspection{GC: gc, Disk: disk(60, 55)}}}
	ts, _, _, _ := consoleWith(t, testOpts{Live: full})
	p := get(t, ts, "/nodes")
	p.safe(t)
	if !strings.Contains(p.body, "The disk is nearly full") || !strings.Contains(p.body, `class="callout bad"`) || strings.Count(p.body, `class="ndisk"`) != 2 {
		t.Error("Nodes does not show the disks and the full one")
	}
	if !strings.Contains(get(t, ts, "/pipelines").body, `aria-label="A build node&#39;s disk is nearly full"`) {
		t.Error("a nearly full disk does not light the Nodes item")
	}
}

// The server renders the folded tree, so the first paint is final: a row
// under a folded parent carries hidden, every other row does not. Folding
// in the script after load showed every step for a moment.
func TestFoldedRowsAreHiddenByTheServer(t *testing.T) {
	ts, st, _ := console(t)
	keys, byPipe := pipelines(t, st)
	rowRe := regexp.MustCompile(`<div class="row [^"]*"[^>]*data-id="([^"]*)" data-parent="([^"]*)"[^>]*>`)
	checkedWF, checkedSteps := 0, 0
	for _, k := range keys {
		p := get(t, ts, "/pipelines/"+k)
		open := map[string]bool{"": true}
		visible := map[string]bool{"": true}
		for _, m := range rowRe.FindAllStringSubmatch(p.body, -1) {
			tag, id, parent := m[0], m[1], m[2]
			want := visible[parent] && open[parent]
			got := !strings.Contains(tag, " hidden")
			if got != want {
				t.Errorf("%s row %s (parent %s): visible %v, want %v", k, id, parent, got, want)
			}
			visible[id] = got
			open[id] = !strings.Contains(tag, `aria-expanded="false"`)
			checkedWF++
		}
		for _, b := range byPipe[k] {
			bp := get(t, ts, fmt.Sprintf("/builds/%d", b.ID))
			folded := map[string]bool{}
			for _, m := range regexp.MustCompile(`data-blk="(\d+)" data-fold="(true|false)" aria-expanded="(true|false)"`).FindAllStringSubmatch(bp.body, -1) {
				if (m[2] == "true") != (m[3] == "false") {
					t.Errorf("build %d block %s: fold %s but aria-expanded %s", b.ID, m[1], m[2], m[3])
				}
				folded[m[1]] = m[2] == "true"
			}
			for _, m := range regexp.MustCompile(`<tr class="st[^"]*" data-of="(\d+)"[^>]*>`).FindAllStringSubmatch(bp.body, -1) {
				if hidden := strings.Contains(m[0], " hidden"); hidden != folded[m[1]] {
					t.Errorf("build %d: a step of block %s hidden=%v, folded=%v", b.ID, m[1], hidden, folded[m[1]])
				}
				checkedSteps++
			}
		}
	}
	if checkedWF == 0 || checkedSteps == 0 {
		t.Fatalf("checked %d timeline rows and %d step rows", checkedWF, checkedSteps)
	}
}

// Two builds of one image side by side: facts, arguments, the Dockerfile's
// differing lines and every step by its place.
func TestCompareBuilds(t *testing.T) {
	ts, st, _ := console(t)
	keys, byPipe := pipelines(t, st)
	byTarget := map[string][]store.BuildRow{}
	for _, k := range keys {
		for _, b := range byPipe[k] {
			byTarget[b.Target] = append(byTarget[b.Target], b)
		}
	}
	var a, b store.BuildRow
	for _, rows := range byTarget {
		if len(rows) >= 2 {
			a, b = rows[0], rows[1]
			break
		}
	}
	if a.ID == 0 {
		t.Fatal("fixtures: no image with two builds")
	}
	page := get(t, ts, fmt.Sprintf("/builds/%d/compare?with=%d", a.ID, b.ID))
	page.safe(t)
	for _, want := range []string{"Compare two builds of", "<h2 class=\"h\" id=\"cfT\">The builds</h2>", "Steps run", "id=\"csT\">Steps</h2>",
		fmt.Sprintf(`href="/builds/%d"`, a.ID), fmt.Sprintf(`href="/builds/%d"`, b.ID)} {
		if !strings.Contains(page.body, want) {
			t.Errorf("compare page lacks %q", want)
		}
	}
	// Without ?with: the previous build, and a list to pick from.
	if p := get(t, ts, fmt.Sprintf("/builds/%d/compare", b.ID)); !strings.Contains(p.body, "Compare with another build of") {
		t.Errorf("no builds to pick from")
	}
	// Builds of different images are not compared.
	for tgt, rows := range byTarget {
		if tgt != a.Target {
			if p := get(t, ts, fmt.Sprintf("/builds/%d/compare?with=%d", a.ID, rows[0].ID)); p.status != 400 || strings.Contains(p.body, "id=\"csT\"") {
				t.Errorf("compared builds of %s and %s: %d", a.Target, tgt, p.status)
			}
			break
		}
	}
	offered := false
	for _, x := range []store.BuildRow{a, b} {
		offered = offered || strings.Contains(get(t, ts, fmt.Sprintf("/builds/%d", x.ID)).body, fmt.Sprintf(`href="/builds/%d/compare"`, x.ID))
	}
	if !offered {
		t.Errorf("the build page does not offer the comparison")
	}
}

func TestLineDiff(t *testing.T) {
	a := "FROM a\nRUN 1\nRUN 2\nRUN 3\nRUN 4\nRUN 5\nRUN 6\nCOPY x\n"
	b := "FROM a\nRUN 1\nRUN 2\nRUN 3\nRUN 4\nRUN 5\nRUN 6\nCOPY y\nRUN 7\n"
	var got []string
	for _, l := range lineDiff(a, b, 1) {
		s := l.Op + l.Text
		if l.Gap {
			s = "…" + s
		}
		got = append(got, s)
	}
	if want := []string{" RUN 6", "-COPY x", "+COPY y", "+RUN 7"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("diff %q", got)
	}
	if lineDiff(a, a, 2) != nil {
		t.Error("a diff of equal texts")
	}
	if d := lineDiff("A\nB\nC\nD\nE\nF\nG\n", "A2\nB\nC\nD\nE\nF\nG2\n", 1); len(d) != 6 || !d[3].Gap {
		t.Errorf("two changes far apart: %+v", d)
	}
}

// A p95 needs 20 rebuilds: with fewer, the nearest-rank p95 is the slowest
// build and "the slowest 5%" is less than one build.
func TestNoP95FromFewRebuilds(t *testing.T) {
	ts, _, _ := console(t)
	imgs := get(t, ts, "/images?window=7d")
	if !strings.Contains(imgs.body, "95% of rebuilds within") || !strings.Contains(imgs.body, "a p95 needs 20") {
		t.Errorf("the images table shows a p95 from fewer than 20 rebuilds")
	}
	for _, h := range regexp.MustCompile(`href="(/images/[^"?]+)"`).FindAllStringSubmatch(imgs.body, -1) {
		if p := get(t, ts, h[1]+"?window=7d"); strings.Contains(p.body, "95% of rebuilds took at most") {
			t.Errorf("%s: a p95 from the fixtures' few rebuilds", h[1])
		}
	}
}

// A pipeline's duration is one number: its card and its timeline count
// from when GitLab created it to its last job's end.
func TestPipelineDurationIsOneNumber(t *testing.T) {
	ts, st, _ := console(t)
	keys, byPipe := pipelines(t, st)
	// GitLab created one pipeline well before its first job waited.
	inst, id, _ := strings.Cut(keys[0], "/")
	pid, _ := strconv.ParseUint(id, 10, 64)
	early := byPipe[keys[0]][len(byPipe[keys[0]])-1].Started.Add(-47 * time.Second)
	for _, b := range byPipe[keys[0]] {
		if b.Started.Add(-47 * time.Second).Before(early) {
			early = b.Started.Add(-47 * time.Second)
		}
	}
	if err := st.SaveJob(ctx, store.Job{Instance: inst, JobID: 7001, PipelineID: pid, ProjectID: 42, Name: "late", PipelineCreatedAt: early}); err != nil {
		t.Fatal(err)
	}
	card := regexp.MustCompile(`job(?:s)? in <b>([^<]+)</b>`)
	row := regexp.MustCompile(`>([^<>]+) · \d+ jobs<`)
	compared := 0
	for _, k := range keys {
		p := get(t, ts, "/pipelines/"+k)
		c, w := card.FindStringSubmatch(p.body), row.FindStringSubmatch(p.body)
		if c == nil || w == nil {
			continue
		}
		compared++
		if c[1] != w[1] {
			t.Errorf("%s: the card says %q, the timeline %q", k, c[1], w[1])
		}
	}
	if compared == 0 {
		t.Fatal("no pipeline with a card and a timeline")
	}
}

// A build page compares a build with its image's usual time only when
// that is the median of at least five builds of its kind.
func TestNoUsualFromAFewBuilds(t *testing.T) {
	ts, st, _ := console(t)
	keys, byPipe := pipelines(t, st)
	n := map[string]int{}
	for _, k := range keys {
		for _, b := range byPipe[k] {
			n[b.Target]++
		}
	}
	for _, k := range keys {
		for _, b := range byPipe[k] {
			if n[b.Target] < minForUsual {
				if p := get(t, ts, fmt.Sprintf("/builds/%d", b.ID)); strings.Contains(p.body, "vs usual") || strings.Contains(p.body, ">usual ") {
					t.Errorf("build %d of %s (%d builds): a usual time", b.ID, b.Target, n[b.Target])
				}
			}
		}
	}
}
