package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// fakeGitLab answers like gitlab.com did in a live check: PKCE checked,
// a refresh revokes the old pair at once, a revoke ends the refresh token
// too, the API answers by what the token's user may read.
type fakeGitLab struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	codes     map[string]string // code -> PKCE challenge
	access    map[string]string // live access token -> username
	refresh   map[string]string // live refresh token -> access token it came with
	readable  map[string]map[uint64]bool
	refreshes int
	probes    int
	revoked   []string
	expiresIn int
	next      int
	// public: every project's jobs are readable, by anyone.
	public bool
}

var users = map[string]gitlab.User{"alice": {ID: 7, Username: "alice", Name: "Alice"}, "root": {ID: 1, Username: "root", Name: "Admin"}}

func newFake(t *testing.T) *fakeGitLab {
	f := &fakeGitLab{t: t, codes: map[string]string{}, access: map[string]string{}, refresh: map[string]string{},
		readable: map[string]map[uint64]bool{"alice": {42: true}}, expiresIn: 7200}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitLab) issue(user string) map[string]any {
	f.next++
	a, r := fmt.Sprintf("at-%s-%d", user, f.next), fmt.Sprintf("rt-%s-%d", user, f.next)
	f.access[a], f.refresh[r] = user, a
	return map[string]any{"access_token": a, "refresh_token": r, "token_type": "Bearer", "expires_in": f.expiresIn, "scope": "openid read_api"}
}

func (f *fakeGitLab) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	jsonOut := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case r.URL.Path == "/oauth/token":
		id, sec, ok := r.BasicAuth()
		if !ok || id != "cid" || sec != "sec" {
			jsonOut(401, map[string]string{"error": "invalid_client"})
			return
		}
		r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			ch, ok := f.codes[r.Form.Get("code")]
			delete(f.codes, r.Form.Get("code"))
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != ch || !strings.HasSuffix(r.Form.Get("redirect_uri"), "/auth/callback/"+history.InstancePrefix(f.srv.URL)) {
				jsonOut(400, map[string]string{"error": "invalid_grant"})
				return
			}
			jsonOut(200, f.issue(strings.SplitN(r.Form.Get("code"), ":", 2)[0]))
		case "refresh_token":
			f.refreshes++
			old, ok := f.refresh[r.Form.Get("refresh_token")]
			if !ok {
				jsonOut(400, map[string]string{"error": "invalid_grant"})
				return
			}
			user := f.access[old]
			delete(f.refresh, r.Form.Get("refresh_token"))
			delete(f.access, old)
			if user == "" {
				jsonOut(400, map[string]string{"error": "invalid_grant"})
				return
			}
			jsonOut(200, f.issue(user))
		}
	case r.URL.Path == "/oauth/revoke":
		r.ParseForm()
		tok := r.Form.Get("token")
		f.revoked = append(f.revoked, tok)
		delete(f.access, tok)
		for rt, a := range f.refresh {
			if a == tok {
				delete(f.refresh, rt)
			}
		}
		jsonOut(200, map[string]string{})
	case r.URL.Path == "/api/v4/user":
		user, ok := f.access[bearer]
		if !ok {
			w.WriteHeader(401)
			return
		}
		jsonOut(200, users[user])
	case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.HasSuffix(r.URL.Path, "/jobs"):
		f.probes++
		user, ok := f.access[bearer]
		if !ok && !f.public {
			w.WriteHeader(401)
			return
		}
		id, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v4/projects/"), "/jobs"), 10, 64)
		if !f.readable[user][id] && !f.public {
			jsonOut(404, map[string]string{"message": "404 Project Not Found"})
			return
		}
		jsonOut(200, []any{})
	default:
		http.NotFound(w, r)
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	a     *Auth
	gl    *fakeGitLab
	st    *store.Store
	clock *clock
	inst  string
}

func setup(t *testing.T) *env {
	t.Helper()
	gl := newFake(t)
	st, err := store.Open(context.Background(), t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &clock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	key := make([]byte, 32)
	a, err := New(Config{PublicURL: "http://127.0.0.1:8080/", Key: key, Now: c.now,
		Instances: []Instance{{URL: gl.srv.URL, ClientID: "cid", Secret: "sec", Admins: []string{"Root"}}}},
		st, gitlab.New([]string{gl.srv.URL}), nil)
	if err != nil {
		t.Fatal(err)
	}
	return &env{a: a, gl: gl, st: st, clock: c, inst: history.InstancePrefix(gl.srv.URL)}
}

// signIn runs the browser's part: start, GitLab approves as user, the
// callback. It returns the session cookie and where the console sent the
// browser.
func (e *env) signIn(t *testing.T, user, to string, have ...*http.Cookie) (*http.Cookie, string) {
	t.Helper()
	w := httptest.NewRecorder()
	if err := e.a.Begin(w, httptest.NewRequest("GET", "/auth/sign-in/"+e.inst, nil), e.inst, to); err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	q := loc.Query()
	login := w.Result().Cookies()[0]
	code := user + ":" + strconv.Itoa(len(e.gl.codes))
	e.gl.mu.Lock()
	e.gl.codes[code] = q.Get("code_challenge")
	e.gl.mu.Unlock()
	r := httptest.NewRequest("GET", "/auth/callback/"+e.inst+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(q.Get("state")), nil)
	r.AddCookie(login)
	for _, c := range have {
		r.AddCookie(c)
	}
	w = httptest.NewRecorder()
	next, err := e.a.Finish(w, r, e.inst)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookie {
			return c, next
		}
	}
	t.Fatal("no session cookie")
	return nil, ""
}

func (e *env) load(t *testing.T, c *http.Cookie) *Viewer {
	t.Helper()
	r := httptest.NewRequest("GET", "/", nil)
	if c != nil {
		r.AddCookie(c)
	}
	v, err := e.a.Load(httptest.NewRecorder(), r)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSignIn(t *testing.T) {
	e := setup(t)
	w := httptest.NewRecorder()
	if err := e.a.Begin(w, httptest.NewRequest("GET", "/", nil), e.inst, "/pipelines/x/1?f=builds"); err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	q := loc.Query()
	if loc.Path != "/oauth/authorize" || q.Get("client_id") != "cid" || q.Get("scope") != "openid read_api" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || len(q.Get("state")) < 40 ||
		q.Get("redirect_uri") != "http://127.0.0.1:8080/auth/callback/"+e.inst || q.Get("response_type") != "code" {
		t.Fatalf("authorize URL %s", loc)
	}
	lc := w.Result().Cookies()[0]
	if lc.Name != LoginCookie || !lc.Secure || !lc.HttpOnly || lc.SameSite != http.SameSiteLaxMode || lc.Path != "/" || lc.MaxAge != 600 {
		t.Errorf("login cookie %+v", lc)
	}
	if strings.Contains(lc.Value, q.Get("state")) {
		t.Error("the login cookie shows its state in the clear")
	}

	cookie, next := e.signIn(t, "alice", "/pipelines/x/1?f=builds")
	if next != "/pipelines/x/1?f=builds" {
		t.Errorf("sent to %q", next)
	}
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.MaxAge != 7*24*3600 {
		t.Errorf("session cookie %+v", cookie)
	}
	v := e.load(t, cookie)
	if v == nil || v.Username != "alice" || v.UserID != 7 || v.Admin || v.Instance != e.inst || v.Host != strings.TrimPrefix(e.gl.srv.URL, "http://") {
		t.Fatalf("viewer %+v", v)
	}
	// The store holds neither the cookie nor the token in the clear.
	raw, _ := base64.RawURLEncoding.DecodeString(cookie.Value)
	if _, err := e.st.Session(context.Background(), raw); !errors.Is(err, store.ErrNotFound) {
		t.Error("a session is found by the raw cookie")
	}
	x, err := e.st.Session(context.Background(), hash(raw))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(x.Token), "at-alice") || strings.Contains(string(x.Token), "rt-alice") {
		t.Error("token stored in the clear")
	}
	if tok, err := v.Token(context.Background()); err != nil || !strings.HasPrefix(tok, "at-alice-") {
		t.Errorf("token %q, %v", tok, err)
	}
	// Admins by username, whatever its case.
	root, _ := e.signIn(t, "root", "/")
	if v := e.load(t, root); v == nil || !v.Admin {
		t.Errorf("root %+v", v)
	}
	// No cookie, a made-up cookie: nobody.
	if v := e.load(t, nil); v != nil {
		t.Error("viewer without a cookie")
	}
	if v := e.load(t, &http.Cookie{Name: SessionCookie, Value: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}); v != nil {
		t.Error("viewer with a made-up cookie")
	}
}

func TestSignInRefused(t *testing.T) {
	e := setup(t)
	start := func() (*http.Cookie, url.Values) {
		w := httptest.NewRecorder()
		e.a.Begin(w, httptest.NewRequest("GET", "/", nil), e.inst, "/")
		loc, _ := url.Parse(w.Header().Get("Location"))
		return w.Result().Cookies()[0], loc.Query()
	}
	finish := func(instance, query string, c *http.Cookie) error {
		r := httptest.NewRequest("GET", "/auth/callback/"+instance+"?"+query, nil)
		if c != nil {
			r.AddCookie(c)
		}
		_, err := e.a.Finish(httptest.NewRecorder(), r, instance)
		return err
	}
	c, q := start()
	e.gl.codes["alice:x"] = q.Get("code_challenge")
	if err := finish(e.inst, "code=alice:x&state=wrong", c); !errors.Is(err, ErrSignIn) {
		t.Errorf("wrong state: %v", err)
	}
	if err := finish(e.inst, "code=alice:x&state="+q.Get("state"), nil); !errors.Is(err, ErrSignIn) {
		t.Errorf("no login cookie (another browser): %v", err)
	}
	if err := finish("abcdef", "code=alice:x&state="+q.Get("state"), c); !errors.Is(err, ErrSignIn) {
		t.Errorf("answer at another GitLab's callback: %v", err)
	}
	if err := finish(e.inst, "error=access_denied&state="+q.Get("state"), c); !errors.Is(err, ErrDenied) {
		t.Errorf("denied: %v", err)
	}
	c, q = start()
	e.clock.add(11 * time.Minute)
	if err := finish(e.inst, "code=alice:x&state="+q.Get("state"), c); !errors.Is(err, ErrSignIn) {
		t.Errorf("too late: %v", err)
	}
	// A code whose verifier does not match (an intercepted code used by
	// another browser's sign-in).
	c, q = start()
	e.gl.codes["alice:y"] = "not-this-challenge"
	if err := finish(e.inst, "code=alice:y&state="+q.Get("state"), c); !errors.Is(err, ErrSignIn) {
		t.Errorf("PKCE mismatch: %v", err)
	}
	if err := e.a.Begin(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), "abcdef", "/"); err == nil {
		t.Error("began at an unknown GitLab")
	}
}

func TestSafePath(t *testing.T) {
	for in, want := range map[string]string{
		"/pipelines/a/1?f=x": "/pipelines/a/1?f=x", "": "/", "//evil.example": "/", "https://evil.example/": "/",
		"/\\evil.example": "/", "javascript:alert(1)": "/", "/auth/sign-in": "/", "/x\r\nSet-Cookie: a": "/", "evil": "/",
	} {
		if got := SafePath(in); got != want {
			t.Errorf("SafePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRefreshOncePerSession(t *testing.T) {
	e := setup(t)
	cookie, _ := e.signIn(t, "alice", "/")
	v := e.load(t, cookie)
	first, _ := v.Token(context.Background())
	e.clock.add(2*time.Hour - 30*time.Second) // within a minute of expiry
	var wg sync.WaitGroup
	got := make([]string, 12)
	errs := make([]error, 12)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = v.Token(context.Background())
		}(i)
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || got[i] == first || got[i] != got[0] {
			t.Fatalf("token %d: %q, %v (first %q, others %q)", i, got[i], errs[i], first, got[0])
		}
	}
	if e.gl.refreshes != 1 {
		t.Errorf("refreshed %d times", e.gl.refreshes)
	}
	// A request that still had the old token and was refused: no second
	// refresh, the new token.
	if tok, err := v.Renew(context.Background(), first); err != nil || tok != got[0] || e.gl.refreshes != 1 {
		t.Errorf("renew with the stale token: %q, %v, refreshes %d", tok, err, e.gl.refreshes)
	}
	// Refused with the current token: renewed.
	if tok, err := v.Renew(context.Background(), got[0]); err != nil || tok == got[0] || e.gl.refreshes != 2 {
		t.Errorf("renew with the current token: %q, %v, refreshes %d", tok, err, e.gl.refreshes)
	}
	// The API through the viewer: an old token is renewed on refusal.
	u, err := e.a.client.User(context.Background(), v, e.inst)
	if err != nil || u.Username != "alice" {
		t.Errorf("user through the viewer: %+v, %v", u, err)
	}
}

func TestRevokedGrantSignsOut(t *testing.T) {
	e := setup(t)
	cookie, _ := e.signIn(t, "alice", "/")
	v := e.load(t, cookie)
	e.gl.mu.Lock()
	e.gl.refresh = map[string]string{} // the person revoked the application at GitLab
	e.gl.access = map[string]string{}
	e.gl.mu.Unlock()
	if _, err := e.a.client.User(context.Background(), v, e.inst); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("revoked grant: %v", err)
	}
	if v := e.load(t, cookie); v != nil {
		t.Error("session kept after GitLab refused its grant")
	}
}

func TestIdleAndKeyChange(t *testing.T) {
	e := setup(t)
	cookie, _ := e.signIn(t, "alice", "/")
	e.clock.add(6 * 24 * time.Hour)
	if v := e.load(t, cookie); v == nil {
		t.Fatal("ended before the idle time")
	}
	// Used on day 6: the idle time counts from then.
	e.clock.add(6 * 24 * time.Hour)
	if v := e.load(t, cookie); v == nil {
		t.Fatal("ended although used within the idle time")
	}
	e.clock.add(7*24*time.Hour + time.Minute)
	if v := e.load(t, cookie); v != nil {
		t.Error("kept past the idle time")
	}
	// A new key: sessions sealed with the old one end.
	cookie, _ = e.signIn(t, "alice", "/")
	key := make([]byte, 32)
	key[0] = 1
	b, _ := New(Config{PublicURL: "http://localhost:8080", Key: key, Now: e.clock.now,
		Instances: []Instance{{URL: e.gl.srv.URL, ClientID: "cid", Secret: "sec"}}}, e.st, gitlab.New([]string{e.gl.srv.URL}), nil)
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(cookie)
	if v, err := b.Load(httptest.NewRecorder(), r); v != nil || err != nil {
		t.Errorf("session under another key: %+v, %v", v, err)
	}
	// Prune removes what is idle.
	e.signIn(t, "alice", "/")
	e.clock.add(8 * 24 * time.Hour)
	if n, err := e.a.Prune(context.Background()); err != nil || n != 1 {
		t.Errorf("pruned %d, %v", n, err)
	}
}

func TestSignOutRevokes(t *testing.T) {
	e := setup(t)
	cookie, _ := e.signIn(t, "alice", "/")
	v := e.load(t, cookie)
	tok, _ := v.Token(context.Background())
	r := httptest.NewRequest("POST", "/auth/sign-out", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	if err := e.a.SignOut(w, r); err != nil {
		t.Fatal(err)
	}
	if len(e.gl.revoked) != 1 || e.gl.revoked[0] != tok {
		t.Errorf("revoked %v", e.gl.revoked)
	}
	if len(e.gl.refresh) != 0 {
		t.Error("refresh token alive after the revoke")
	}
	if c := w.Result().Cookies(); len(c) != 1 || c[0].MaxAge >= 0 {
		t.Errorf("cookie not cleared: %+v", c)
	}
	if v := e.load(t, cookie); v != nil {
		t.Error("signed in after signing out")
	}
}

func TestCanRead(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	alice := e.load(t, mustCookie(e.signIn(t, "alice", "/")))
	root := e.load(t, mustCookie(e.signIn(t, "root", "/")))
	ask := func(v *Viewer, instance string, project uint64) bool {
		t.Helper()
		ok, err := e.a.CanRead(ctx, v, instance, project)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !ask(alice, e.inst, 42) || ask(alice, e.inst, 43) {
		t.Fatal("alice's projects")
	}
	probes := e.gl.probes
	ask(alice, e.inst, 42)
	ask(alice, e.inst, 43)
	if e.gl.probes != probes {
		t.Error("asked GitLab again within the kept time")
	}
	// No longer a member: seen within the allow time, then not.
	e.gl.mu.Lock()
	e.gl.readable["alice"] = map[uint64]bool{43: true}
	e.gl.mu.Unlock()
	e.clock.add(2 * time.Minute)
	if !ask(alice, e.inst, 42) || !ask(alice, e.inst, 43) {
		t.Error("the deny answer is kept 90 s, the allow answer 10 min")
	}
	e.clock.add(9 * time.Minute)
	if ask(alice, e.inst, 42) {
		t.Error("access kept past 10 minutes")
	}
	if ask(alice, "abcdef", 42) || ask(alice, e.inst, 0) || ask(nil, e.inst, 42) {
		t.Error("another GitLab, no project or nobody")
	}
	probes = e.gl.probes
	if !ask(root, e.inst, 999) || !ask(root, "abcdef", 0) || e.gl.probes != probes {
		t.Error("admins see everything without asking")
	}
	// GitLab cannot answer: no, with the error.
	e.gl.srv.Close()
	ok, err := e.a.CanRead(ctx, &Viewer{Instance: e.inst, UserID: 99, a: e.a, id: []byte("x")}, e.inst, 42)
	if ok || err == nil {
		t.Errorf("unanswered: %v, %v", ok, err)
	}
}

func mustCookie(c *http.Cookie, _ string) *http.Cookie { return c }

func TestConfig(t *testing.T) {
	st, _ := store.Open(context.Background(), t.TempDir(), store.Options{})
	defer st.Close()
	gl := gitlab.New([]string{"https://gitlab.com"})
	ok := Config{PublicURL: "https://ci.example.com", Key: make([]byte, 32), Instances: []Instance{{URL: "https://gitlab.com", ClientID: "a", Secret: "b"}}}
	if _, err := New(ok, st, gl, nil); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Config){
		"http elsewhere":   func(c *Config) { c.PublicURL = "http://ci.example.com" },
		"query":            func(c *Config) { c.PublicURL = "https://ci.example.com/?a=1" },
		"short key":        func(c *Config) { c.Key = make([]byte, 16) },
		"no instance":      func(c *Config) { c.Instances = nil },
		"no secret":        func(c *Config) { c.Instances = []Instance{{URL: "https://gitlab.com", ClientID: "a"}} },
		"unknown GitLab":   func(c *Config) { c.Instances = []Instance{{URL: "https://gitlab.com/", ClientID: "a", Secret: "b"}} },
		"the same twice":   func(c *Config) { c.Instances = append(c.Instances, c.Instances[0]) },
		"another protocol": func(c *Config) { c.PublicURL = "ftp://ci.example.com" },
	} {
		c := ok
		c.Instances = append([]Instance(nil), ok.Instances...)
		change(&c)
		if _, err := New(c, st, gl, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, u := range []string{"http://localhost:18081", "http://127.0.0.1:8080/", "http://[::1]:8080"} {
		c := ok
		c.PublicURL = u
		if _, err := New(c, st, gl, nil); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	if k, err := ParseKey([]byte("  " + base64.StdEncoding.EncodeToString(make([]byte, 32)) + "\n")); err != nil || len(k) != 32 {
		t.Errorf("key: %v", err)
	}
	if _, err := ParseKey([]byte("c2hvcnQ=")); err == nil || !strings.Contains(err.Error(), "decodes to 5 bytes") {
		t.Errorf("short key: %v", err)
	}
	// What External Secrets' Password generator makes (docs/gitops.md):
	// 32 printable characters, base64-encoded, are a key; its default of 24
	// characters is refused, and so is the raw text.
	eso := `a1B2"c3\D4e~5F6g7H8i9J0k!L1m2N3o4P`[:32]
	if k, err := ParseKey([]byte(base64.StdEncoding.EncodeToString([]byte(eso)))); err != nil || string(k) != eso {
		t.Errorf("the generator's key, length 32 and base64: %q, %v", k, err)
	}
	if _, err := ParseKey([]byte(base64.StdEncoding.EncodeToString([]byte(eso[:24])))); err == nil || !strings.Contains(err.Error(), "decodes to 24 bytes") {
		t.Errorf("the generator's default length: %v", err)
	}
	if _, err := ParseKey([]byte(eso)); err == nil || !strings.Contains(err.Error(), "not base64") {
		t.Errorf("the generator's raw text: %v", err)
	}
}

func TestTwoGitLabs(t *testing.T) {
	e := setup(t)
	other := newFake(t)
	other.public = true
	b, err := New(Config{PublicURL: "https://ci.example.com", Key: make([]byte, 32), Now: e.clock.now,
		Instances: []Instance{{URL: e.gl.srv.URL, ClientID: "cid", Secret: "sec"}, {URL: other.srv.URL, ClientID: "cid", Secret: "sec"}}},
		e.st, gitlab.New([]string{e.gl.srv.URL, other.srv.URL}), nil)
	if err != nil {
		t.Fatal(err)
	}
	e.a = b
	inst2 := history.InstancePrefix(other.srv.URL)
	if ch := b.Instances(); len(ch) != 2 || ch[0].Instance != e.inst || ch[1].Instance != inst2 {
		t.Errorf("instances %+v", ch)
	}
	if b.RedirectURL(inst2) != "https://ci.example.com/auth/callback/"+inst2 {
		t.Errorf("redirect %s", b.RedirectURL(inst2))
	}
	// A sign-in started with the first GitLab, answered at the second's
	// callback: refused (an answer of one GitLab never signs in to another).
	w := httptest.NewRecorder()
	b.Begin(w, httptest.NewRequest("GET", "/", nil), e.inst, "/")
	loc, _ := url.Parse(w.Header().Get("Location"))
	other.codes["alice:z"] = loc.Query().Get("code_challenge")
	r := httptest.NewRequest("GET", "/auth/callback/"+inst2+"?code=alice:z&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	r.AddCookie(w.Result().Cookies()[0])
	if _, err := b.Finish(httptest.NewRecorder(), r, inst2); !errors.Is(err, ErrSignIn) {
		t.Errorf("answer at the other GitLab's callback: %v", err)
	}
	// Signed in with the first GitLab, alice sees nothing of the second,
	// even a project anyone may read there: the console does not ask.
	alice := e.load(t, mustCookie(e.signIn(t, "alice", "/")))
	if ok, err := b.CanRead(context.Background(), alice, inst2, 42); ok || err != nil || other.probes != 0 {
		t.Errorf("project of another GitLab: %v, %v, asked %d", ok, err, other.probes)
	}
}

func TestSealedTokenBoundToItsSession(t *testing.T) {
	e := setup(t)
	ac, _ := e.signIn(t, "alice", "/")
	rc, _ := e.signIn(t, "root", "/")
	id := func(c *http.Cookie) []byte {
		raw, _ := base64.RawURLEncoding.DecodeString(c.Value)
		return hash(raw)
	}
	rootRow, err := e.st.Session(context.Background(), id(rc))
	if err != nil {
		t.Fatal(err)
	}
	// Someone with write access to the database moves root's sealed token
	// into alice's session: it does not open there.
	if err := e.st.SetSessionToken(context.Background(), id(ac), rootRow.Token); err != nil {
		t.Fatal(err)
	}
	if v := e.load(t, ac); v != nil {
		t.Errorf("alice's session opened another session's token: %+v", v)
	}
}

func TestSignInAgainEndsTheOldSession(t *testing.T) {
	e := setup(t)
	first, _ := e.signIn(t, "alice", "/")
	old, _ := e.load(t, first).Token(context.Background())
	second, _ := e.signIn(t, "alice", "/", first)
	if v := e.load(t, first); v != nil {
		t.Error("the session from before the sign-in still works")
	}
	if v := e.load(t, second); v == nil {
		t.Fatal("the new session does not work")
	}
	if len(e.gl.revoked) != 1 || e.gl.revoked[0] != old {
		t.Errorf("the old session's token is not revoked at GitLab: %v", e.gl.revoked)
	}
}

// A session used every day still ends 30 days after sign-in: then GitLab
// is asked again who this is and what they may read.
func TestMaxAge(t *testing.T) {
	e := setup(t)
	cookie, _ := e.signIn(t, "alice", "/")
	for day := 1; day <= 29; day++ {
		e.clock.add(24 * time.Hour)
		if v := e.load(t, cookie); v == nil {
			t.Fatalf("ended on day %d, used every day", day)
		}
	}
	e.clock.add(24*time.Hour + time.Minute)
	if v := e.load(t, cookie); v != nil {
		t.Error("kept past 30 days after sign-in")
	}
	e.signIn(t, "alice", "/")
	e.clock.add(29 * 24 * time.Hour)
	e.signIn(t, "alice", "/")
	e.clock.add(2 * 24 * time.Hour)
	// The first is 31 days old; the second, 2 days and unused, stays.
	if n, err := e.a.Prune(context.Background()); err != nil || n != 1 {
		t.Errorf("pruned %d, %v", n, err)
	}
}
