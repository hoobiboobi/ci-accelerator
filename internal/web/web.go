// Package web serves the console's pages: server-rendered HTML with a
// small script, no framework. Everything shown comes from builds and CI
// jobs and is escaped. The pages have no sign-in of their own yet: until
// sign-in through GitLab exists they must not be exposed beyond the
// cluster.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

//go:embed templates/*.html static/*
var files embed.FS

// Live is the daemons as the collector sees them now.
type Live interface {
	Daemons() []collector.DaemonState
}

// Running is the CI jobs in progress (their pods exist).
type Running interface {
	Jobs() []store.Job
}

// Options are the console's sources besides its store; each may be nil.
type Options struct {
	Live    Live
	Running Running
	API     *gitlab.Fetcher
	// Release names the installation on the nodes page.
	Release string
}

// Server renders the pages.
type Server struct {
	store  *store.Store
	opt    Options
	log    *slog.Logger
	tmpl   map[string]*template.Template
	assets map[string]string // static file -> its URL with a content version
	// Open event streams, bounded: each holds a connection.
	streams chan struct{}
	// Heartbeat keeps idle streams open through proxies.
	Heartbeat time.Duration
	// APIWait is how long a page waits for GitLab's API.
	APIWait time.Duration
	now     func() time.Time
}

// MaxStreams bounds the open live-update streams.
const MaxStreams = 256

// The templates of each page besides the layout and the shared parts.
var pageFiles = map[string][]string{
	"home":     nil,
	"build":    {"dockerfile", "waterfall"},
	"pipeline": {"dockerfile", "waterfall"},
	"job":      nil,
	"step":     nil,
	"steppage": {"step"},
	"notfound": nil,
	"insights": nil,
	"nodes":    nil,
}

// New parses the templates; a template that does not parse is a build
// error, caught by the tests.
func New(s *store.Store, log *slog.Logger, opt Options) (*Server, error) {
	srv := &Server{store: s, opt: opt, log: log, tmpl: map[string]*template.Template{}, assets: map[string]string{},
		streams: make(chan struct{}, MaxStreams), Heartbeat: 25 * time.Second, APIWait: 700 * time.Millisecond, now: time.Now}
	static, _ := fs.ReadDir(files, "static")
	for _, f := range static {
		data, err := files.ReadFile("static/" + f.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		srv.assets[f.Name()] = "/static/" + f.Name() + "?v=" + hex.EncodeToString(sum[:6])
	}
	asset := func(name string) (string, error) {
		if u, ok := srv.assets[name]; ok {
			return u, nil
		}
		return "", fmt.Errorf("no static file %q", name)
	}
	for page, more := range pageFiles {
		names := []string{"templates/layout.html", "templates/parts.html", "templates/list.html", "templates/" + page + ".html"}
		for _, m := range more {
			names = append(names, "templates/"+m+".html")
		}
		t, err := template.New(page).Funcs(funcs).Funcs(template.FuncMap{"asset": asset}).ParseFS(files, names...)
		if err != nil {
			return nil, err
		}
		srv.tmpl[page] = t
	}
	return srv, nil
}

// Register adds the pages to mux.
func (s *Server) Register(mux *http.ServeMux) {
	static, _ := fs.Sub(files, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(http.FileServerFS(static))))
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /pipelines/{instance}/{id}", s.pipeline)
	mux.HandleFunc("GET /jobs/{instance}/{id}", s.job)
	mux.HandleFunc("GET /builds/{id}", s.build)
	mux.HandleFunc("GET /builds/{id}/steps/{step}", s.step)
	mux.HandleFunc("GET /insights", s.insights)
	mux.HandleFunc("GET /nodes", s.nodes)
	mux.HandleFunc("GET /projects/{instance}/{id}", s.chooseProject)
	mux.HandleFunc("GET /search-index", s.index)
	mux.HandleFunc("GET /events", s.events)
	// Any other address gets the console's own page, not a bare text 404.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) { s.notFound(w, r, "page") })
}

// CSP is the content security policy of every response: only the console's
// own script and stylesheet, no inline code. Style attributes are allowed:
// the server writes bar positions and sizes into them (numbers only); a
// browser without style-src-attr falls back to style-src and only loses
// the bars.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// Secure sets the headers every response gets: the content security
// policy, and no framing, sniffing or referrers.
func Secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", CSP)
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		h.ServeHTTP(w, r)
	})
}

// cacheFor lets browsers keep a static file for a year when it is asked
// for by its versioned URL (the pages link nothing else), and revalidate
// otherwise.
func cacheFor(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		h.ServeHTTP(w, r)
	})
}

// render executes the whole page before sending it, so a template error
// is a 500 and never half a page with a 200.
func (s *Server) render(w http.ResponseWriter, page string, status int, data any) {
	var buf bytes.Buffer
	if err := s.tmpl[page].ExecuteTemplate(&buf, page, data); err != nil {
		s.log.Error("render", "page", page, "err", err)
		http.Error(w, "the console could not render this page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("page", "err", err)
	http.Error(w, "the console could not read its store", http.StatusInternalServerError)
}

// Shell is what every page's frame needs.
type Shell struct {
	Nav  string // pipes, insights, nodes
	Live string // what changes update the page: "all", a pipeline key, or ""
	// LiveNodes: the page also shows the daemons' live state (connections,
	// inspections, running builds) and updates when it changes.
	LiveNodes bool
	Title     string
	Project   store.Project
	Projects  []store.Project
	Warn      string // a warning dot on Build nodes
	Count     int    // pipelines in the list
	List      *ListView
	Split     bool   // the page has the pipeline list beside it
	Has       string // what the pane shows beside the list: none, pipe, job
}

// NotFound is the page for an address with nothing behind it.
type NotFound struct {
	Shell
	What string
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request, what string) {
	sh, err := s.shell(r, "", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.Title = "Not found"
	s.render(w, "notfound", http.StatusNotFound, NotFound{Shell: sh, What: what})
}

// shell resolves the project the pages show: the request's, the one chosen
// before, or the most recently built.
func (s *Server) shell(r *http.Request, nav, live string) (Shell, error) {
	sh := Shell{Nav: nav, Live: live}
	ps, err := s.store.Projects(r.Context())
	if err != nil {
		return sh, err
	}
	sh.Projects = ps
	want := r.URL.Query().Get("project")
	if want == "" {
		if c, err := r.Cookie("project"); err == nil {
			want = c.Value
		}
	}
	for _, p := range ps {
		if p.String() == want {
			sh.Project = p
		}
	}
	if sh.Project.ID == 0 && len(ps) > 0 {
		sh.Project = ps[0]
	}
	if s.opt.Live != nil {
		for _, d := range s.opt.Live.Daemons() {
			if !d.Connected {
				sh.Warn = "A build daemon is not reachable"
			}
		}
	}
	return sh, nil
}

func (s *Server) chooseProject(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseUint(r.PathValue("id"), 10, 64)
	k := store.ProjectKey{Instance: r.PathValue("instance"), ID: id}
	if _, ok := store.ParseProjectKey(k.String()); !ok {
		s.notFound(w, r, "project")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "project", Value: k.String(), Path: "/", MaxAge: 365 * 24 * 3600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode})
	to := r.URL.Query().Get("to")
	if to != "/insights" && to != "/nodes" {
		to = "/"
	}
	http.Redirect(w, r, to, http.StatusFound)
}

// ---- the list ----

// ListView is the pipeline list beside the pages.
type ListView struct {
	Filter      string
	FilterLabel string // a filter that is not one of the buttons
	Filters     []FilterLink
	Pipes       []ListPipe
	Clear       string
}

// FilterLink is one of the list's filter buttons.
type FilterLink struct {
	Label, URL string
	On         bool
}

// ListPipe is a pipeline in the list.
type ListPipe struct {
	Key, URL, Title, Short, SHA, When, Dur string
	Open                                   bool
	Jobs                                   []ListRow
}

// ListRow is a job in the list.
type ListRow struct {
	Key, URL, Name, Dur string
	Cat                 Cat
	Open                bool
	Retried             bool // a later attempt of the job followed
	// Elapsed: seconds a running job has run; the page counts on from it.
	Elapsed int
}

// withFilter keeps the list's filter on a link.
func withFilter(u, f string) string {
	if f == "" {
		return u
	}
	return u + "?f=" + url.QueryEscape(f)
}

// listFor builds the list: the project's latest pipelines with their jobs
// that built here, and the jobs running now. open is the key of the open
// item ("p:<instance>/<id>" or "j:<instance>/<id>").
func (s *Server) listFor(ctx context.Context, r *http.Request, sh *Shell, open, path string) (*ListView, error) {
	f := r.URL.Query().Get("f")
	lv := &ListView{Filter: f}
	known := false
	for _, x := range filters {
		known = known || x.Key == f
		lv.Filters = append(lv.Filters, FilterLink{Label: x.Label, URL: withFilter(path, x.Key), On: x.Key == f})
	}
	if !known {
		lv.FilterLabel, lv.Clear = filterLabel(f), path
	}
	list, err := s.store.List(ctx, sh.Project.ProjectKey, 40)
	if err != nil {
		return nil, err
	}
	sh.Count = len(list)
	running := map[string]store.Job{}
	if s.opt.Running != nil {
		for _, j := range s.opt.Running.Jobs() {
			if sh.Project.ID != 0 && j.ProjectID != sh.Project.ID {
				continue
			}
			running[fmt.Sprintf("%s/%d", j.Instance, j.JobID)] = j
		}
	}
	seen := map[string]bool{}
	byKey := map[string]int{}
	for _, p := range list {
		key := store.PipelineKey(p.Instance, p.ID)
		lp := ListPipe{Key: "p:" + key, URL: withFilter("/pipelines/"+key, f), Title: p.CommitTitle, Short: "#" + shortID(p.ID),
			SHA: short(p.SHA, 7), When: when(p.Created), Open: open == "p:"+key}
		if lp.Title == "" {
			lp.Title = "Pipeline #" + strconv.FormatUint(p.ID, 10)
		}
		if lp.When == "" {
			lp.When = when(p.Started)
		}
		var start, end time.Time
		retried := retriedOf(p.Jobs, func(j store.ListJob) (string, uint64) { return j.Name, j.ID })
		for _, j := range p.Jobs {
			jt := timesOf(j.JobInfo, j.Started, j.Completed)
			if start.IsZero() || jt.Start.Before(start) {
				start = jt.Start
			}
			if jt.End.After(end) {
				end = jt.End
			}
			jk := fmt.Sprintf("%s/%d", j.Instance, j.ID)
			c := catOf(j.Reason, j.Error, j.Built)
			if _, ok := running[jk]; ok {
				seen[jk] = true
				// Its pod may outlive the job a little; GitLab knows better.
				if !store.Finished(j.Status) {
					c = cats["running"]
				}
			}
			if !passes(f, c) {
				continue
			}
			row := ListRow{Key: "j:" + jk, URL: withFilter(fmt.Sprintf("/builds/%d", j.BuildID), f), Name: j.Name,
				Dur: duration(jt.Total), Cat: c, Open: open == "j:"+jk, Retried: retried[j.ID]}
			if c.Key == "running" && !jt.Start.IsZero() {
				row.Elapsed = int(s.now().Sub(jt.Start).Seconds())
				row.Dur = duration(time.Duration(row.Elapsed) * time.Second)
			}
			lp.Jobs = append(lp.Jobs, row)
		}
		if !end.IsZero() {
			lp.Dur = duration(end.Sub(start))
		}
		byKey[key] = len(lv.Pipes)
		lv.Pipes = append(lv.Pipes, lp)
	}
	// Running jobs without a build yet.
	var fresh []ListPipe
	for jk, j := range running {
		if seen[jk] || !passes(f, cats["running"]) {
			continue
		}
		row := ListRow{Key: "j:" + jk, URL: fmt.Sprintf("/jobs/%s/%d", j.Instance, j.JobID), Name: j.Name, Cat: cats["running"],
			Open: open == "j:"+jk}
		if !j.StartedAt.IsZero() {
			row.Elapsed = int(s.now().Sub(j.StartedAt).Seconds())
			row.Dur = duration(time.Duration(row.Elapsed) * time.Second)
		}
		pk := store.PipelineKey(j.Instance, j.PipelineID)
		if i, ok := byKey[pk]; ok {
			lv.Pipes[i].Jobs = append(lv.Pipes[i].Jobs, row)
			continue
		}
		found := false
		for i := range fresh {
			if fresh[i].Key == "p:"+pk {
				fresh[i].Jobs = append(fresh[i].Jobs, row)
				found = true
			}
		}
		if !found {
			title := j.CommitTitle
			if title == "" {
				title = "Pipeline #" + strconv.FormatUint(j.PipelineID, 10)
			}
			fresh = append(fresh, ListPipe{Key: "p:" + pk, URL: "/pipelines/" + pk, Title: title, Short: "#" + shortID(j.PipelineID),
				SHA: short(j.SHA, 7), When: when(j.PipelineCreatedAt), Dur: "running", Open: open == "p:"+pk, Jobs: []ListRow{row}})
		}
	}
	lv.Pipes = append(fresh, lv.Pipes...)
	if f != "" {
		kept := lv.Pipes[:0]
		for _, p := range lv.Pipes {
			if len(p.Jobs) > 0 {
				kept = append(kept, p)
			}
		}
		lv.Pipes = kept
	}
	return lv, nil
}

// neighbours returns the jobs before and after open in the list.
func (lv *ListView) neighbours(open string) (prev, next string) {
	var rows []ListRow
	for _, p := range lv.Pipes {
		rows = append(rows, p.Jobs...)
	}
	for i, r := range rows {
		if r.Key == open {
			if i > 0 {
				prev = rows[i-1].URL
			}
			if i+1 < len(rows) {
				next = rows[i+1].URL
			}
		}
	}
	return prev, next
}

// pipeNeighbours returns the newer and older pipelines around open.
func (lv *ListView) pipeNeighbours(open string) (newer, older string) {
	for i, p := range lv.Pipes {
		if p.Key == open {
			if i > 0 {
				newer = lv.Pipes[i-1].URL
			}
			if i+1 < len(lv.Pipes) {
				older = lv.Pipes[i+1].URL
			}
		}
	}
	return newer, older
}

// ensureAPI reads a pipeline's jobs from GitLab when that is due, waiting
// briefly; it says whether it asked.
func (s *Server) ensureAPI(p *store.Pipeline) bool {
	if p == nil || s.opt.API == nil || p.ProjectID == 0 {
		return false
	}
	if store.Finished(p.Status) && !p.APIAt.IsZero() {
		return false
	}
	s.opt.API.Ensure(p.Instance, p.ProjectID, p.ID, p.Status, p.APIAt, s.APIWait)
	return true
}

// projName is a project's name, the last part of its path.
func projName(path string, id uint64) string {
	if path == "" {
		return fmt.Sprintf("project %d", id)
	}
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// projNS is a project's namespace.
func projNS(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[:i]
	}
	return ""
}

func projInitial(path string) string {
	n := projName(path, 0)
	if n == "" {
		return "P"
	}
	return strings.ToUpper(n[:1])
}

var funcs = template.FuncMap{
	"mul":      func(a, b int) int { return a * b },
	"join":     strings.Join,
	"research": researchFor,
	"critL": func(r WFRow) string {
		l, _ := barSpan(r)
		return strconv.FormatFloat(l, 'f', 2, 64)
	},
	"critW": func(r WFRow) string {
		l, rr := barSpan(r)
		return strconv.FormatFloat(rr-l, 'f', 2, 64)
	},
	"projName":    projName,
	"projNS":      projNS,
	"projInitial": projInitial,
	"dur":         dur,
	"when":        when,
	"icon":        icon,
	"short":       short,
	"shortd":      shortDaemon,
	"bytes":       bytesText,
	"sicon":       stateIcon,
	"slabel": func(s string) string {
		if l, ok := stateLabels[s]; ok {
			return l
		}
		return s
	},
	"add": func(a, b int) int { return a + b },
	"ago": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format("2 Jan 15:04 UTC")
	},
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// barSpan is where a row's bars begin and end, percent of the axis.
func barSpan(r WFRow) (float64, float64) {
	lo, hi := 100.0, 0.0
	for _, b := range r.Bars {
		l, _ := strconv.ParseFloat(b.L, 64)
		w, _ := strconv.ParseFloat(b.W, 64)
		lo, hi = min(lo, l), max(hi, l+w)
	}
	if hi < lo {
		return 0, 0
	}
	return lo, hi
}
