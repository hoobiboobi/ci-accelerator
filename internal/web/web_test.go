package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	srv, err := New(st, slog.New(slog.NewTextHandler(logTo, nil)), opt)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(Secure(mux))
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

// Every page reachable from the home page renders, is safe, and the data
// with markup shows up as text.
func TestPagesRenderAndEscape(t *testing.T) {
	ts, _, _ := console(t)
	seen := map[string]bool{}
	queue := []string{"/"}
	kinds := map[string]int{}
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		if seen[path] {
			continue
		}
		seen[path] = true
		p := get(t, ts, path)
		if p.status == http.StatusFound {
			queue = append(queue, p.header.Get("Location"))
			continue
		}
		for _, m := range stepSpan.FindAllStringSubmatch(p.body, -1) {
			if m[1] == m[2] {
				t.Errorf("%s: a step's time range with equal ends: %s", path, m[0])
			}
		}
		if p.status != http.StatusOK {
			t.Errorf("%s: %d %s", path, p.status, p.body)
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
	if kinds[""] != 1 || kinds["pipelines"] < 8 || kinds["builds"] < 13 {
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
		"/pipelines/abcdef/1", "/pipelines/abcdef/x", "/jobs/abcdef/77", "/jobs/abcdef/x", "/nope",
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

// A job's address leads to its last build.
func TestJobRedirect(t *testing.T) {
	ts, st, ids := console(t)
	ps, err := st.Pipelines(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	_, jobs, builds, err := st.PipelinePage(ctx, ps[0].Instance, ps[0].ID)
	if err != nil || len(jobs) == 0 {
		t.Fatal(jobs, err)
	}
	var want int64
	for _, b := range builds {
		if b.Build.Attribution.Job == jobs[0].ID && b.ID > want {
			want = b.ID
		}
	}
	p := get(t, ts, fmt.Sprintf("/jobs/%s/%d", ps[0].Instance, jobs[0].ID))
	if p.status != http.StatusFound || p.header.Get("Location") != fmt.Sprintf("/builds/%d?via=log", want) {
		t.Errorf("job redirect: %d to %q, want build %d (%d builds stored)", p.status, p.header.Get("Location"), want, len(ids))
	}
	// Arriving from the job log, the page says how, with the line.
	via := get(t, ts, p.header.Get("Location"))
	if !strings.Contains(via.body, "How you got here") || !strings.Contains(via.body, fmt.Sprintf("/jobs/%s/%d", ps[0].Instance, jobs[0].ID)) {
		t.Errorf("no way back to the job log")
	}
	if strings.Contains(get(t, ts, fmt.Sprintf("/builds/%d", want)).body, "How you got here") {
		t.Errorf("the job-log banner shows without the job log")
	}
}

// The pipeline page's Dockerfile has one cell per job on every line, so a
// job that did not run a line leaves its cell empty instead of shifting
// the next job's result into its column.
func TestPipelineDockerfileColumns(t *testing.T) {
	ts, st, _ := console(t)
	ps, err := st.Pipelines(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, pl := range ps {
		if pl.Builds < 2 {
			continue
		}
		p := get(t, ts, fmt.Sprintf("/pipelines/%s/%d", pl.Instance, pl.ID))
		cols := strings.Count(p.body, `<span class="jh"`)
		rows := regexp.MustCompile(`(?s)<span class="dr">(.*?)</span>\s*</div>`).FindAllStringSubmatch(p.body, -1)
		for _, r := range rows[1:] {
			if n := strings.Count(r[1], `class="jc`); n != cols {
				t.Errorf("pipeline %d: a line has %d cells for %d jobs: %s", pl.ID, n, cols, r[1])
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no pipeline with several builds")
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

// A live page's stream says when its pipeline changed, and only its own.
func TestEvents(t *testing.T) {
	ts, st, _, srv := consoleServer(t)
	srv.Heartbeat = 50 * time.Millisecond
	ps, err := st.Pipelines(ctx, 100)
	if err != nil || len(ps) < 2 {
		t.Fatal(ps, err)
	}
	mine, other := ps[0], ps[1]
	key := store.PipelineKey(mine.Instance, mine.ID)
	home := get(t, ts, "/")
	pipe := get(t, ts, "/pipelines/"+key)
	if !strings.Contains(home.body, `data-live="all"`) || !strings.Contains(pipe.body, `data-live="`+key+`"`) {
		t.Fatalf("live markers missing")
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(sctx, "GET", ts.URL+"/events?pipeline="+url.QueryEscape(key), nil)
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
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
	next := func(skipPings bool) string {
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
	}
	if l := next(false); l != ": ping" {
		t.Errorf("first line %q, want a heartbeat", l)
	}
	job := func(p store.Pipeline, name string) {
		if err := st.SaveJob(ctx, store.Job{Instance: p.Instance, JobID: 9_000_000 + p.ID, PipelineID: p.ID, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	job(other, "elsewhere")
	// The daemons' live state: not for a pipeline's page.
	st.Announce("")
	job(mine, "here")
	if l := next(true); l != "event: changed" {
		t.Errorf("got %q", l)
	}
	if l := next(true); l != "data: "+key {
		t.Errorf("got %q, want only the page's pipeline", l)
	}

	// The nodes page asks for the daemons' state.
	nodesPage := get(t, ts, "/nodes")
	if !strings.Contains(nodesPage.body, `data-live-nodes="true"`) || strings.Contains(pipe.body, `data-live-nodes`) {
		t.Errorf("live-nodes marker on the wrong pages")
	}
	req2, _ := http.NewRequestWithContext(sctx, "GET", ts.URL+"/events?nodes=1", nil)
	res2, err := ts.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	sc := bufio.NewScanner(res2.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "retry:") {
			break
		}
	}
	st.Announce("")
	got := ""
	for sc.Scan() {
		if l := sc.Text(); strings.HasPrefix(l, "data: ") {
			got = l
			break
		}
	}
	if got != "data: live" {
		t.Errorf("nodes stream got %q", got)
	}
}

type running []store.Job

func (r running) Jobs() []store.Job { return r }

type live []collector.DaemonState

func (l live) Daemons() []collector.DaemonState { return l }

// Jobs in progress show in the list before they have a build, with a page
// of their own; the nodes page shows each daemon's cache and running
// builds; GitLab's job times split a build's job.
func TestLiveSources(t *testing.T) {
	inst := history.InstancePrefix("https://gitlab.example.com/")
	started := time.Now().Add(-42 * time.Second)
	rj := store.Job{Instance: inst, JobID: 777001, PipelineID: 999, ProjectID: 42, Name: "deploy" + evil, StartedAt: started,
		CommitTitle: "running " + evil, Node: "node-1"}
	daemons := live{{Name: "box-buildkit-abcde", Connected: true, InspectedAt: time.Now(),
		Inspection: collector.Inspection{Node: "node-1", Version: "v0.33.1",
			GC: []collector.GCRule{{Filters: []string{"type==exec.cachemount"}, Keep: 336 * time.Hour}, {MaxUsed: 40 << 30, MinFree: 16 << 30}},
			Usage: collector.Usage{Total: 300 << 20, Images: 200 << 20, Layers: 90 << 20, Mounts: 10 << 20,
				ImageSizes: map[string]int64{"python:3.13-slim": 200 << 20},
				MountList:  []collector.Mount{{Description: "cached mount /root/.cache/pip from exec x", Size: 10 << 20, LastUsed: time.Now()}}}},
		Active: []collector.Active{{Daemon: "box-buildkit-abcde", Target: "api" + evil, Started: started}}},
		{Name: "box-buildkit-fghij", Connected: false, Inspection: collector.Inspection{Usage: collector.Usage{ImageSizes: map[string]int64{"python:3.13-slim": 200 << 20}}}}}
	other := store.Job{Instance: inst, JobID: 777002, PipelineID: 998, ProjectID: 7, Name: "elsewhere-job", StartedAt: started}
	daemons[0].Inspection.GC = append([]collector.GCRule{{Filters: []string{"type==source.local"}, Keep: 48 * time.Hour, MaxUsed: 100 << 30}}, daemons[0].Inspection.GC...)
	daemons[0].Inspection.Usage.ImageSizes["golang:1.27"] = 300 << 20
	ts, st, _, _ := consoleWith(t, Options{Running: running{rj, other}, Live: daemons, Release: "box"})

	home := get(t, ts, "/")
	home.safe(t)
	if !strings.Contains(home.body, "/jobs/"+inst+"/777001") || !strings.Contains(home.body, "deploy&lt;script&gt;") {
		t.Errorf("running job not in the list")
	}
	if !regexp.MustCompile(`data-elapsed="4[0-9]"`).MatchString(home.body) {
		t.Errorf("the running job's time does not count on in the list")
	}
	if strings.Contains(home.body, "elsewhere-job") {
		t.Errorf("a running job of another project is in the list")
	}
	jp := get(t, ts, "/jobs/"+inst+"/777001")
	jp.safe(t)
	if jp.status != 200 || !strings.Contains(jp.body, "running for") || !strings.Contains(jp.body, "building api&lt;script&gt;") {
		t.Errorf("job page: %d", jp.status)
	}
	nodes := get(t, ts, "/nodes")
	nodes.safe(t)
	for _, want := range []string{"1 ready of 2", "on node-1", "300 MB", "/root/.cache/pip cache mount", "Duplicate cache",
		"python:3.13-slim", "Keep cache up to", "40.0 GB per node", "14 days after last use", "Running now", "Unreachable", "release <code>box</code>",
		"daemon replaced since"} {
		if !strings.Contains(nodes.body, want) {
			t.Errorf("nodes page lacks %q", want)
		}
	}
	if strings.Contains(nodes.body, "golang:1.27") {
		t.Errorf("an image on one node listed as duplicate")
	}
	if !strings.Contains(nodes.body, `class="wdot"`) {
		t.Errorf("no warning dot for the unreachable daemon")
	}

	// GitLab's times: the job started 7 s before its build.
	ps, _ := st.Pipelines(ctx, 100)
	_, jobs, builds, _ := st.PipelinePage(ctx, ps[0].Instance, ps[0].ID)
	b := builds[0]
	var job store.JobInfo
	for _, j := range jobs {
		if j.ID == b.Build.Attribution.Job {
			job = j
		}
	}
	err := st.SaveAPIPipeline(ctx, store.APIPipeline{Instance: ps[0].Instance, ID: ps[0].ID, ProjectID: 42, Status: "success",
		WebURL: "https://gitlab.example.com/p/-/pipelines/1", Read: time.Now(), Jobs: []store.APIJob{{ID: job.ID, Status: "success",
			Started: b.Build.Started.Add(-7 * time.Second), Finished: b.Build.Completed.Add(2 * time.Second),
			Duration: b.Build.Completed.Sub(b.Build.Started) + 9*time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	bp := get(t, ts, fmt.Sprintf("/builds/%d", b.ID))
	if !strings.Contains(bp.body, "Runner &amp; checkout <b>7.0 s</b>") || !strings.Contains(bp.body, "After the build <b>2.0 s</b>") {
		t.Errorf("build page without GitLab's times")
	}
	pp := get(t, ts, "/pipelines/"+ps[0].Instance+"/"+strconv.FormatUint(ps[0].ID, 10))
	if !strings.Contains(pp.body, `href="https://gitlab.example.com/p/-/pipelines/1"`) {
		t.Errorf("pipeline page without its GitLab link")
	}
}

// The list's filters: rebuilt leaves running jobs out; a category keeps
// only its jobs.
func TestListFilter(t *testing.T) {
	inst := history.InstancePrefix("https://gitlab.example.com/")
	ts, _, _, _ := consoleWith(t, Options{Running: running{{Instance: inst, JobID: 777003, PipelineID: 997, ProjectID: 42, Name: "now-running"}}})
	jobsIn := func(body string) int { return strings.Count(body, `class="opt j`) }
	// Only the list: the home page shows the latest pipeline's jobs again.
	list := func(path string) string {
		b := get(t, ts, path).body
		i := strings.Index(b, `id="list"`)
		j := strings.Index(b[i:], "</section>")
		return b[i : i+j]
	}
	all, rebuilt, files := list("/"), list("/?f=rebuilt"), list("/?f=files")
	if !strings.Contains(all, "now-running") || strings.Contains(rebuilt, "now-running") || jobsIn(rebuilt) != jobsIn(all)-1 {
		t.Errorf("rebuilt filter: %d of %d jobs", jobsIn(rebuilt), jobsIn(all))
	}
	if n := jobsIn(files); n == 0 || n != strings.Count(files, `<span class="vi vi-files"`) || n == jobsIn(all) {
		t.Errorf("files filter: %d jobs, %d files", n, strings.Count(files, `<span class="vi vi-files"`))
	}
	if !strings.Contains(files, `href="/" aria-label="Clear filter"`) {
		t.Errorf("a category filter has no way back")
	}
}

// A job whose pod still exists but which GitLab reports finished is not
// shown running.
func TestFinishedJobNotRunning(t *testing.T) {
	ts0, st0, _ := console(t)
	ts0.Close()
	ps, _ := st0.Pipelines(ctx, 100)
	p := ps[0]
	_, jobs, _, _ := st0.PipelinePage(ctx, p.Instance, p.ID)
	j := jobs[0]
	ts, st, _, _ := consoleWith(t, Options{Running: running{{Instance: j.Instance, JobID: j.ID, PipelineID: p.ID, ProjectID: 42, Name: j.Name}}})
	ps, _ = st.Pipelines(ctx, 100)
	running := func() bool { return strings.Contains(get(t, ts, "/").body, "vi-running") }
	if !running() {
		t.Fatal("a job with a pod is not shown running")
	}
	err := st.SaveAPIPipeline(ctx, store.APIPipeline{Instance: p.Instance, ID: p.ID, ProjectID: 42, Status: "success", Read: time.Now(),
		Jobs: []store.APIJob{{ID: j.ID, Status: "success"}}})
	if err != nil {
		t.Fatal(err)
	}
	if running() {
		t.Error("a job GitLab reports finished is shown running")
	}
	if pp := get(t, ts, "/pipelines/"+p.Instance+"/"+strconv.FormatUint(p.ID, 10)).body; strings.Contains(pp, "still running") {
		t.Error("the pipeline page counts a finished job as running")
	}
}

// rewrite sends every request to the test server, whatever host it names.
type rewrite struct{ to *url.URL }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.to.Scheme, r.to.Host
	return http.DefaultTransport.RoundTrip(req)
}

// Opening a pipeline asks GitLab for its jobs, and the build page then
// splits the job's time.
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
				"duration": 65.0, "pipeline": map[string]any{"status": "success", "web_url": "https://gitlab.example.com/p/-/pipelines/1"}})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer gl.Close()
	to, _ := url.Parse(gl.URL)
	client := gitlab.New([]string{"https://gitlab.example.com/"}, "t")
	client.HTTP = &http.Client{Transport: rewrite{to}}
	f := &gitlab.Fetcher{Client: client}
	ts, st, _, srv := consoleWith(t, Options{API: f})
	f.Store = st
	srv.APIWait = 3 * time.Second
	ps, _ := st.Pipelines(ctx, 100)
	p := ps[0]
	_, jobs, builds, _ := st.PipelinePage(ctx, p.Instance, p.ID)
	mu.Lock()
	seen := map[uint64]bool{}
	for _, b := range builds {
		for _, j := range jobs {
			// A job's start is before its first build.
			if j.ID == b.Build.Attribution.Job && !seen[j.ID] {
				seen[j.ID] = true
				ids, starts = append(ids, j.ID), append(starts, b.Build.Started)
			}
		}
	}
	mu.Unlock()
	page := get(t, ts, fmt.Sprintf("/builds/%d", builds[0].ID))
	if !strings.Contains(page.body, "Runner &amp; checkout <b>5.0 s</b>") || !strings.Contains(page.body, "1 min 5 s total") {
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
	ps, _ := st.Pipelines(ctx, 100)
	_, _, builds, _ := st.PipelinePage(ctx, ps[0].Instance, ps[0].ID)
	b := builds[len(builds)-1]
	err := st.SetReason(ctx, b.Build.Ref, b.Build.Daemon, history.Reason{Kind: history.CacheElsewhere, Step: b.Build.Steps[len(b.Build.Steps)-1],
		Old: "box-buildkit-old12", New: "daemon-a"})
	if err != nil {
		t.Fatal(err)
	}
	if page := get(t, ts, fmt.Sprintf("/builds/%d", b.ID)); !strings.Contains(page.body, "<b>old12</b> is gone") {
		t.Errorf("no note that the daemon is gone")
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
	if !strings.Contains(row, `chip failed`) || strings.Contains(row, `chip built`) || !strings.Contains(row, `c-failed`) {
		t.Errorf("failed step row: %s", row)
	}
	if !strings.Contains(p.body, "1 failed") || !strings.Contains(p.body, "only for builds that succeed") {
		t.Error("failed build: no failed count or no note on the missing Dockerfile")
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
		!strings.Contains(crow, "chip canceled") || !strings.Contains(c.body, "1 canceled") {
		t.Errorf("canceled build shown as failed: %s", crow)
	}
	e := get(t, ts, fmt.Sprintf("/builds/%d", eid))
	e.safe(t)
	if strings.Contains(e.body, "<table><thead><tr><th class=\"num\">Line") || strings.Contains(e.body, `data-act="toggleall"`) ||
		!strings.Contains(e.body, "No step ran") {
		t.Error("a build without steps shows an empty steps table")
	}
}

// Jobs without a build say what GitLab says about them, and no job "sets
// the pipeline time" while others still run.
func TestPipelineJobStates(t *testing.T) {
	ts, st, _ := console(t)
	ps, _ := st.Pipelines(ctx, 100)
	p := ps[0]
	_, jobs, _, _ := st.PipelinePage(ctx, p.Instance, p.ID)
	var apiJobs []store.APIJob
	for _, j := range jobs {
		apiJobs = append(apiJobs, store.APIJob{ID: j.ID, Name: j.Name, Status: "success"})
	}
	extra := []store.APIJob{{ID: 900001, Name: "queued", Status: "pending"}, {ID: 900002, Name: "starting", Status: "running", Started: time.Now().Add(-3 * time.Hour)},
		{ID: 900003, Name: "deploy", Status: "manual"}, {ID: 900004, Name: "notify", Status: "success"},
		{ID: 900005, Name: "gate", Status: "waiting_for_resource"}, {ID: 900006, Name: "notify", Status: "failed"}}
	save := func(status string) {
		err := st.SaveAPIPipeline(ctx, store.APIPipeline{Instance: p.Instance, ID: p.ID, ProjectID: 42, Status: status, Read: time.Now(),
			Jobs: append(append([]store.APIJob(nil), apiJobs...), extra...)})
		if err != nil {
			t.Fatal(err)
		}
	}
	url := "/pipelines/" + p.Instance + "/" + strconv.FormatUint(p.ID, 10)
	save("running")
	pg := get(t, ts, url)
	pg.safe(t)
	for _, want := range []string{"Waiting to start (pending)", "Waiting to start (waiting for resource)", "A manual job",
		"Running — its builds appear", "No build on the daemons · success", "so far: jobs still running"} {
		if !strings.Contains(pg.body, want) {
			t.Errorf("running pipeline lacks %q", want)
		}
	}
	// notify was retried: its first attempt says so, the retry does not.
	if n := strings.Count(pg.body, "retried: a later attempt follows"); n != 1 {
		t.Errorf("%d jobs marked retried, want 1", n)
	}
	if strings.Contains(pg.body, "sets the pipeline time") || strings.Contains(pg.body, "it ran elsewhere") {
		t.Error("a job sets the time of a pipeline that still runs")
	}
	// The time so far counts from the running job's start, not only the
	// builds that completed.
	if m := regexp.MustCompile(`Pipeline time</span><span class="v" data-elapsed="\d+">([^<]+)<`).FindStringSubmatch(pg.body); m == nil || !strings.Contains(m[1], "h") {
		t.Errorf("time so far: %v", m)
	}
	if !regexp.MustCompile(`data-elapsed="10[0-9]{3}">3 h 0 min<`).MatchString(pg.body) {
		t.Error("the running job's time does not count on")
	}
	extra[1].Status = "success"
	save("success")
	pg = get(t, ts, url)
	if !strings.Contains(pg.body, "sets the pipeline time") || strings.Contains(pg.body, "so far") {
		t.Error("a finished pipeline without the job that set its time")
	}
}

// A pipeline whose jobs run but whose builds have not completed yet has a
// page: the list links to it from the start.
func TestPipelineWithoutBuildsYet(t *testing.T) {
	inst := history.InstancePrefix("https://gitlab.example.com/")
	rj := store.Job{Instance: inst, JobID: 880001, PipelineID: 8800, ProjectID: 42, Name: "build-api", StartedAt: time.Now().Add(-20 * time.Second),
		CommitTitle: "first " + evil}
	ts, st, _, _ := consoleWith(t, Options{Running: running{rj}})
	if err := st.SaveJob(ctx, rj); err != nil {
		t.Fatal(err)
	}
	home := get(t, ts, "/")
	if !strings.Contains(home.body, `href="/pipelines/`+inst+`/8800"`) {
		t.Fatal("running pipeline not in the list")
	}
	p := get(t, ts, "/pipelines/"+inst+"/8800")
	p.safe(t)
	if p.status != http.StatusOK || !strings.Contains(p.body, "first &lt;script&gt;") || !strings.Contains(p.body, "Running — its builds appear") {
		t.Errorf("running pipeline page: %d", p.status)
	}
}

// A job that ran two builds shows two rows named by their targets.
func TestJobWithTwoBuilds(t *testing.T) {
	ts, st, _ := console(t)
	ps, _ := st.Pipelines(ctx, 100)
	p := ps[0]
	_, _, builds, _ := st.PipelinePage(ctx, p.Instance, p.ID)
	if len(builds) < 2 {
		t.Skip("fixture pipeline with one build")
	}
	a, b := builds[0].Build, *builds[1].Build
	b.Ref, b.Attribution.Job, b.Target = "secondref", a.Attribution.Job, "second<i>"
	if _, err := st.SaveBuild(ctx, store.Build{Build: &b, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	pg := get(t, ts, "/pipelines/"+p.Instance+"/"+strconv.FormatUint(p.ID, 10))
	pg.safe(t)
	if !strings.Contains(pg.body, " · second&lt;i&gt;") {
		t.Error("the job's second build is not named by its target")
	}
}
