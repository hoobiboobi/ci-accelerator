package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// startLogin begins a sign-in and returns its login cookie and state.
func (e *env) startLogin(t *testing.T) (*http.Cookie, url.Values) {
	t.Helper()
	w := httptest.NewRecorder()
	if err := e.a.Begin(w, httptest.NewRequest("GET", "/", nil), e.inst, "/"); err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	return w.Result().Cookies()[0], loc.Query()
}

func (e *env) callback(login *http.Cookie, query string) (*httptest.ResponseRecorder, error) {
	r := httptest.NewRequest("GET", "/auth/callback/"+e.inst+"?"+query, nil)
	r.AddCookie(login)
	w := httptest.NewRecorder()
	_, err := e.a.Finish(w, r, e.inst)
	return w, err
}

// A login cookie answers one callback. Replayed with made-up codes, it
// never reaches GitLab's token endpoint again, and each answer clears it.
func TestLoginCookieIsSingleUse(t *testing.T) {
	e := setup(t)
	login, q := e.startLogin(t)
	before := e.gl.tokenHits.Load()
	for i := range 20 {
		w, err := e.callback(login, "code=made-up-"+string(rune('a'+i))+"&state="+url.QueryEscape(q.Get("state")))
		if !errors.Is(err, ErrSignIn) {
			t.Fatalf("replay %d: %v", i, err)
		}
		cleared := false
		for _, c := range w.Result().Cookies() {
			cleared = cleared || c.Name == LoginCookie && c.MaxAge < 0
		}
		if !cleared {
			t.Fatalf("replay %d did not clear the login cookie", i)
		}
	}
	if n := e.gl.tokenHits.Load() - before; n > 1 {
		t.Fatalf("20 callbacks with one login cookie made %d token exchanges, want at most 1", n)
	}
	// A real answer after the replays does not sign in either: that
	// sign-in is spent; a new one works.
	e.gl.mu.Lock()
	e.gl.codes["alice:late"] = q.Get("code_challenge")
	e.gl.mu.Unlock()
	if _, err := e.callback(login, "code=alice:late&state="+url.QueryEscape(q.Get("state"))); !errors.Is(err, ErrSignIn) {
		t.Fatalf("a spent sign-in signed in: %v", err)
	}
	if c, _ := e.signIn(t, "alice", "/"); e.load(t, c) == nil {
		t.Fatal("a new sign-in after the replays failed")
	}
}

// A login cookie past its expiry is refused before any exchange, even with
// a code GitLab would take.
func TestExpiredLoginCookie(t *testing.T) {
	e := setup(t)
	login, q := e.startLogin(t)
	e.gl.mu.Lock()
	e.gl.codes["alice:ok"] = q.Get("code_challenge")
	e.gl.mu.Unlock()
	e.clock.add(10*time.Minute + time.Second)
	before := e.gl.tokenHits.Load()
	if _, err := e.callback(login, "code=alice:ok&state="+url.QueryEscape(q.Get("state"))); !errors.Is(err, ErrSignIn) {
		t.Fatalf("an expired login cookie: %v", err)
	}
	if n := e.gl.tokenHits.Load() - before; n != 0 {
		t.Fatalf("an expired login cookie reached the token endpoint %d times", n)
	}
}

// GitLab's refusal of a code may carry text of its own: the error the
// sign-in returns, and what it logs, keep only the error's code.
func TestRefusedExchangeKeepsGitLabsBodyOut(t *testing.T) {
	var hits atomic.Int32
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_grant","error_description":"SECRETBODY"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer gl.Close()
	st, err := store.Open(context.Background(), t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var logs bytes.Buffer
	a, err := New(Config{PublicURL: "https://ci.example.com", Key: make([]byte, 32),
		Instances: []Instance{{URL: gl.URL, ClientID: "cid", Secret: "sec"}}},
		st, gitlab.New([]string{gl.URL}), slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	inst := history.InstancePrefix(gl.URL)
	authURL, login, err := a.BeginWith(inst, "/")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	_, _, err = a.FinishWith(context.Background(), inst, login, url.Values{"state": {u.Query().Get("state")}, "code": {"x"}}, "")
	if !errors.Is(err, ErrSignIn) || hits.Load() != 1 {
		t.Fatalf("refused exchange: %v (%d exchanges)", err, hits.Load())
	}
	if strings.Contains(err.Error(), "SECRETBODY") || strings.Contains(logs.String(), "SECRETBODY") {
		t.Fatalf("GitLab's body got out: %v; logs %q", err, logs.String())
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("the error lost GitLab's code: %v", err)
	}
}
