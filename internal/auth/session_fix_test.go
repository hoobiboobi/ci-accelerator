package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// revokedSoon waits a little for GitLab to receive the revoke of tok.
func (f *fakeGitLab) revokedSoon(tok string) bool {
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		f.mu.Lock()
		for _, r := range f.revoked {
			if r == tok {
				f.mu.Unlock()
				return true
			}
		}
		f.mu.Unlock()
	}
	return false
}

// A session that ends by its idle time or its age on a request has its
// token revoked at GitLab, as a sign-out does: the grant does not outlive
// the session.
func TestSessionEndedByAgeRevokes(t *testing.T) {
	for _, tc := range []struct {
		name string
		live func(t *testing.T, e *env, c *http.Cookie)
	}{
		{"idle", func(t *testing.T, e *env, c *http.Cookie) { e.clock.add(7*24*time.Hour + time.Minute) }},
		{"max age", func(t *testing.T, e *env, c *http.Cookie) {
			for range 29 {
				e.clock.add(24 * time.Hour)
				e.load(t, c)
			}
			e.clock.add(24*time.Hour + time.Minute)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			cookie, _ := e.signIn(t, "alice", "/")
			tok, err := e.load(t, cookie).Token(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			tc.live(t, e, cookie)
			if v := e.load(t, cookie); v != nil {
				t.Fatal("the session outlived its time")
			}
			if !e.gl.revokedSoon(tok) {
				t.Errorf("the ended session's token was not revoked at GitLab: %v", e.gl.revoked)
			}
		})
	}
}

// The renewal state of a session goes with the session: at sign-out, at
// its end on a request and at the prune.
func TestRenewalStateGoesWithTheSession(t *testing.T) {
	count := func(a *Auth) int {
		n := 0
		a.locks.Range(func(any, any) bool { n++; return true })
		return n
	}
	e := setup(t)
	renew := func(name string) *http.Cookie {
		c, _ := e.signIn(t, name, "/")
		v := e.load(t, c)
		tok, _ := v.Token(context.Background())
		if _, err := v.Renew(context.Background(), tok); err != nil {
			t.Fatal(err)
		}
		return c
	}
	out := renew("alice")
	if count(e.a) != 1 {
		t.Fatalf("%d renewal states after one renewal", count(e.a))
	}
	r := httptest.NewRequest("POST", "/auth/sign-out", nil)
	r.AddCookie(out)
	e.a.SignOut(httptest.NewRecorder(), r)
	if n := count(e.a); n != 0 {
		t.Errorf("%d renewal states after the sign-out", n)
	}

	idle := renew("alice")
	e.clock.add(8 * 24 * time.Hour)
	if v := e.load(t, idle); v != nil {
		t.Fatal("kept past the idle time")
	}
	if n := count(e.a); n != 0 {
		t.Errorf("%d renewal states after the session ended on a request", n)
	}

	renew("alice")
	renew("root")
	e.clock.add(8 * 24 * time.Hour)
	if _, err := e.a.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := count(e.a); n != 0 {
		t.Errorf("%d renewal states after the prune", n)
	}
}

// A return address that the redirect's cleaning turns into one under
// /auth/ is refused like one written so.
func TestSafePathCleansBeforeTheAuthCheck(t *testing.T) {
	for _, in := range []string{"/./auth/sign-in/abc", "/x/../auth/sign-in/abc", "/%61uth/sign-in/abc", "/.//auth/x", "/auth"} {
		if got := SafePath(in); got != "/" {
			t.Errorf("SafePath(%q) = %q, want /", in, got)
		}
	}
	for _, in := range []string{"/authors", "/x/auth/y", "/builds/../pipelines"} {
		if got := SafePath(in); got != in {
			t.Errorf("SafePath(%q) = %q, want it unchanged", in, got)
		}
	}
}
