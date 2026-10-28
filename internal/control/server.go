package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// The controller's API for the web: calls of the pages' reads (and the
// few writes they cause) by name, a stream of changes, the configuration
// the web needs, and a health check. It is internal: its shapes follow
// the console's own types and change with them; the web and the
// controller always come from the same release.
//
//	POST /internal/v1/call/<method>  body: the arguments as a JSON array
//	                                 answer: {"r": [results], "err": {...}}
//	GET  /internal/v1/events         server-sent events, data: ["key", ...]
//	GET  /internal/v1/config         Config
//	GET  /healthz                    the store answers
//
// Calls carry the caller's identity (Authorization: Bearer, checked by
// Authorize) and, for a page a person signed in to, their session cookie
// (X-CI-Session): GitLab is read with that person's token.

// SessionHeader carries the session cookie of the page a call serves.
const SessionHeader = "X-CI-Session"

// SetupHeader carries the setup the web started with (Config.Setup). A
// controller set up otherwise (sign-in turned on, a token added) answers
// 409: the web must start again, never serve pages under an old setup.
const SetupHeader = "X-CI-Setup"

// Config is what the web needs to know about the controller's setup.
type Config struct {
	Version string
	Release string
	// GitLab: the console can read GitLab's API at all; InstallToken: with
	// a token of the installation (no sign-in).
	GitLab       bool
	InstallToken bool
	// Known are the GitLabs' trace-id prefixes the console reads.
	Known []string
	// Sign-in: on, where browsers reach the console, the GitLabs to sign
	// in with, how long an unused session lasts.
	SignIn    bool
	PublicURL string
	Instances []auth.Choice
	Idle      time.Duration
}

// Setup is a digest of what decides how the web serves the pages: sign-in
// and its GitLabs, GitLab's API and whose token, the GitLabs read. Not the
// version: a web and a controller of two releases still speak.
func (c Config) Setup() string {
	cp := c
	cp.Version = ""
	b, _ := json.Marshal(cp)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Server serves the controller's API.
type Server struct {
	Local  *Local
	GitLab *LocalGitLab // nil: no GitLab API
	Auth   *auth.Auth   // nil: no sign-in
	Config Config
	// Authorize checks the caller of every request but the health check
	// and says its role; nil lets everyone through (tests only).
	Authorize func(r *http.Request) (Role, error)
	// Routes serves the build routers; nil: none.
	Routes *Routes
	Log    *slog.Logger
	// Heartbeat keeps an idle event stream open.
	Heartbeat time.Duration

	methods  map[string]reflect.Value
	stopping chan struct{}
	stopOnce sync.Once
}

// Stop ends the event streams at once (http.Server.Shutdown waits for
// open requests, and a stream never ends by itself): the web hears of a
// restart right away.
func (s *Server) Stop() {
	s.init()
	s.stopOnce.Do(func() { close(s.stopping) })
}

// The methods the web may call, by receiver. Nothing else is reachable:
// none of the store's writes but SettleElsewhere (a settled reason the
// pages work out with GitLab).
var (
	backendMethods = []string{"Projects", "Builds", "Branches", "Targets", "StepCounts", "ImageDays", "PipelineList",
		"JobsOf", "BuildsOf", "PipelineInfo", "PipelineProject", "PipelineJobs", "Job", "JobBuilds", "BuildPage", "Refs",
		"Previous", "StepHistory", "Output", "DaemonCounts", "ProjectCache", "Elsewhere", "SettleElsewhere", "Compared", "SaveCompare",
		"Recent", "Ping", "Daemons"}
	gitlabMethods = []string{"Ensure", "JobLogPart", "Changed", "Status"}
	authMethods   = []string{"BeginWith", "FinishWith", "LoadCookie", "EndCookie"}
)

// sessions answers what the person a call is for may read.
type sessions struct{ a *auth.Auth }

// CanReadAs is auth.CanRead for the person whose session cookie ctx
// carries.
func (s sessions) CanReadAs(ctx context.Context, instance string, project uint64) (bool, error) {
	v, _, err := s.a.LoadCookie(ctx, auth.CookieFrom(ctx))
	if err != nil || v == nil {
		return false, err
	}
	return s.a.CanRead(ctx, v, instance, project)
}

func (s *Server) init() {
	if s.methods != nil {
		return
	}
	s.methods = map[string]reflect.Value{}
	s.stopping = make(chan struct{})
	add := func(recv any, names []string) {
		v := reflect.ValueOf(recv)
		for _, n := range names {
			m := v.MethodByName(n)
			if !m.IsValid() {
				panic("control: no method " + n)
			}
			s.methods[n] = m
		}
	}
	add(s.Local, backendMethods)
	if s.GitLab != nil {
		add(s.GitLab, gitlabMethods)
	}
	if s.Auth != nil {
		add(s.Auth, authMethods)
		add(sessions{s.Auth}, []string{"CanReadAs"})
	}
	if s.Heartbeat == 0 {
		s.Heartbeat = 25 * time.Second
	}
	if s.Log == nil {
		s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
}

// Handler serves the API.
func (s *Server) Handler() http.Handler {
	s.init()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.Local.Ping(ctx); err != nil {
			http.Error(w, "store unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok")
	})
	mux.Handle("POST /internal/v1/call/{method}", s.authorized(RoleWeb, http.HandlerFunc(s.call)))
	mux.Handle("GET /internal/v1/events", s.authorized(RoleWeb, http.HandlerFunc(s.events)))
	mux.Handle("GET /internal/v1/config", s.authorized(RoleWeb, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.Config)
	})))
	if s.Routes != nil {
		mux.Handle("GET /internal/v1/route/warm", s.authorized(RoleRouter, http.HandlerFunc(s.Routes.warm)))
		mux.Handle("POST /internal/v1/route/decisions", s.authorized(RoleRouter, http.HandlerFunc(s.Routes.decisions)))
	}
	return mux
}

func (s *Server) authorized(want Role, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Authorize != nil {
			role, err := s.Authorize(r)
			if err == nil && !role.may(want) {
				err = fmt.Errorf("%w: a %s may not call %s", ErrForbidden, role, r.URL.Path)
			}
			if err != nil {
				status := http.StatusUnauthorized
				if errors.Is(err, ErrForbidden) {
					status = http.StatusForbidden
				}
				s.Log.Warn("controller API refused a call", "path", r.URL.Path, "err", err)
				http.Error(w, err.Error(), status)
				return
			}
		}
		if got := r.Header.Get(SetupHeader); got != "" && got != s.Config.Setup() {
			http.Error(w, "the controller's setup changed: the web must start again", http.StatusConflict)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// wireErr is an error as the API carries it: its text and, for the
// errors the pages tell apart, its kind.
type wireErr struct {
	Msg   string    `json:"msg"`
	Kind  string    `json:"kind,omitempty"`
	Until time.Time `json:"until,omitzero"`
}

type answer struct {
	R   []json.RawMessage `json:"r"`
	Err *wireErr          `json:"err,omitempty"`
}

func toWire(err error) *wireErr {
	if err == nil {
		return nil
	}
	e := &wireErr{Msg: err.Error()}
	var rl *gitlab.RateLimited
	switch {
	case errors.Is(err, store.ErrNotFound):
		e.Kind = "notfound"
	case errors.Is(err, sql.ErrNoRows):
		e.Kind = "norows"
	case errors.Is(err, auth.ErrSignedOut):
		e.Kind = "signedout"
	case errors.Is(err, auth.ErrDenied):
		e.Kind = "denied"
	case errors.Is(err, auth.ErrSignIn):
		e.Kind = "signin"
	case errors.Is(err, ErrNoToken):
		e.Kind = "notoken"
	case errors.As(err, &rl):
		e.Kind, e.Until = "ratelimited", rl.Until
	}
	return e
}

var (
	ctxType = reflect.TypeFor[context.Context]()
	errType = reflect.TypeFor[error]()
)

func (s *Server) call(w http.ResponseWriter, r *http.Request) {
	m, ok := s.methods[r.PathValue("method")]
	if !ok {
		http.Error(w, "no such method", http.StatusNotFound)
		return
	}
	var raw []json.RawMessage
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&raw); err != nil {
		http.Error(w, "arguments: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if c := r.Header.Get(SessionHeader); c != "" {
		ctx = auth.WithCookie(ctx, c)
	}
	t := m.Type()
	var args []reflect.Value
	first := 0
	if t.NumIn() > 0 && t.In(0) == ctxType {
		args = append(args, reflect.ValueOf(ctx))
		first = 1
	}
	if len(raw) != t.NumIn()-first {
		http.Error(w, fmt.Sprintf("%s takes %d arguments", r.PathValue("method"), t.NumIn()-first), http.StatusBadRequest)
		return
	}
	for i := first; i < t.NumIn(); i++ {
		v := reflect.New(t.In(i))
		if err := json.Unmarshal(raw[i-first], v.Interface()); err != nil {
			http.Error(w, fmt.Sprintf("argument %d: %v", i-first+1, err), http.StatusBadRequest)
			return
		}
		args = append(args, v.Elem())
	}
	out := m.Call(args)
	var ans answer
	for i, v := range out {
		if i == len(out)-1 && t.Out(i) == errType {
			if !v.IsNil() {
				ans.Err = toWire(v.Interface().(error))
			}
			continue
		}
		b, err := json.Marshal(v.Interface())
		if err != nil {
			s.Log.Error("controller API answer", "method", r.PathValue("method"), "err", err)
			http.Error(w, "the answer could not be encoded", http.StatusInternalServerError)
			return
		}
		ans.R = append(ans.R, b)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ans)
}

// events streams the store's changes: one event per batch, its data the
// changed keys as a JSON array.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sub := s.Local.Subscribe()
	defer s.Local.Unsubscribe(sub)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": hello\n\n")
	flusher.Flush()
	beat := time.NewTicker(s.Heartbeat)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.stopping:
			return
		case <-beat.C:
			fmt.Fprint(w, ": ping\n\n")
		case <-sub.C:
			b, _ := json.Marshal(sub.Take())
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		flusher.Flush()
	}
}

// ConfigFor describes a controller with these parts; gl and a may be nil.
func ConfigFor(version, release string, gl *LocalGitLab, a *auth.Auth) Config {
	c := Config{Version: version, Release: release}
	if gl != nil {
		c.GitLab = true
		c.InstallToken = gl.Token != nil && a == nil
		c.Known = gl.Fetcher.Client.Instances()
	}
	if a != nil {
		c.SignIn, c.PublicURL, c.Instances, c.Idle = true, a.PublicURL(), a.Instances(), a.Idle()
	}
	return c
}
