package web

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// Without sign-in the console shows everything to whoever reaches it. With
// sign-in (Options.Auth) every page and stream needs a person signed in
// through GitLab, and shows only projects whose jobs GitLab lets that
// person read; admins see everything.

// Gate lets requests through to h. With sign-in: static files, the health
// check and the sign-in pages to anyone; the JSON API to admins; the rest
// to people signed in (pages send others to sign in, streams and data
// answer 401). Requests that change something must come from the
// console's own pages.
func (s *Server) Gate(h http.Handler) http.Handler {
	a := s.opt.Auth
	if a == nil {
		return h
	}
	csrf := http.NewCrossOriginProtection()
	if err := csrf.AddTrustedOrigin(a.PublicURL()); err != nil {
		s.log.Warn("console origin not trusted for posts", "err", err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/healthz" || strings.HasPrefix(p, "/static/") {
			h.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(p, "/auth/") {
			csrf.Handler(h).ServeHTTP(w, r)
			return
		}
		v, err := a.Load(w, r)
		if err != nil {
			s.fail(w, err)
			return
		}
		w.Header().Set("Vary", "Cookie")
		if v == nil {
			if r.Method == http.MethodGet && isPage(p) {
				http.Redirect(w, r, "/auth/sign-in?to="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "Sign in to the console first.", http.StatusUnauthorized)
			return
		}
		if strings.HasPrefix(p, "/api/") && !v.Admin {
			http.Error(w, "The console's JSON API answers admins only.", http.StatusForbidden)
			return
		}
		ctx := auth.With(r.Context(), v)
		if c, err := r.Cookie(auth.SessionCookie); err == nil {
			// For a backend in another process: it finds the person by it.
			ctx = auth.WithCookie(ctx, c.Value)
		}
		csrf.Handler(h).ServeHTTP(w, r.WithContext(ctx))
	})
}

// isPage says whether a path is a page a browser opens (sent to sign in),
// not a stream or data the pages fetch (answered 401).
func isPage(p string) bool {
	return p != "/events" && p != "/search-index" && !strings.HasPrefix(p, "/api/") && !strings.HasSuffix(p, "/log")
}

// viewer is the person signed in on r; nil without sign-in.
func viewer(r *http.Request) *auth.Viewer { return auth.From(r.Context()) }

// may says whether the person on r may see a project's builds and jobs;
// without sign-in, everyone may.
func (s *Server) may(r *http.Request, instance string, project uint64) bool {
	if s.opt.Auth == nil {
		return true
	}
	ok, err := s.opt.Auth.CanRead(r.Context(), viewer(r), instance, project)
	if err != nil {
		s.log.Warn("could not ask GitLab what a person may see", "project", store.ProjectKey{Instance: instance, ID: project}.String(), "err", err)
	}
	return ok
}

// readable keeps the projects the person on r may see, asking GitLab about
// several at a time.
func (s *Server) readable(r *http.Request, ps []store.Project) []store.Project {
	if s.opt.Auth == nil {
		return ps
	}
	keep := make([]bool, len(ps))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, p := range ps {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			keep[i] = s.may(r, p.Instance, p.ID)
			<-sem
		}()
	}
	wg.Wait()
	out := ps[:0:0]
	for i, p := range ps {
		if keep[i] {
			out = append(out, p)
		}
	}
	return out
}

// mayBuild says whether the person on r may see a stored build: by its
// pipeline's project, else its job's; a build of no known project only
// admins.
func (s *Server) mayBuild(r *http.Request, p *store.BuildPage) bool {
	project := uint64(0)
	switch {
	case p.Pipeline != nil && p.Pipeline.ProjectID != 0:
		project = p.Pipeline.ProjectID
	case p.Job != nil:
		project = p.Job.ProjectID
	}
	return s.may(r, p.Build.Attribution.Instance, project)
}

// nothing is the project of a person who may see none: no query finds it.
var nothing = store.ProjectKey{Instance: "000000", ID: math.MaxInt64}

// ---- sign-in pages ----

// SignIn is the sign-in page.
type SignIn struct {
	Title   string
	Choices []SignInChoice
	Message string
	Bad     bool // the message reports a failure
}

// SignInChoice is a GitLab to sign in with.
type SignInChoice struct{ Host, URL string }

var signInMessages = map[string]struct {
	text string
	bad  bool
}{
	"denied": {"GitLab did not allow the console to read with your account. Sign in again and choose Authorize to go on.", true},
	"failed": {"The sign-in did not complete. Start it again; if it keeps failing, the console's log says why.", true},
	"out":    {"You are signed out, and this session's access to your GitLab account was revoked. While you are signed in to GitLab, signing in here again may not ask you again: GitLab asks only when the console holds no access of yours (from another browser, say). To withdraw it everywhere, revoke the application in GitLab under Preferences > Applications.", false},
}

func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	a := s.opt.Auth
	to := auth.SafePath(r.URL.Query().Get("to"))
	msg, hasMsg := signInMessages[r.URL.Query().Get("e")]
	choices := a.Instances()
	if len(choices) == 1 && !hasMsg {
		// One GitLab: straight there. With a valid grant GitLab sends the
		// person back at once.
		http.Redirect(w, r, "/auth/sign-in/"+choices[0].Instance+"?to="+url.QueryEscape(to), http.StatusFound)
		return
	}
	v := SignIn{Title: "Sign in", Message: msg.text, Bad: msg.bad}
	for _, c := range choices {
		v.Choices = append(v.Choices, SignInChoice{Host: c.Host, URL: "/auth/sign-in/" + c.Instance + "?to=" + url.QueryEscape(to)})
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "signin", http.StatusOK, v)
}

func (s *Server) signInWith(w http.ResponseWriter, r *http.Request) {
	to := r.URL.Query().Get("to")
	if err := s.opt.Auth.Begin(w, r, r.PathValue("instance"), to); err != nil {
		http.Redirect(w, r, "/auth/sign-in?e=failed&to="+url.QueryEscape(auth.SafePath(to)), http.StatusFound)
	}
}

func (s *Server) signInDone(w http.ResponseWriter, r *http.Request) {
	to, err := s.opt.Auth.Finish(w, r, r.PathValue("instance"))
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case errors.Is(err, auth.ErrDenied):
		http.Redirect(w, r, "/auth/sign-in?e=denied&to="+url.QueryEscape(to), http.StatusFound)
	case err != nil:
		s.log.Warn("sign-in failed", "err", err)
		http.Redirect(w, r, "/auth/sign-in?e=failed&to="+url.QueryEscape(to), http.StatusFound)
	default:
		http.Redirect(w, r, to, http.StatusFound)
	}
}

func (s *Server) signOut(w http.ResponseWriter, r *http.Request) {
	if err := s.opt.Auth.SignOut(w, r); err != nil {
		s.log.Warn("sign-out", "err", err)
	}
	http.Redirect(w, r, "/auth/sign-in?e=out", http.StatusSeeOther)
}
