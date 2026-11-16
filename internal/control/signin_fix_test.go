package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// fixClock is a clock the test moves.
type fixClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fixClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// signedIn starts a GitLab that signs alice in (A) and one that only
// counts what reaches it (B), and a sign-in that knows both; it returns
// alice's session cookie.
func signedIn(t *testing.T, st *store.Store) (a *auth.Auth, client *gitlab.Client, cookie, instA, instB string, hitsB *atomic.Int32, clock *fixClock) {
	t.Helper()
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/token":
			json.NewEncoder(w).Encode(map[string]any{"access_token": "at-alice", "refresh_token": "rt-alice", "token_type": "Bearer", "expires_in": 7200})
		case "/api/v4/user":
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "username": "alice", "name": "Alice"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(gl.Close)
	hitsB = &atomic.Int32{}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(other.Close)
	clock = &fixClock{t: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	client = gitlab.New([]string{gl.URL, other.URL})
	a, err := auth.New(auth.Config{PublicURL: "https://ci.example.com", Key: make([]byte, 32), Now: clock.now,
		Instances: []auth.Instance{{URL: gl.URL, ClientID: "cid", Secret: "sec"}, {URL: other.URL, ClientID: "cid", Secret: "sec"}}},
		st, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	instA, instB = history.InstancePrefix(gl.URL), history.InstancePrefix(other.URL)
	authURL, login, err := a.BeginWith(instA, "/")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	_, cookie, err = a.FinishWith(ctx, instA, login, url.Values{"state": {u.Query().Get("state")}, "code": {"c"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	return a, client, cookie, instA, instB, hitsB, clock
}

// A person's token goes only to the GitLab that issued it, also when the
// controller's API is asked for another GitLab: nothing reaches B, and
// the reads say why.
func TestLocalGitLabKeepsTheTokenWithItsGitLab(t *testing.T) {
	st := newStore(t)
	a, client, cookie, instA, instB, hitsB, _ := signedIn(t, st)
	v, _, err := a.LoadCookie(ctx, cookie)
	if err != nil || v == nil || !v.Usable() || v.Instance != instA {
		t.Fatalf("alice: %+v %v", v, err)
	}
	g := &LocalGitLab{Fetcher: &gitlab.Fetcher{Client: client, Store: st}, Auth: a}
	for name, rctx := range map[string]context.Context{"viewer": auth.With(ctx, v), "cookie": auth.WithCookie(ctx, cookie)} {
		g.Ensure(rctx, instB, 5, 77, "running", time.Time{}, 2*time.Second)
		if _, err := g.JobLogPart(rctx, instB, 5, 9001, gitlab.LogQuery{}); !errors.Is(err, gitlab.ErrOtherGitLab) {
			t.Errorf("%s: job log of B: %v", name, err)
		}
		if _, _, err := g.Changed(rctx, instB, 5, "a", "b"); !errors.Is(err, gitlab.ErrOtherGitLab) {
			t.Errorf("%s: comparison of B: %v", name, err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := hitsB.Load(); n != 0 {
		t.Fatalf("%d requests with alice's token reached the other GitLab", n)
	}
}

// An open live stream checks the session without taking the renewal of
// the cookie: the next page still gets Set-Cookie (both in one process
// and through the controller's API).
func TestStreamCheckLeavesTheRenewalToPages(t *testing.T) {
	st := newStore(t)
	a, _, cookie, _, _, _, clock := signedIn(t, st)
	srv := &Server{Local: &Local{Store: st}, Auth: a, Config: Config{Release: "box", SignIn: true, Idle: a.Idle()}}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	c, err := Dial(ctx, ClientOptions{URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	req := func() *http.Request {
		r := httptest.NewRequest("GET", "/builds", nil)
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cookie})
		return r
	}
	renewed := func(w *httptest.ResponseRecorder) bool {
		for _, ck := range w.Result().Cookies() {
			if ck.Name == auth.SessionCookie && ck.MaxAge > 0 {
				return true
			}
		}
		return false
	}
	type signer interface {
		Load(http.ResponseWriter, *http.Request) (*auth.Viewer, error)
		Check(*http.Request) (*auth.Viewer, error)
	}
	for name, s := range map[string]signer{"local": a, "client": c} {
		clock.add(11 * time.Minute)
		if v, err := s.Check(req()); err != nil || v == nil || v.Username != "alice" {
			t.Fatalf("%s: check: %+v %v", name, v, err)
		}
		w := httptest.NewRecorder()
		if v, err := s.Load(w, req()); err != nil || v == nil {
			t.Fatalf("%s: load: %+v %v", name, v, err)
		}
		if !renewed(w) {
			t.Errorf("%s: the page after a stream's check did not renew the session cookie", name)
		}
	}
	// A check still ends a session past its idle time.
	clock.add(a.Idle() + time.Minute)
	if v, err := a.Check(req()); err != nil || v != nil {
		t.Errorf("idle session: %+v %v", v, err)
	}
}

// A controller of the release before (no CheckCookie) still answers the
// web's check, through LoadCookie.
func TestCheckFallsBackToLoadCookie(t *testing.T) {
	var called []string
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/internal/v1/config":
			json.NewEncoder(w).Encode(Config{Release: "box", SignIn: true})
		case r.URL.Path == "/internal/v1/call/LoadCookie":
			mu.Lock()
			called = append(called, "LoadCookie")
			mu.Unlock()
			v, _ := json.Marshal(auth.Viewer{Username: "alice"})
			touched, _ := json.Marshal(true)
			json.NewEncoder(w).Encode(answer{R: []json.RawMessage{v, touched}})
		default:
			mu.Lock()
			called = append(called, strings.TrimPrefix(r.URL.Path, "/internal/v1/call/"))
			mu.Unlock()
			http.Error(w, "no such method", http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	c, err := Dial(ctx, ClientOptions{URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/events", nil)
	r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "x"})
	v, err := c.Check(r)
	if err != nil || v == nil || v.Username != "alice" {
		t.Fatalf("check: %+v %v", v, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(called, ",") != "CheckCookie,LoadCookie" {
		t.Errorf("calls %v", called)
	}
}
