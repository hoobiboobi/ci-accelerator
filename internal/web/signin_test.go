package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// signInGitLab is a GitLab for the sign-in pages: a code names the person
// who approved; alice may read project 42's jobs, bob may not, root is an
// admin of the console. It records the tokens the console read with.
type signInGitLab struct {
	srv     *httptest.Server
	mu      sync.Mutex
	used    []string // Authorization headers of API reads
	revoked []string
}

func newSignInGitLab(t *testing.T) *signInGitLab {
	g := &signInGitLab{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		user := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer at-")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/oauth/token":
			r.ParseForm()
			json.NewEncoder(w).Encode(map[string]any{"access_token": "at-" + r.Form.Get("code"), "refresh_token": "rt-" + r.Form.Get("code"),
				"token_type": "Bearer", "expires_in": 7200})
		case r.URL.Path == "/oauth/revoke":
			r.ParseForm()
			g.revoked = append(g.revoked, r.Form.Get("token"))
			fmt.Fprint(w, "{}")
		case r.URL.Path == "/api/v4/user":
			ids := map[string]int{"alice": 7, "bob": 8, "root": 1}
			if ids[user] == 0 {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": ids[user], "username": user, "name": strings.ToUpper(user[:1]) + user[1:]})
		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/42/"):
			g.used = append(g.used, r.Header.Get("Authorization"))
			if user != "alice" && user != "root" {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"message":"404 Project Not Found"}`)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/trace") {
				fmt.Fprint(w, "deployed\n")
				return
			}
			fmt.Fprint(w, "[]")
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// signedIn returns a console with sign-in, and a function that gives a
// browser signed in as a person ("" for nobody).
func signedIn(t *testing.T) (*httptest.Server, *store.Store, map[string]int64, *signInGitLab, func(user string) *http.Client) {
	t.Helper()
	g := newSignInGitLab(t)
	to, _ := url.Parse(g.srv.URL)
	via := &http.Client{Transport: rewrite{to}, Timeout: 10 * time.Second}
	const gl = "https://gitlab.example.com/"
	ts, st, ids, _ := consoleBuild(t, func(st *store.Store) testOpts {
		client := gitlab.New([]string{gl})
		client.HTTP = via
		a, err := auth.New(auth.Config{PublicURL: "https://127.0.0.1", Key: make([]byte, 32), HTTP: via,
			Instances: []auth.Instance{{URL: gl, ClientID: "cid", Secret: "sec", Admins: []string{"root"}}}}, st, client, nil)
		if err != nil {
			t.Fatal(err)
		}
		return testOpts{Auth: a, API: &gitlab.Fetcher{Client: client, Store: st}}
	}, httptest.NewTLSServer)
	inst := history.InstancePrefix(gl)
	browser := func(user string) *http.Client {
		jar, _ := cookiejar.New(nil)
		c := new(http.Client)
		*c = *ts.Client() // one client per browser: its own cookies
		c.Jar = jar
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		if user == "" {
			return c
		}
		res, err := c.Get(ts.URL + "/auth/sign-in/" + inst + "?to=/")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		loc, _ := url.Parse(res.Header.Get("Location"))
		if loc.Host != "gitlab.example.com" || loc.Query().Get("redirect_uri") != "https://127.0.0.1/auth/callback/"+inst {
			t.Fatalf("sent to %s", loc)
		}
		res, err = c.Get(ts.URL + "/auth/callback/" + inst + "?code=" + user + "&state=" + url.QueryEscape(loc.Query().Get("state")))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/" {
			t.Fatalf("callback for %s: %d to %q", user, res.StatusCode, res.Header.Get("Location"))
		}
		return c
	}
	return ts, st, ids, g, browser
}

func fetch(t *testing.T, c *http.Client, ts *httptest.Server, method, path string, h map[string]string) page {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, nil)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return page{res.StatusCode, res.Header, string(b), path}
}

func TestSignInRequired(t *testing.T) {
	ts, _, ids, _, browser := signedIn(t)
	nobody := browser("")
	for path, want := range map[string]int{"/": 302, "/pipelines/abcdef/1": 302, "/images": 302,
		"/events": 401, "/search-index": 401, "/jobs/abcdef/1/log": 401, "/api/v1/builds": 401,
		"/healthz": 200, "/static/console.css": 200} {
		p := fetch(t, nobody, ts, "GET", path, nil)
		if p.status != want {
			t.Errorf("%s without sign-in: %d, want %d", path, p.status, want)
		}
		if strings.Contains(p.body, "example/") {
			t.Errorf("%s without sign-in shows project data", path)
		}
	}
	deep := fmt.Sprintf("/builds/%d?x=1", ids[firstRef(ids)])
	if p := fetch(t, nobody, ts, "GET", deep, nil); p.status != 302 || p.header.Get("Location") != "/auth/sign-in?to="+url.QueryEscape(deep) {
		t.Errorf("a page sends to sign in and back: %d %q", p.status, p.header.Get("Location"))
	}
	// One GitLab: the sign-in page goes straight there; with a message it
	// shows itself.
	if p := fetch(t, nobody, ts, "GET", "/auth/sign-in?to=/images", nil); p.status != 302 || !strings.HasPrefix(p.header.Get("Location"), "/auth/sign-in/") ||
		!strings.Contains(p.header.Get("Location"), "to=%2Fimages") {
		t.Errorf("sign-in with one GitLab: %d %q", p.status, p.header.Get("Location"))
	}
	p := fetch(t, nobody, ts, "GET", "/auth/sign-in?e=denied&to=//evil.example", nil)
	p.safe(t)
	if p.status != 200 || !strings.Contains(p.body, "did not allow the console") || !strings.Contains(p.body, "Sign in with GitLab") ||
		strings.Contains(p.body, "evil.example") {
		t.Errorf("sign-in page: %d", p.status)
	}
	// A made-up or unknown callback signs nobody in.
	if p := fetch(t, nobody, ts, "GET", "/auth/callback/"+history.InstancePrefix("https://gitlab.example.com/")+"?code=alice&state=x", nil); p.status != 302 ||
		!strings.Contains(p.header.Get("Location"), "e=failed") {
		t.Errorf("callback without a sign-in: %d %q", p.status, p.header.Get("Location"))
	}
	if p := fetch(t, nobody, ts, "GET", "/", nil); p.status != 302 {
		t.Error("signed in by a made-up callback")
	}
}

func firstRef(ids map[string]int64) string {
	for k := range ids {
		return k
	}
	return ""
}

// buildsByProject splits the fixture builds into those of a known project
// and those of none (a build without a trace of a CI job), each lowest id
// first, so a test never depends on map order.
func buildsByProject(t *testing.T, st *store.Store, ids map[string]int64) (known, none []int64) {
	t.Helper()
	for _, id := range ids {
		p, err := st.BuildPage(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case p.Pipeline != nil && p.Pipeline.ProjectID != 0, p.Job != nil && p.Job.ProjectID != 0:
			known = append(known, id)
		default:
			none = append(none, id)
		}
	}
	slices.Sort(known)
	slices.Sort(none)
	if len(known) == 0 || len(none) == 0 {
		t.Fatalf("fixtures: %d builds of a project, %d of none", len(known), len(none))
	}
	return known, none
}

// seedJob stores job 6003, which built nothing, in a pipeline of project
// 42 that has builds; it returns the pipeline's key.
func seedJob(t *testing.T, st *store.Store) (inst, key string) {
	t.Helper()
	keys, _ := pipelines(t, st)
	inst, id, _ := strings.Cut(keys[0], "/")
	pid, _ := strconv.ParseUint(id, 10, 64)
	if err := st.SaveJob(ctx, store.Job{Instance: inst, JobID: 6003, PipelineID: pid, ProjectID: 42, ProjectPath: "example/app", Name: "deploy"}); err != nil {
		t.Fatal(err)
	}
	return inst, keys[0]
}

func TestSignedInSeesWhatGitLabAllows(t *testing.T) {
	ts, st, ids, g, browser := signedIn(t)
	inst, key := seedJob(t, st)
	alice, bob, root := browser("alice"), browser("bob"), browser("root")
	known, none := buildsByProject(t, st, ids)
	build := fmt.Sprintf("/builds/%d", known[0])
	orphan := fmt.Sprintf("/builds/%d", none[0])
	paths := []string{"/pipelines/" + key, "/jobs/" + inst + "/6003", build, build + "/steps/0", "/projects/" + inst + "/42", "/images/api"}

	home := fetch(t, alice, ts, "GET", "/", nil)
	home.safe(t)
	if home.status != 200 || !strings.Contains(home.body, "@alice") || !strings.Contains(home.body, `action="/auth/sign-out"`) ||
		!strings.Contains(home.body, `class="rrow"`) || strings.Contains(home.body, "sign-in is off") {
		t.Fatalf("alice's home: %d", home.status)
	}
	for _, p := range paths {
		if got := fetch(t, alice, ts, "GET", p, nil); got.status != 200 && got.status != 302 {
			t.Errorf("alice %s: %d", p, got.status)
		}
	}
	// A build of no known project: admins only, for alice too.
	for _, p := range []string{orphan, orphan + "/steps/0"} {
		if got := fetch(t, alice, ts, "GET", p, nil); got.status != 404 {
			t.Errorf("alice %s (no project): %d, want 404", p, got.status)
		}
		if got := fetch(t, root, ts, "GET", p, nil); got.status != 200 {
			t.Errorf("root %s (no project): %d, want 200", p, got.status)
		}
	}
	if p := fetch(t, alice, ts, "GET", "/jobs/"+inst+"/6003/log", nil); p.status != 200 || !strings.Contains(p.body, "deployed") {
		t.Errorf("alice's log: %d %q", p.status, p.body)
	}
	if idx := fetch(t, alice, ts, "GET", "/search-index", nil); !strings.Contains(idx.body, "/pipelines/"+key) {
		t.Error("alice's search lacks her pipeline")
	}
	g.mu.Lock()
	for _, h := range g.used {
		if h != "Bearer at-alice" && h != "Bearer at-bob" && h != "Bearer at-root" {
			t.Errorf("GitLab read with %q", h)
		}
	}
	aliceRead := false
	for _, h := range g.used {
		aliceRead = aliceRead || h == "Bearer at-alice"
	}
	g.mu.Unlock()
	if !aliceRead {
		t.Error("GitLab was not read with alice's own token")
	}
	if p := fetch(t, alice, ts, "GET", "/api/v1/builds", nil); p.status != 403 {
		t.Errorf("JSON API for a non-admin: %d", p.status)
	}

	// bob may read nothing: every page of the project is "not found", the
	// list and search are empty, and the home page says why.
	bh := fetch(t, bob, ts, "GET", "/", nil)
	bh.safe(t)
	if bh.status != 200 || !strings.Contains(bh.body, "No project you can read builds here") || strings.Contains(bh.body, "/images/api") ||
		strings.Contains(bh.body, "example/") || strings.Contains(bh.body, "fix &lt;") {
		t.Errorf("bob's home shows the project or no reason: %d", bh.status)
	}
	for _, p := range append(paths, "/jobs/"+inst+"/6003/log") {
		got := fetch(t, bob, ts, "GET", p, nil)
		if got.status != 404 || strings.Contains(got.body, "fix &lt;") || strings.Contains(got.body, "example/") {
			t.Errorf("bob %s: %d", p, got.status)
		}
	}
	if idx := fetch(t, bob, ts, "GET", "/search-index", nil); strings.Contains(idx.body, "/pipelines/") || strings.Contains(idx.body, "/builds/") {
		t.Errorf("bob's search: %s", idx.body)
	}
	for _, p := range []string{"/images", "/builds"} {
		if got := fetch(t, bob, ts, "GET", p, nil); got.status != 200 || strings.Contains(got.body, "/builds/") || strings.Contains(got.body, "example/") {
			t.Errorf("bob's %s: %d", p, got.status)
		}
	}

	// root is an admin: everything, the JSON API too.
	if p := fetch(t, root, ts, "GET", "/api/v1/builds", nil); p.status != 200 {
		t.Errorf("JSON API for an admin: %d", p.status)
	}
	if p := fetch(t, root, ts, "GET", "/", nil); !strings.Contains(p.body, "@root") || !strings.Contains(p.body, "admin") {
		t.Error("root is not shown as admin")
	}
}

func TestLiveUpdatesOnlyOfReadableProjects(t *testing.T) {
	ts, st, _, _, browser := signedIn(t)
	_, key := seedJob(t, st)
	hears := func(c *http.Client, key string) bool {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+"/events", nil)
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		lines := make(chan string, 16)
		go func() {
			sc := bufio.NewScanner(res.Body)
			for sc.Scan() {
				lines <- sc.Text()
			}
			close(lines)
		}()
		<-lines // retry:
		time.Sleep(50 * time.Millisecond)
		st.Announce(key)
		timeout := time.After(time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					return false
				}
				if l == "data: "+key {
					return true
				}
			case <-timeout:
				return false
			}
		}
	}
	if !hears(browser("alice"), key) {
		t.Error("alice does not hear of her pipeline")
	}
	if hears(browser("bob"), key) {
		t.Error("bob hears of a pipeline he may not see")
	}
}

func TestSignOut(t *testing.T) {
	ts, _, _, g, browser := signedIn(t)
	alice := browser("alice")
	// A post from another site is refused.
	if p := fetch(t, alice, ts, "POST", "/auth/sign-out", map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}); p.status != 403 {
		t.Errorf("cross-site sign-out: %d", p.status)
	}
	if p := fetch(t, alice, ts, "GET", "/", nil); p.status != 200 {
		t.Fatal("signed out by another site")
	}
	p := fetch(t, alice, ts, "POST", "/auth/sign-out", map[string]string{"Origin": ts.URL, "Sec-Fetch-Site": "same-origin"})
	if p.status != 303 || p.header.Get("Location") != "/auth/sign-in?e=out" {
		t.Errorf("sign-out: %d %q", p.status, p.header.Get("Location"))
	}
	g.mu.Lock()
	revoked := append([]string(nil), g.revoked...)
	g.mu.Unlock()
	if len(revoked) != 1 || revoked[0] != "at-alice" {
		t.Errorf("revoked %v", revoked)
	}
	if p := fetch(t, alice, ts, "GET", "/", nil); p.status != 302 {
		t.Error("still signed in after signing out")
	}
	page := fetch(t, alice, ts, "GET", "/auth/sign-in?e=out", nil)
	if !strings.Contains(page.body, "You are signed out") {
		t.Error("the sign-in page does not say the person signed out")
	}
}
