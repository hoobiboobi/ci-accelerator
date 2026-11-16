package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
)

// addWaiting adds a cluster at the hub's pages that joins with a token
// (it gets one at once) and keeps its runner token.
func addWaiting(t *testing.T, ts *httptest.Server, name string) {
	t.Helper()
	p := as(t, ts, "operator", "POST", "/clusters", url.Values{"name": {name}, "joins": {"token"}, "storage": {"emptyDir"},
		"gitlab_url": {"https://gitlab.example.com/"}, "runner_token": {"glrt-not-a-real-token"}})
	if p.status != 200 {
		t.Fatalf("add %s: %d", name, p.status)
	}
}

// Removing a cluster takes its name typed on its page, checked by the
// server; the button is apart from the others and says what it does.
func TestHubRemoveNeedsTheName(t *testing.T) {
	ctx := context.Background()
	ts, reg := hubAdmin(t, true)
	addWaiting(t, ts, "ap3")
	c := as(t, ts, "operator", "GET", "/clusters/ap3", nil)
	for _, want := range []string{`<section class="box danger"`, "cannot be undone", "Type ap3 to confirm", `<input type="text" name="name" required`,
		"Voids its 1 unused join token and forgets its settings and 1 kept secret.", "Joining again needs those secrets entered anew.", ">Remove ap3</button>"} {
		if !strings.Contains(c.body, want) {
			t.Errorf("cluster page: no %q", want)
		}
	}
	if n := strings.Count(c.body, `action="/clusters/ap3/remove"`); n != 1 {
		t.Errorf("remove forms: %d", n)
	}
	if strings.Contains(c.body, "Remove from the fleet</button>") {
		t.Error("the one-click button is still there")
	}
	if v := as(t, ts, "viewer", "GET", "/clusters/ap3", nil); strings.Contains(v.body, "Type ap3") {
		t.Error("a viewer is offered the removal")
	}
	for _, c := range []struct{ typed, msg string }{
		{"", "Not removed: type ap3 to confirm."},
		{"ap", `Not removed: the name typed was "ap", not "ap3".`},
		{"AP3", `Not removed: the name typed was "AP3", not "ap3".`},
	} {
		form := url.Values{}
		if c.typed != "" {
			form.Set("name", c.typed)
		}
		p := as(t, ts, "operator", "POST", "/clusters/ap3/remove", form)
		msg, bad := flashOf(p)
		if p.status != http.StatusSeeOther || p.header.Get("Location") != "/clusters/ap3" || !bad || msg != c.msg {
			t.Errorf("typed %q: %d %q %q bad=%v", c.typed, p.status, p.header.Get("Location"), msg, bad)
		}
		if _, err := reg.Settings(ctx, "ap3"); err != nil {
			t.Fatalf("typed %q: removed", c.typed)
		}
	}
	long := strings.Repeat("x", 500)
	if msg, _ := flashOf(as(t, ts, "operator", "POST", "/clusters/ap3/remove", url.Values{"name": {long}})); len(msg) > 200 {
		t.Errorf("a long name typed is said whole: %d bytes", len(msg))
	}
	p := as(t, ts, "operator", "POST", "/clusters/ap3/remove", url.Values{"name": {" ap3 "}})
	if msg, bad := flashOf(p); bad || !strings.Contains(msg, "is removed") {
		t.Errorf("the name typed: %q bad=%v", msg, bad)
	}
	if _, err := reg.Settings(ctx, "ap3"); err == nil {
		t.Error("not removed with the name typed")
	}
}

// Revoking an API token, changing or taking away a role, and a join token
// that voids an unused one are asked once more on one page; a form sent
// without it changes nothing.
func TestHubConfirmPage(t *testing.T) {
	ctx := context.Background()
	ts, reg := hubAdmin(t, true)
	confirmPage := func(what string, p page, want ...string) {
		t.Helper()
		if p.status != 200 || p.header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d", what, p.status)
		}
		for _, w := range append(want, `name="confirm" value="yes"`, ">Cancel</a>") {
			if !strings.Contains(p.body, w) {
				t.Errorf("%s: no %q", what, w)
			}
		}
	}

	// An API token.
	as(t, ts, "owner", "POST", "/access/tokens", url.Values{"name": {"t1"}, "role": {"operator"}, "days": {"30"}})
	tk := as(t, ts, "owner", "GET", "/access/tokens", nil)
	if !strings.Contains(tk.body, `href="/access/tokens?confirm=revoke&amp;name=t1"`) || strings.Contains(tk.body, `action="/access/tokens/revoke"`) {
		t.Error("Revoke is not a link to the confirmation")
	}
	confirmPage("revoke", as(t, ts, "owner", "GET", "/access/tokens?confirm=revoke&name=t1", nil),
		"Revoke the token t1?", `action="/access/tokens/revoke"`, `name="name" value="t1"`, "are refused by the hub&#39;s API from now on", "It was never used.")
	confirmPage("revoke posted", as(t, ts, "owner", "POST", "/access/tokens/revoke", url.Values{"name": {"t1"}}), "Revoke the token t1?")
	active := func() bool {
		all, _ := reg.Tokens(ctx)
		return len(all) == 1 && all[0].RevokedAt.IsZero()
	}
	if !active() {
		t.Fatal("revoked without the confirmation")
	}
	if p := as(t, ts, "owner", "POST", "/access/tokens/revoke", url.Values{"name": {"t1"}, "confirm": {"yes"}}); p.status != http.StatusSeeOther || active() {
		t.Errorf("confirmed: %d", p.status)
	}
	if p := as(t, ts, "owner", "GET", "/access/tokens?confirm=revoke&name=t1", nil); p.status != http.StatusSeeOther {
		t.Errorf("a revoked token asked again: %d", p.status)
	}
	if p := as(t, ts, "operator", "GET", "/access/tokens?confirm=revoke&name=t1", nil); p.status != http.StatusForbidden {
		t.Errorf("an operator's revoke page: %d", p.status)
	}

	// A role changed.
	op := "gitlab:gitlab.example.com/2"
	pp := as(t, ts, "owner", "GET", "/access", nil)
	if !strings.Contains(pp.body, `href="/access?confirm=role&amp;subject=gitlab%3agitlab.example.com%2f2&amp;role=owner"`) {
		t.Error("Make owner is not a link to the confirmation")
	}
	confirmPage("role", as(t, ts, "owner", "GET", "/access?confirm=role&subject="+url.QueryEscape(op)+"&role=owner", nil),
		"an owner?", "is an operator now: clusters, join tokens and the audit.", `action="/access/people"`, `name="role" value="owner"`, ">Make owner</button>")
	confirmPage("role posted", as(t, ts, "owner", "POST", "/access/people", url.Values{"subject": {op}, "role": {"owner"}}), ">Make owner</button>")
	if reg.RoleOf(op) != fleet.Operator {
		t.Fatal("a role changed without the confirmation")
	}
	as(t, ts, "owner", "POST", "/access/people", url.Values{"subject": {op}, "role": {"owner"}, "confirm": {"yes"}})
	if reg.RoleOf(op) != fleet.Owner {
		t.Error("the confirmed change is not made")
	}
	// A role given to someone without one is not a change: made at once.
	if p := as(t, ts, "owner", "POST", "/access/people", url.Values{"gitlab": {"gitlab.example.com"}, "user": {"77"}, "role": {"operator"}}); p.status != http.StatusSeeOther ||
		reg.RoleOf("gitlab:gitlab.example.com/77") != fleet.Operator {
		t.Errorf("a role given: %d", p.status)
	}

	// A role taken away.
	confirmPage("remove role", as(t, ts, "owner", "GET", "/access?confirm=remove-role&subject="+url.QueryEscape(op), nil),
		"Remove the role of ", `action="/access/people/remove"`, ">Remove the role</button>", "change nothing at the hub")
	confirmPage("remove posted", as(t, ts, "owner", "POST", "/access/people/remove", url.Values{"subject": {op}}), ">Remove the role</button>")
	if reg.RoleOf(op) != fleet.Owner {
		t.Fatal("a role taken away without the confirmation")
	}
	as(t, ts, "owner", "POST", "/access/people/remove", url.Values{"subject": {op}, "confirm": {"yes"}})
	if reg.RoleOf(op) != fleet.Viewer {
		t.Error("the confirmed removal is not made")
	}

	// Their own role, an owner from the values, someone without a role:
	// no confirmation, the reason.
	for _, c := range []struct{ who, path, want string }{
		{"owner", "/access?confirm=role&role=operator&subject=" + url.QueryEscape("gitlab:gitlab.example.com/1"), "Your own role is changed by another owner."},
		{"owner2", "/access?confirm=remove-role&subject=" + url.QueryEscape("gitlab:gitlab.example.com/1"), "from the hub's values"},
		{"owner", "/access?confirm=remove-role&subject=" + url.QueryEscape("gitlab:gitlab.example.com/99"), "has no role"},
	} {
		p := as(t, ts, c.who, "GET", c.path, nil)
		if msg, bad := flashOf(p); p.status != http.StatusSeeOther || !bad || !strings.Contains(msg, c.want) {
			t.Errorf("%s %s: %d %q", c.who, c.path, p.status, msg)
		}
	}
	if p := as(t, ts, "operator", "GET", "/access?confirm=remove-role&subject="+url.QueryEscape("gitlab:gitlab.example.com/5"), nil); p.status != http.StatusForbidden {
		t.Errorf("an operator's role page: %d", p.status)
	}

	// A new join token that voids an unused one.
	addWaiting(t, ts, "ap4")
	c := as(t, ts, "operator", "GET", "/clusters/ap4", nil)
	if !strings.Contains(c.body, `href="/clusters/ap4?confirm=join"`) || strings.Contains(c.body, `action="/clusters/ap4/join"`) {
		t.Error("New join token is not a link to the confirmation")
	}
	js, _ := reg.JoinTokens(ctx, "ap4")
	confirmPage("join", as(t, ts, "operator", "GET", "/clusters/ap4?confirm=join", nil),
		"A new join token for ap4?", "Join token #", "stops working", `action="/clusters/ap4/join"`)
	confirmPage("join posted", as(t, ts, "operator", "POST", "/clusters/ap4/join", url.Values{}), "A new join token for ap4?")
	if again, _ := reg.JoinTokens(ctx, "ap4"); len(again) != len(js) {
		t.Fatal("a join token issued without the confirmation")
	}
	if p := as(t, ts, "operator", "POST", "/clusters/ap4/join", url.Values{"confirm": {"yes"}}); p.status != 200 || !strings.Contains(p.body, "cia_join_") {
		t.Errorf("confirmed join: %d", p.status)
	}
	// None unused: nothing to ask, the button sends the form.
	all, _ := reg.JoinTokens(ctx, "ap4")
	for _, j := range all {
		if j.Active(time.Now()) {
			if err := reg.RevokeJoin(ctx, "token:tf", "ap4", j.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if c := as(t, ts, "operator", "GET", "/clusters/ap4", nil); !strings.Contains(c.body, `action="/clusters/ap4/join"`) {
		t.Error("with no unused token the button is not the form")
	}
	if p := as(t, ts, "operator", "POST", "/clusters/ap4/join", url.Values{}); p.status != 200 || !strings.Contains(p.body, "cia_join_") {
		t.Errorf("join with none to void: %d", p.status)
	}
}

// A cluster's page says when its store was replaced, with the date, and
// leads to the audit; it shows the link events the hub kept.
func TestHubClusterStoreAndLinkEvents(t *testing.T) {
	ev := &fleet.LinkEvents{}
	ts, reg := hubAdminWith(t, true, func(h *Hub) { h.Events = ev })
	if _, err := reg.Add(context.Background(), "token:tf", fleet.OriginAPI, "us", pubKey(t)); err != nil {
		t.Fatal(err)
	}
	c := as(t, ts, "operator", "GET", "/clusters/us", nil)
	for _, want := range []string{"Store replaced on <span title=\"2026-10-20 12:00:00 UTC\">Oct 20, 2026 12:00 UTC</span>: builds from before then are gone.",
		`href="/audit?target=us"`, "Link events at the hub", "None since the hub started."} {
		if !strings.Contains(c.body, want) {
			t.Errorf("cluster page: no %q", want)
		}
	}
	ev.Add("us", "refused, from 203.0.113.7:51544: it speaks protocol 2; this hub speaks 1")
	ev.Add("us", "closed after 3h0m0s, from 203.0.113.7:51544")
	c = as(t, ts, "operator", "GET", "/clusters/us", nil)
	i, j := strings.Index(c.body, "closed after 3h0m0s"), strings.Index(c.body, "it speaks protocol 2; this hub speaks 1")
	if i < 0 || j < 0 || i > j {
		t.Errorf("link events, newest first: %d %d", i, j)
	}
	if e := as(t, ts, "operator", "GET", "/clusters/eu", nil); strings.Contains(e.body, "Store replaced") || strings.Contains(e.body, "closed after") {
		t.Error("another cluster's store or events")
	}
	// Without kept events the section is not there.
	ts2, reg2 := hubAdmin(t, true)
	reg2.Add(context.Background(), "token:tf", fleet.OriginAPI, "us", pubKey(t))
	if c := as(t, ts2, "operator", "GET", "/clusters/us", nil); strings.Contains(c.body, "Link events") {
		t.Error("link events shown without a store of them")
	}
}

// storeFleet is a hub's fleet whose cluster us came back with another
// store.
type storeFleet struct{ fakeFleet }

func (storeFleet) Members() []FleetMember {
	ms := fakeFleet{}.Members()
	for i := range ms {
		switch ms[i].Name {
		case "us":
			ms[i].Store = "a71e00000000"
		case "eu":
			ms[i].Store = "3f9c00000000"
		case "old":
			ms[i].Store = "0000aaaaaaaa"
		}
	}
	return ms
}

// An address with the mark of a store the cluster no longer has says the
// build is gone, with the date; other pages lead to the cluster now.
func TestOldStoreAddresses(t *testing.T) {
	srv, err := New(Empty{}, testLogger(), Options{Fleet: storeFleet{}})
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return testNow }
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seg, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/m/"), "/")
		if !srv.OldStore(w, r, seg) {
			w.Write([]byte("served by the cluster"))
		}
	}))
	defer ts.Close()
	p := get(t, ts, "/m/us.3f9c/builds/27")
	if p.status != http.StatusGone || !strings.Contains(p.body, "This build is gone") ||
		!strings.Contains(p.body, "The store of us was replaced on Oct 20, 2026 12:00 UTC: builds from before then are not kept, and build 27 of its store now is another build.") ||
		!strings.Contains(p.body, `href="/m/us/builds"`) {
		t.Errorf("an old build: %d %s", p.status, p.body)
	}
	if p := get(t, ts, "/m/us.3f9c/builds/27/steps/3"); p.status != http.StatusGone {
		t.Errorf("an old build's step: %d", p.status)
	}
	if p := get(t, ts, "/m/us.3f9c/pipelines?ref=main"); p.status != http.StatusFound || p.header.Get("Location") != "/m/us.a71e/pipelines?ref=main" {
		t.Errorf("another page: %d %q", p.status, p.header.Get("Location"))
	}
	if p := get(t, ts, "/m/us.3f9c/builds"); p.status != http.StatusFound || p.header.Get("Location") != "/m/us.a71e/builds" {
		t.Errorf("the builds list: %d %q", p.status, p.header.Get("Location"))
	}
	// Never replaced: no date is claimed.
	if p := get(t, ts, "/m/eu.a71e/builds/5"); p.status != http.StatusGone || strings.Contains(p.body, "replaced on") || !strings.Contains(p.body, "does not have now") {
		t.Errorf("an unknown store: %d", p.status)
	}
	for _, path := range []string{"/m/us.a71e/builds/27", "/m/us/builds/27", "/m/ap.1234/builds/1", "/m/old.ffff/builds/1", "/m/ghost.1234/builds/1"} {
		if p := get(t, ts, path); p.status != 200 || p.body != "served by the cluster" {
			t.Errorf("%s: %d", path, p.status)
		}
	}
	for store, want := range map[string]string{"3f9c00000000": "/m/eu.3f9c", "": "/m/eu", "XYZW00000000": "/m/eu", "ab": "/m/eu"} {
		if got := MemberBase("eu", store); got != want {
			t.Errorf("MemberBase(%q): %q", store, got)
		}
	}
	if n, m := SplitMember("lab-fa.3f9c"); n != "lab-fa" || m != "3f9c" {
		t.Errorf("SplitMember: %q %q", n, m)
	}
}
