package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

const (
	twoA = "https://gitlab.example.com/"
	twoB = "https://gitlab-b.example.com/"
)

// twoGitLabs is two GitLabs behind one test server, told apart by the host
// the console asked for. Each takes only its own tokens (at-<user>-<A|B>n)
// and records what it was asked with which token.
type twoGitLabs struct {
	srv *httptest.Server
	mu  sync.Mutex
	log []string
	n   int
	// jobs: GitLab's job lists by address; any other list is empty.
	jobs map[string]string
}

func (g *twoGitLabs) take() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := g.log
	g.log = nil
	return out
}

type twoRoute struct{ to *url.URL }

func (r twoRoute) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("X-Two-Host", req.URL.Host)
	req.URL.Scheme, req.URL.Host = r.to.Scheme, r.to.Host
	return http.DefaultTransport.RoundTrip(req)
}

func newTwoGitLabs(t *testing.T) *twoGitLabs {
	g := &twoGitLabs{}
	users := map[string]map[string]int{
		"A": {"alice": 7, "root": 1},
		"B": {"bob": 7},
	}
	members := map[string]map[string]bool{"A": {"alice": true, "root": true}, "B": {"bob": true}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		name := "A"
		if strings.Contains(r.Header.Get("X-Two-Host"), "gitlab-b") {
			name = "B"
		}
		authz := r.Header.Get("Authorization")
		if strings.HasPrefix(authz, "Basic ") {
			authz = "Basic(client)"
		}
		g.log = append(g.log, fmt.Sprintf("GitLab %s <- %s %s [%s]", name, r.Method, r.URL.Path, authz))
		w.Header().Set("Content-Type", "application/json")
		tok := strings.TrimPrefix(authz, "Bearer at-")
		user, own := "", false
		if i := strings.Index(tok, "-"); i > 0 {
			user = tok[:i]
			own = strings.HasPrefix(tok[i+1:], name)
		}
		switch {
		case r.URL.Path == "/oauth/token":
			_ = r.ParseForm()
			g.n++
			who := r.Form.Get("code")
			if r.Form.Get("grant_type") == "refresh_token" {
				who = strings.TrimPrefix(r.Form.Get("refresh_token"), "rt-")
				if i := strings.Index(who, "-"); i > 0 {
					who = who[:i]
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("at-%s-%s%d", who, name, g.n), "refresh_token": fmt.Sprintf("rt-%s-%s%d", who, name, g.n),
				"token_type": "Bearer", "expires_in": 7200})
		case r.URL.Path == "/oauth/revoke":
			fmt.Fprint(w, "{}")
		case !own:
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/api/v4/user":
			id := users[name][user]
			if id == 0 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "username": user, "name": user})
		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/42/"):
			if !members[name][user] {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/trace") {
				fmt.Fprintf(w, "log of GitLab %s\n", name)
				return
			}
			if body, ok := g.jobs[name+r.URL.Path]; ok {
				fmt.Fprint(w, body)
				return
			}
			fmt.Fprint(w, "[]")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// twoSeed stores the recorded builds once per GitLab, with the same
// pipeline, job and project ids on each; project 42 is team-a/app on A
// and team-b/app on B. On B the first build's own job is not saved (its
// pod was never seen) while its pipeline is known through another job.
func twoSeed(t *testing.T, st *store.Store) map[string][]int64 {
	t.Helper()
	dirs, _ := filepath.Glob("../history/testdata/records/*")
	prefA := history.InstancePrefix(twoA)
	cfg := history.Config{Instances: map[string]bool{prefA: true}, SecretKey: st.SecretKey()}
	read := func(d, n string) []byte {
		b, _ := os.ReadFile(filepath.Join(d, n))
		return b
	}
	out := map[string][]int64{}
	for ui, u := range []string{twoA, twoB} {
		pref := history.InstancePrefix(u)
		team := "team-a"
		if u == twoB {
			team = "team-b"
		}
		var bs []*history.Build
		for _, d := range dirs {
			rec, err := history.ParseRecord(read(d, "record.json"))
			if err != nil {
				t.Fatal(err)
			}
			progress, _ := history.DecodeProgress(bytes.NewReader(read(d, "logs.bin")))
			b, err := history.NewBuild(rec, progress, [][]byte{read(d, "provenance.json")}, read(d, "trace.json"), read(d, "error.bin"), cfg)
			if err != nil {
				t.Fatal(err)
			}
			b.Daemon = "daemon-a"
			if ui > 0 {
				b.Ref += "b"
				if b.Attribution.State == history.Attributed {
					b.Attribution.Instance = pref
					b.Attribution.TraceID = "01" + pref + b.Attribution.TraceID[8:]
				}
			}
			bs = append(bs, b)
		}
		sort.Slice(bs, func(i, j int) bool { return bs[i].Started.Before(bs[j].Started) })
		history.Link(bs)
		first := true
		for _, b := range bs {
			a := b.Attribution
			if a.State != history.Attributed {
				continue
			}
			j := store.Job{Instance: a.Instance, JobID: a.Job, PipelineID: a.Pipeline, ProjectID: 42,
				ProjectPath: team + "/app", Name: "build-" + b.Target + "-" + team, Stage: "build", CommitTitle: "commit of " + team,
				CommitAuthor: team + "-dev", URL: strings.TrimSuffix(u, "/") + "/" + team + "/app/-/jobs/" + fmt.Sprint(a.Job), Ref: "main"}
			if u == twoB && first {
				first = false
				j.JobID, j.Name = 999001, "deploy-"+team
			}
			if err := st.SaveJob(ctx, j); err != nil {
				t.Fatal(err)
			}
		}
		for _, b := range bs {
			prev, _, _ := st.Previous(ctx, b)
			var p *history.Build
			if prev != nil {
				p = prev.Build
			}
			r, _ := history.FirstRebuild(p, b)
			id, err := st.SaveBuild(ctx, store.Build{Build: b, Generation: 1, Reason: r})
			if err != nil {
				t.Fatal(err)
			}
			if b.Attribution.State == history.Attributed {
				out[pref] = append(out[pref], id)
			}
		}
	}
	return out
}

type twoConsole struct {
	ts           *httptest.Server
	st           *store.Store
	g            *twoGitLabs
	ids          map[string][]int64
	prefA, prefB string
}

// newTwoConsole is a console with sign-in through two GitLabs; user id 1
// of A is an admin.
func newTwoConsole(t *testing.T) *twoConsole {
	t.Helper()
	testLogs = &lockedBuffer{}
	c := &twoConsole{g: newTwoGitLabs(t), prefA: history.InstancePrefix(twoA), prefB: history.InstancePrefix(twoB)}
	st, err := store.Open(ctx, t.TempDir(), store.Options{Now: func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c.st = st
	c.ids = twoSeed(t, st)
	to, _ := url.Parse(c.g.srv.URL)
	via := &http.Client{Transport: twoRoute{to}, Timeout: 10 * time.Second}
	client := gitlab.New([]string{twoA, twoB})
	client.HTTP = via
	o := testOpts{API: &gitlab.Fetcher{Client: client, Store: st}}
	a, err := auth.New(auth.Config{PublicURL: "https://127.0.0.1", Key: make([]byte, 32), HTTP: via, Instances: []auth.Instance{
		{URL: twoA, ClientID: "cid", Secret: "client-secret-value", Admins: []string{"id:1"}},
		{URL: twoB, ClientID: "cid", Secret: "client-secret-value"},
	}}, st, client, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	o.Auth = a
	backend, opt := o.wire(t, st)
	srv, err := New(backend, testLogger(), opt)
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return testNow }
	srv.APIWait = 2 * time.Second
	mux := http.NewServeMux()
	srv.Register(mux)
	c.ts = httptest.NewTLSServer(AccessLog(testLogger(), Secure(Compress(srv.Gate(mux)))))
	t.Cleanup(c.ts.Close)
	return c
}

func (c *twoConsole) browser(t *testing.T, user, inst string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	b := new(http.Client)
	*b = *c.ts.Client()
	b.Jar = jar
	b.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := b.Get(c.ts.URL + "/auth/sign-in/" + inst + "?to=/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	loc, _ := url.Parse(res.Header.Get("Location"))
	res, err = b.Get(c.ts.URL + "/auth/callback/" + inst + "?code=" + user + "&state=" + url.QueryEscape(loc.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/" {
		t.Fatalf("sign-in of %s at %s: %d to %q", user, inst, res.StatusCode, res.Header.Get("Location"))
	}
	c.g.take()
	return b
}

// get fetches a page and returns its status and Location.
func (c *twoConsole) get(t *testing.T, b *http.Client, p string) (int, string) {
	t.Helper()
	res, err := b.Get(c.ts.URL + p)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res.StatusCode, res.Header.Get("Location")
}

// foreignToken matches a request to GitLab B made with a token of A.
var foreignToken = regexp.MustCompile(`^GitLab B <- .*\[Bearer at-[a-z]+-A`)

// renewalAtA matches a token renewal at GitLab A. After the sign-in nothing
// renews: a renewal follows a 401 from the GitLab a token was sent to.
var renewalAtA = regexp.MustCompile(`^GitLab A <- POST /oauth/token`)

func (c *twoConsole) noForeignTokens(t *testing.T, who string) {
	t.Helper()
	time.Sleep(150 * time.Millisecond) // background reads the pages started
	for _, l := range c.g.take() {
		if foreignToken.MatchString(l) {
			t.Errorf("%s: a token of GitLab A was sent to GitLab B: %s", who, l)
		}
		if renewalAtA.MatchString(l) {
			t.Errorf("%s: a token was renewed at GitLab A: %s", who, l)
		}
	}
}

// A person's GitLab token goes to the GitLab that issued it only: not when
// a plain member follows a job-log link to a build of the other GitLab
// that the console cannot place, and not when an admin of one GitLab reads
// the other's pages.
func TestTokenStaysWithItsGitLab(t *testing.T) {
	c := newTwoConsole(t)
	first := c.ids[c.prefB][0]
	p, err := c.st.BuildPage(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	a := p.Build.Attribution
	jobB := fmt.Sprintf("/jobs/%s/%d", c.prefB, a.Job)

	alice := c.browser(t, "alice", c.prefA)
	c.g.take() // the sign-in's own token exchange
	for _, path := range []string{jobB, fmt.Sprintf("/builds/%d?via=log", first), fmt.Sprintf("/builds/%d", first)} {
		if code, loc := c.get(t, alice, path); code != http.StatusNotFound {
			t.Errorf("alice@A %s: %d %q, want 404", path, code, loc)
		}
	}
	c.noForeignTokens(t, "alice@A")

	root := c.browser(t, "root", c.prefA)
	c.g.take()
	paths := []string{"/projects/" + c.prefB + "/42", "/", "/insights", "/builds", "/pipelines?project=" + c.prefB + "/42"}
	for _, id := range c.ids[c.prefB] {
		pb, err := c.st.BuildPage(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		ab := pb.Build.Attribution
		paths = append(paths, fmt.Sprintf("/builds/%d", id), fmt.Sprintf("/builds/%d/compare", id),
			fmt.Sprintf("/pipelines/%s/%d", c.prefB, ab.Pipeline), fmt.Sprintf("/jobs/%s/%d", c.prefB, ab.Job),
			fmt.Sprintf("/jobs/%s/%d/log", c.prefB, ab.Job))
	}
	for _, path := range paths {
		c.get(t, root, path)
	}
	c.noForeignTokens(t, "root@A (admin)")

	// The other GitLab's own member still reaches the build through the
	// job's link, with a token of B asked at B: GitLab lists the job.
	c.g.mu.Lock()
	c.g.jobs = map[string]string{fmt.Sprintf("B/api/v4/projects/42/pipelines/%d/jobs", a.Pipeline): fmt.Sprintf(
		`[{"id":%d,"name":"build","stage":"build","status":"success","pipeline":{"id":%d,"project_id":42,"status":"success"}}]`, a.Job, a.Pipeline)}
	c.g.mu.Unlock()
	bob := c.browser(t, "bob", c.prefB)
	if code, loc := c.get(t, bob, jobB); code != http.StatusFound || !strings.HasPrefix(loc, fmt.Sprintf("/builds/%d", first)) {
		t.Errorf("bob@B %s: %d %q, want 302 to the build", jobB, code, loc)
	}
	c.noForeignTokens(t, "bob@B")
}
