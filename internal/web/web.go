// Package web serves the console's pages: server-rendered HTML with a
// small script, no framework. Everything shown comes from builds and CI
// jobs and is escaped. Without sign-in (Options.Auth nil) the pages show
// everything to whoever reaches them and must not be exposed beyond the
// cluster; with it, each person sees what GitLab lets them read.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
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

// Options are the console's sources besides its store; each may be nil.
type Options struct {
	Live Live
	API  *gitlab.Fetcher
	// Token is the installation's GitLab token; nil: none. Not used with
	// sign-in: GitLab is read with each person's own token.
	Token gitlab.Credential
	// Auth signs people in through GitLab; nil: no sign-in.
	Auth *auth.Auth
	// Release names the installation on the nodes page.
	Release string
	// Assets replaces the built-in templates and static files (a directory
	// with templates/ and static/), read again for every page: for working
	// on the pages without rebuilding the console.
	Assets fs.FS
}

// Server renders the pages.
type Server struct {
	store  *store.Store
	opt    Options
	log    *slog.Logger
	files  fs.FS // templates/ and static/
	tmplMu sync.Mutex
	tmpl   map[string]*template.Template
	assets map[string]string // static file -> its URL with a content version
	// Open event streams, bounded: each holds a connection.
	streams chan struct{}
	// Heartbeat keeps idle streams open through proxies.
	Heartbeat time.Duration
	// APIWait is how long a page waits for GitLab's API.
	APIWait time.Duration
	now     func() time.Time

	// Figures computed over a project's recent builds, kept briefly.
	statsMu sync.Mutex
	stats   map[string]*cachedStats

	// Projects whose CacheElsewhere builds are being settled, and when
	// each was last tried.
	settleMu sync.Mutex
	settling map[store.ProjectKey]time.Time
}

// MaxStreams bounds the open live-update streams.
const MaxStreams = 256

// The templates of each page besides the layout and the shared parts.
var pageFiles = map[string][]string{
	"overview":  nil,
	"builds":    nil,
	"pipelines": nil,
	"images":    nil,
	"image":     nil,
	"build":     {"dockerfile"},
	"pipeline":  {"waterfall"},
	"job":       nil,
	"step":      nil,
	"steppage":  {"step"},
	"notfound":  nil,
	"nodes":     nil,
	"signin":    nil,
}

// New parses the templates; a template that does not parse is a build
// error, caught by the tests.
func New(s *store.Store, log *slog.Logger, opt Options) (*Server, error) {
	srv := &Server{store: s, opt: opt, log: log, files: files,
		streams: make(chan struct{}, MaxStreams), Heartbeat: 25 * time.Second, APIWait: 700 * time.Millisecond, now: time.Now}
	if opt.Assets != nil {
		srv.files = opt.Assets
	}
	tmpl, assets, err := parsePages(srv.files)
	if err != nil {
		return nil, err
	}
	srv.tmpl, srv.assets = tmpl, assets
	return srv, nil
}

// parsePages parses every page's templates and versions the static files.
func parsePages(src fs.FS) (map[string]*template.Template, map[string]string, error) {
	tmpl, assets := map[string]*template.Template{}, map[string]string{}
	static, _ := fs.ReadDir(src, "static")
	for _, f := range static {
		data, err := fs.ReadFile(src, "static/"+f.Name())
		if err != nil {
			return nil, nil, err
		}
		sum := sha256.Sum256(data)
		assets[f.Name()] = "/static/" + f.Name() + "?v=" + hex.EncodeToString(sum[:6])
	}
	asset := func(name string) (string, error) {
		if u, ok := assets[name]; ok {
			return u, nil
		}
		return "", fmt.Errorf("no static file %q", name)
	}
	for page, more := range pageFiles {
		names := []string{"templates/layout.html", "templates/parts.html", "templates/" + page + ".html"}
		for _, m := range more {
			names = append(names, "templates/"+m+".html")
		}
		t, err := template.New(page).Funcs(funcs).Funcs(template.FuncMap{"asset": asset}).ParseFS(src, names...)
		if err != nil {
			return nil, nil, err
		}
		tmpl[page] = t
	}
	return tmpl, assets, nil
}

// Register adds the pages to mux.
func (s *Server) Register(mux *http.ServeMux) {
	static, _ := fs.Sub(s.files, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(http.FileServerFS(static))))
	mux.HandleFunc("GET /{$}", s.overview)
	mux.HandleFunc("GET /pipelines", s.pipelines)
	mux.HandleFunc("GET /builds", s.builds)
	mux.HandleFunc("GET /images", s.images)
	mux.HandleFunc("GET /images/{target}", s.image)
	mux.HandleFunc("GET /pipelines/{instance}/{id}", s.pipeline)
	mux.HandleFunc("GET /jobs/{instance}/{id}", s.job)
	mux.HandleFunc("GET /jobs/{instance}/{id}/log", s.jobLog)
	mux.HandleFunc("GET /builds/{id}", s.build)
	mux.HandleFunc("GET /builds/{id}/steps/{step}", s.step)
	mux.HandleFunc("GET /nodes", s.nodes)
	// Addresses of the first preview, kept so bookmarks still lead somewhere.
	mux.HandleFunc("GET /insights", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/images", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /projects/{instance}/{id}", s.chooseProject)
	mux.HandleFunc("GET /search-index", s.index)
	mux.HandleFunc("GET /events", s.events)
	if s.opt.Auth != nil {
		mux.HandleFunc("GET /auth/sign-in", s.signIn)
		mux.HandleFunc("GET /auth/sign-in/{instance}", s.signInWith)
		mux.HandleFunc("GET /auth/callback/{instance}", s.signInDone)
		mux.HandleFunc("POST /auth/sign-out", s.signOut)
	}
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
	if s.opt.Assets != nil {
		// Working on the pages: what is on disk now.
		tmpl, assets, err := parsePages(s.files)
		if err != nil {
			s.log.Error("templates", "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.tmplMu.Lock()
		s.tmpl, s.assets = tmpl, assets
		s.tmplMu.Unlock()
	}
	s.tmplMu.Lock()
	t := s.tmpl[page]
	s.tmplMu.Unlock()
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, page, data); err != nil {
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
	Nav string // overview, builds, images, nodes
	// Live is what changes update the page: a pipeline key, "project:" and
	// a project key, or "". A page on a project only says that something
	// is new (Notify); a pipeline's page updates itself.
	Live   string
	Notify bool
	// LiveNodes: the page also shows the daemons' live state (connections,
	// inspections, running builds) and updates when it changes.
	LiveNodes bool
	Title     string
	Project   store.Project
	Projects  []store.Project
	Warn      string // a warning dot on Nodes
	// Viewer is the person signed in; nil without sign-in.
	Viewer *auth.Viewer
	// NoProjects: the person signed in may see no project here.
	NoProjects bool
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
	sh := Shell{Nav: nav, Live: live, Viewer: viewer(r)}
	ps, err := s.store.Projects(r.Context())
	if err != nil {
		return sh, err
	}
	ps = s.readable(r, ps)
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
	if len(ps) == 0 && sh.Viewer != nil && !sh.Viewer.Admin {
		// Nothing to see: every query of the pages finds nothing.
		sh.Project.ProjectKey, sh.NoProjects = nothing, true
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

// projectPage is the shell of a page about the chosen project: it says
// when the project has new builds, at most every 30 seconds.
func (s *Server) projectPage(r *http.Request, nav string) (Shell, error) {
	sh, err := s.shell(r, nav, "")
	if err == nil && sh.Project.ID != 0 && !sh.NoProjects {
		sh.Live, sh.Notify = "project:"+sh.Project.String(), true
	}
	return sh, err
}

// pinProject makes the shell show the project of what the page is about.
func pinProject(sh *Shell, instance string, project uint64) {
	for _, pr := range sh.Projects {
		if pr.Instance == instance && pr.ID == project {
			sh.Project = pr
		}
	}
}

var pagesOf = map[string]bool{"/": true, "/builds": true, "/images": true, "/nodes": true}

func (s *Server) chooseProject(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseUint(r.PathValue("id"), 10, 64)
	k := store.ProjectKey{Instance: r.PathValue("instance"), ID: id}
	if _, ok := store.ParseProjectKey(k.String()); !ok || !s.may(r, k.Instance, k.ID) {
		s.notFound(w, r, "project")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "project", Value: k.String(), Path: "/", MaxAge: 365 * 24 * 3600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode})
	to := r.URL.Query().Get("to")
	if !pagesOf[to] {
		to = "/"
	}
	http.Redirect(w, r, to, http.StatusFound)
}

// api returns the GitLab reader and whose token it reads with for r; nil
// when the console has no token to read with.
func (s *Server) api(r *http.Request) (*gitlab.Fetcher, gitlab.Credential) {
	if s.opt.API == nil {
		return nil, nil
	}
	if s.opt.Auth != nil {
		if v := viewer(r); v != nil {
			return s.opt.API, v
		}
		return nil, nil
	}
	if s.opt.Token == nil {
		return nil, nil
	}
	return s.opt.API, s.opt.Token
}

// ensureAPI reads a pipeline's jobs from GitLab when that is due, waiting
// briefly; it says whether it asked.
func (s *Server) ensureAPI(r *http.Request, p *store.Pipeline) bool {
	api, cred := s.api(r)
	if p == nil || api == nil || p.ProjectID == 0 {
		return false
	}
	status := p.Status
	if store.Finished(status) && !p.APIAt.IsZero() {
		// Over, as GitLab last said, unless a job ran since (a manual job
		// was run, a job retried).
		if !p.Activity.After(p.APIAt) {
			return false
		}
		status = ""
	}
	api.Ensure(cred, p.Instance, p.ProjectID, p.ID, status, p.APIAt, s.APIWait)
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
	"projName": projName,
	"statuses": func() []Status {
		out := make([]Status, len(statusKeys))
		for i, k := range statusKeys {
			out[i] = statuses[k]
		}
		return out
	},
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
