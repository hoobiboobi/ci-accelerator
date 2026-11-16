package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
)

// delivered: the clusters a page asked the hub to deliver secrets to;
// removed: the clusters whose link a page asked the hub to close.
var delivered, removed []string

// signedInSigner stands for sign-in on pages served without the gate: the
// test names the principal itself.
type signedInSigner struct{ Signer }

// hubAdmin is a hub's root pages with sign-in on (signIn) or off; the
// header X-Who names the person: owner (from the values), owner2 (given
// the role on the pages), operator, viewer or nobody.
func hubAdmin(t *testing.T, signIn bool) (*httptest.Server, *fleet.Registry) {
	t.Helper()
	return hubAdminWith(t, signIn, nil)
}

// hubAdminWith is hubAdmin with the Hub changed by change first.
func hubAdminWith(t *testing.T, signIn bool, change func(*Hub)) (*httptest.Server, *fleet.Registry) {
	t.Helper()
	ctx := context.Background()
	reg, err := fleet.OpenRegistry(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	if err := reg.SyncOwners(ctx, []string{"gitlab:gitlab.example.com/1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Grant(ctx, "local", fleet.OriginLocal, "gitlab:gitlab.example.com/2", fleet.Operator); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Grant(ctx, "local", fleet.OriginLocal, "gitlab:gitlab.example.com/5", fleet.Owner); err != nil {
		t.Fatal(err)
	}
	who := func(r *http.Request) fleet.Principal {
		switch r.Header.Get("X-Who") {
		case "owner":
			return fleet.Principal{Subject: "gitlab:gitlab.example.com/1", Role: fleet.Owner}
		case "owner2":
			return fleet.Principal{Subject: "gitlab:gitlab.example.com/5", Role: fleet.Owner}
		case "operator":
			return fleet.Principal{Subject: "gitlab:gitlab.example.com/2", Role: fleet.Operator}
		case "viewer":
			return fleet.Principal{Subject: "gitlab:gitlab.example.com/3", Role: fleet.Viewer}
		}
		return fleet.Principal{}
	}
	sealer, _ := fleet.NewSealer([]byte("0123456789abcdef0123456789abcdef"))
	opt := Options{Fleet: fakeFleet{}, Version: "test", Hub: &Hub{Registry: reg, Who: who, Bootstrap: true, GitLabs: []string{"gitlab.example.com"},
		Sealer: sealer, Version: "0.8.0", Info: fleet.HubInfo{URL: "https://hub.example.com", PublicKeys: "-----BEGIN PUBLIC KEY-----\nhub\n-----END PUBLIC KEY-----\n"},
		Deliver: func(name string) { delivered = append(delivered, name) }, Removed: func(name string) { removed = append(removed, name) }}}
	if signIn {
		opt.Auth = signedInSigner{}
	}
	if change != nil {
		change(opt.Hub)
	}
	srv, err := New(Empty{}, testLogger(), opt)
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return testNow }
	mux := http.NewServeMux()
	srv.Register(mux)
	var h http.Handler = mux
	if signIn {
		// The gate's part: the person signed in, in the request.
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if who := r.Header.Get("X-Who"); who != "" {
				r = r.WithContext(auth.With(r.Context(), &auth.Viewer{Instance: "0a1b2c", Host: "gitlab.example.com", Username: who}))
			}
			mux.ServeHTTP(w, r)
		})
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, reg
}

// flashOf is the message a change left for the next page: its text, and
// whether it reports a failure.
func flashOf(p page) (string, bool) {
	for _, c := range (&http.Response{Header: p.header}).Cookies() {
		if c.Name == "hub-msg" && c.MaxAge > 0 {
			v, _ := url.QueryUnescape(c.Value)
			if len(v) < 1 {
				return "", false
			}
			return v[1:], v[0] == '1'
		}
	}
	return "", false
}

func as(t *testing.T, ts *httptest.Server, who, method, path string, form url.Values) page {
	t.Helper()
	return asWith(t, ts, who, method, path, form, nil)
}

// asWith is as, with cookies.
func asWith(t *testing.T, ts *httptest.Server, who, method, path string, form url.Values, cookies []*http.Cookie) page {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, ts.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("X-Who", who)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	c := ts.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return page{res.StatusCode, res.Header, string(b), path}
}

func TestHubAccessPages(t *testing.T) {
	ctx := context.Background()
	ts, reg := hubAdmin(t, true)

	// An owner: everything.
	p := as(t, ts, "owner", "GET", "/access", nil)
	for _, want := range []string{"People · 3", "API tokens · 1", "Give a role", "gitlab.example.com/1", "locked", "Make owner", `href="/access"`, `href="/audit"`} {
		if !strings.Contains(p.body, want) {
			t.Errorf("owner's Access: no %q", want)
		}
	}
	if strings.Count(p.body, `action="/access/people/remove"`) != 2 {
		t.Error("the owner from the values can be removed, or the others cannot")
	}
	p = as(t, ts, "owner", "POST", "/access/people", url.Values{"gitlab": {"gitlab.example.com"}, "user": {"id:42"}, "role": {"operator"}})
	if p.status != http.StatusSeeOther || reg.RoleOf("gitlab:gitlab.example.com/42") != fleet.Operator {
		t.Fatalf("give a role: %d %s", p.status, reg.RoleOf("gitlab:gitlab.example.com/42"))
	}
	if p := as(t, ts, "owner", "POST", "/access/people", url.Values{"gitlab": {"gitlab.example.com"}, "user": {"bob"}, "role": {"operator"}}); p.status != http.StatusSeeOther {
		t.Errorf("a username for an id: %d", p.status)
	} else if _, bad := flashOf(p); !bad || p.header.Get("Location") != "/access" {
		t.Errorf("a username for an id: %q", p.header.Get("Location"))
	}
	if p := as(t, ts, "owner2", "POST", "/access/people/remove", url.Values{"subject": {"gitlab:gitlab.example.com/1"}}); reg.RoleOf("gitlab:gitlab.example.com/1") != fleet.Owner {
		t.Errorf("removing the values' owner: %s", p.header.Get("Location"))
	} else if _, bad := flashOf(p); !bad {
		t.Error("removing the values' owner: no failure said")
	}

	p = as(t, ts, "owner", "POST", "/access/tokens", url.Values{"name": {"terraform"}, "role": {"owner"}, "days": {"30"}})
	if p.status != 200 || p.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("issue: %d %q", p.status, p.header.Get("Cache-Control"))
	}
	if strings.Contains(p.body, "newsecret") {
		t.Error("a class with no style")
	}
	i := strings.Index(p.body, fleet.TokenPrefix)
	if i < 0 {
		t.Fatal("the new token's secret is not shown")
	}
	secret := p.body[i : i+len(fleet.TokenPrefix)+43]
	if pr, ok := reg.TokenPrincipal(ctx, secret); !ok || pr.Subject != "token:terraform" {
		t.Fatalf("the shown secret does not open: %v", ok)
	}
	if again := as(t, ts, "owner", "GET", "/access/tokens", nil); strings.Contains(again.body, secret) || !strings.Contains(again.body, "terraform") {
		t.Error("the secret is shown again, or the token is not listed")
	}
	p = as(t, ts, "owner", "POST", "/access/tokens/revoke", url.Values{"name": {"terraform"}})
	if p.status != http.StatusSeeOther {
		t.Errorf("revoke: %d", p.status)
	}
	if _, ok := reg.TokenPrincipal(ctx, secret); ok {
		t.Error("revoked from the page, still opens")
	}

	// An operator: reads Access and the audit, changes no role, sees no token.
	p = as(t, ts, "operator", "GET", "/access", nil)
	if p.status != 200 || strings.Contains(p.body, "Give a role") || strings.Contains(p.body, `href="/access/tokens"`) || strings.Contains(p.body, "Make owner") {
		t.Errorf("operator's Access: %d", p.status)
	}
	for _, c := range []struct{ method, path string }{{"POST", "/access/people"}, {"POST", "/access/people/remove"}, {"GET", "/access/tokens"},
		{"POST", "/access/tokens"}, {"POST", "/access/tokens/revoke"}} {
		if p := as(t, ts, "operator", c.method, c.path, url.Values{"subject": {"gitlab:gitlab.example.com/42"}, "role": {"owner"}, "name": {"x"}}); p.status != http.StatusForbidden {
			t.Errorf("operator %s %s: %d", c.method, c.path, p.status)
		}
	}
	if reg.RoleOf("gitlab:gitlab.example.com/42") != fleet.Operator {
		t.Error("an operator changed a role")
	}
	// Nor issued a token: only the owner's is there.
	if ts, err := reg.Tokens(ctx); err != nil || len(ts) != 1 || ts[0].Name != "terraform" {
		t.Errorf("tokens after the operator's attempts: %v %v", ts, err)
	}
	// The owner's issue is in the audit as such.
	evs, err := reg.AuditQuery(ctx, fleet.AuditQuery{Actor: "gitlab:gitlab.example.com/1"})
	if err != nil {
		t.Fatal(err)
	}
	issued := false
	for _, e := range evs {
		issued = issued || e.Action == "token.issue"
	}
	if !issued {
		t.Errorf("no token.issue by the owner in the audit: %v", evs)
	}
	p = as(t, ts, "operator", "GET", "/audit", nil)
	for _, want := range []string{"role.grant", "token.issue", "token.revoke", "gitlab:gitlab.example.com/42"} {
		if !strings.Contains(p.body, want) {
			t.Errorf("audit: no %q", want)
		}
	}
	if f := as(t, ts, "operator", "GET", "/audit?who=values", nil); strings.Contains(f.body, "token.issue") || !strings.Contains(f.body, "role.grant") {
		t.Error("the audit's Who filter")
	}
	if m := as(t, ts, "operator", "GET", "/", nil); m.status != 200 || !strings.Contains(m.body, "Members") {
		t.Errorf("operator's Members: %d", m.status)
	}

	// A viewer: to a cluster; no Members, Access or audit.
	if p := as(t, ts, "viewer", "GET", "/", nil); p.status != http.StatusFound || p.header.Get("Location") != "/m/eu/" {
		t.Errorf("viewer's home: %d %q", p.status, p.header.Get("Location"))
	}
	for _, path := range []string{"/access", "/audit", "/access/tokens"} {
		if p := as(t, ts, "viewer", "GET", path, nil); p.status != http.StatusForbidden {
			t.Errorf("viewer %s: %d", path, p.status)
		}
	}
}

// Without sign-in the pages read for everyone and change nothing.
func TestHubAccessWithoutSignIn(t *testing.T) {
	ts, reg := hubAdmin(t, false)
	p := as(t, ts, "", "GET", "/access", nil)
	if p.status != 200 || !strings.Contains(p.body, "Sign-in is off") || strings.Contains(p.body, "Give a role") || strings.Contains(p.body, `href="/access/tokens"`) {
		t.Errorf("Access without sign-in: %d", p.status)
	}
	if p := as(t, ts, "", "GET", "/audit", nil); p.status != 200 {
		t.Errorf("audit without sign-in: %d", p.status)
	}
	// Even a request that claims a role changes nothing.
	for _, path := range []string{"/access/people", "/access/tokens"} {
		if p := as(t, ts, "owner", "POST", path, url.Values{"subject": {"gitlab:gitlab.example.com/9"}, "role": {"owner"}, "name": {"x"}}); p.status != http.StatusForbidden {
			t.Errorf("POST %s without sign-in: %d", path, p.status)
		}
	}
	if reg.RoleOf("gitlab:gitlab.example.com/9") != fleet.Viewer {
		t.Error("a role given without sign-in")
	}
}

func TestHubClusters(t *testing.T) {
	ctx := context.Background()
	ts, reg := hubAdmin(t, true)
	delivered = nil
	if p := as(t, ts, "viewer", "GET", "/clusters/new", nil); p.status != http.StatusForbidden {
		t.Errorf("viewer's form: %d", p.status)
	}
	p := as(t, ts, "operator", "GET", "/clusters/new", nil)
	for _, want := range []string{"Add a cluster", `name="runner_token"`, `type="password"`, `data-when="storage=hostPath"`, "Build cache disk", "S3 cache for the runner"} {
		if !strings.Contains(p.body, want) {
			t.Errorf("form: no %q", want)
		}
	}
	form := url.Values{"name": {"prod-ap"}, "joins": {"token"}, "token_hours": {"24"}, "storage": {"hostPath"}, "host_path": {"/var/mnt/nvme/buildkit"},
		"cache_limit": {"400GB"}, "gitlab_url": {"https://gitlab.example.com/"}, "runner_token": {"glrt-not-a-real-token"}, "concurrent": {"40"},
		"s3_server": {"s3.example.com"}, "s3_bucket": {"ci-cache"}, "s3_region": {"eu-central-1"}, "s3_access_key": {"AKIAEXAMPLE"}, "s3_secret_key": {"very-secret-key"}}
	p = as(t, ts, "operator", "POST", "/clusters", form)
	if p.status != 200 || p.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d %s", p.status, p.body)
	}
	for _, want := range []string{"Install prod-ap", "create secret generic fleet-join --from-literal=token=cia_join_", "prod-ap.values.yaml",
		"fleet-runner-token", "/var/mnt/nvme/buildkit", "helm upgrade --install ci-accelerator"} {
		if !strings.Contains(p.body, want) {
			t.Errorf("install page: no %q", want)
		}
	}
	for _, secret := range []string{"glrt-not-a-real-token", "very-secret-key", "AKIAEXAMPLE"} {
		if strings.Contains(p.body, secret) {
			t.Errorf("a secret on the install page: %s", secret)
		}
	}
	if js, _ := reg.JoinTokens(ctx, "prod-ap"); len(js) != 1 || js[0].CreatedBy != "gitlab:gitlab.example.com/2" {
		t.Errorf("join tokens: %+v", js)
	}
	if set, err := reg.Settings(ctx, "prod-ap"); err != nil || len(set.Secrets) != 3 {
		t.Errorf("kept: %+v %v", set, err)
	}
	if again := as(t, ts, "operator", "POST", "/clusters", form); again.status != http.StatusUnprocessableEntity || !strings.Contains(again.body, "waiting to join") {
		t.Errorf("the same name again: %d", again.status)
	}
	bad := url.Values{"name": {"eu2"}, "joins": {"token"}, "storage": {"hostPath"}, "host_path": {"relative"}, "gitlab_url": {"https://g/"}, "runner_token": {"nope"}}
	if b := as(t, ts, "operator", "POST", "/clusters", bad); b.status != http.StatusUnprocessableEntity || !strings.Contains(b.body, "an absolute path") ||
		!strings.Contains(b.body, "a runner authentication token") || strings.Contains(b.body, "nope") || !strings.Contains(b.body, `value="relative"`) {
		t.Errorf("refused values: %d", b.status)
	}

	m := as(t, ts, "operator", "GET", "/", nil)
	for _, want := range []string{"Waiting to join", `href="/clusters/prod-ap"`, `href="/clusters/new"`} {
		if !strings.Contains(m.body, want) {
			t.Errorf("Members: no %q", want)
		}
	}
	c := as(t, ts, "operator", "GET", "/clusters/prod-ap", nil)
	for _, want := range []string{"waiting to join", "Runner token", "not yet", "New join token", "/var/mnt/nvme/buildkit", "valid until"} {
		if !strings.Contains(c.body, want) {
			t.Errorf("cluster page: no %q", want)
		}
	}
	if j := as(t, ts, "operator", "POST", "/clusters/prod-ap/join", url.Values{}); j.status != 200 || !strings.Contains(j.body, "cia_join_") {
		t.Errorf("a new join token: %d", j.status)
	}
	if js, _ := reg.JoinTokens(ctx, "prod-ap"); len(js) != 2 || js[1].RevokedAt.IsZero() {
		t.Errorf("the older token is not revoked: %+v", js)
	}
	e := as(t, ts, "operator", "GET", "/clusters/prod-ap/edit", nil)
	if !strings.Contains(e.body, "saved · leave empty to keep") || strings.Contains(e.body, `name="joins"`) || strings.Contains(e.body, `id="cf0">Cluster<`) {
		t.Errorf("the change form")
	}
	ch := url.Values{"storage": {"emptyDir"}, "gitlab_url": {"https://gitlab.example.com/"}, "concurrent": {"8"}}
	u := as(t, ts, "operator", "POST", "/clusters/prod-ap", ch)
	// Not joined yet: install commands, and where its join token comes from.
	if u.status != 200 || !strings.Contains(u.body, "Install prod-ap") || !strings.Contains(u.body, "kubectl create namespace") ||
		!strings.Contains(u.body, "New join token") || strings.Contains(u.body, "cia_join_") || strings.Contains(u.body, "fleet-key") {
		t.Errorf("change: %d", u.status)
	}
	if len(delivered) != 1 || delivered[0] != "prod-ap" {
		t.Errorf("delivery after a change: %v", delivered)
	}
	if set, _ := reg.Settings(ctx, "prod-ap"); !set.Kept()["runner_token"] || set.Kept()["s3_access_key"] || set.Values["concurrent"] != "8" {
		t.Errorf("after the change: %+v", set)
	}
	if v := as(t, ts, "viewer", "POST", "/clusters/prod-ap/remove", url.Values{}); v.status != http.StatusForbidden {
		t.Errorf("a viewer removes: %d", v.status)
	}
	if r := as(t, ts, "operator", "POST", "/clusters/prod-ap/remove", url.Values{}); r.status != http.StatusSeeOther {
		t.Errorf("remove: %d", r.status)
	}
	if _, err := reg.Settings(ctx, "prod-ap"); err == nil {
		t.Error("settings kept after removal")
	}
	if js, _ := reg.JoinTokens(ctx, "prod-ap"); js[0].RevokedAt.IsZero() {
		t.Error("an unused join token survives removal")
	}
}

// oneGitLab is sign-in through one GitLab, for the sign-in page alone.
type oneGitLab struct{ Signer }

func (oneGitLab) Instances() []auth.Choice {
	return []auth.Choice{{Instance: "0a1b2c", Host: "gitlab.example.com"}}
}
func (oneGitLab) PublicURL() string { return "https://hub.example.com" }

// A hub nobody may manage yet says so on its sign-in page, instead of
// sending people straight to GitLab; once there is an owner it does not.
func TestHubWithoutOwners(t *testing.T) {
	owners := false
	srv, err := New(Empty{}, testLogger(), Options{Fleet: fakeFleet{}, Auth: oneGitLab{}, Hub: &Hub{NoOwners: func() bool { return !owners }}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	p := get(t, ts, "/auth/sign-in")
	if p.status != 200 || !strings.Contains(p.body, "This hub has no owners yet") || !strings.Contains(p.body, "Sign in to the fleet") {
		t.Errorf("no owners: %d", p.status)
	}
	owners = true
	if p := get(t, ts, "/auth/sign-in"); p.status != http.StatusFound || strings.Contains(p.body, "no owners") {
		t.Errorf("with an owner: %d", p.status)
	}
}
