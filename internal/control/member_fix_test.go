package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// A member's controller never receives the session of the page the hub
// serves: it is the hub's credential. The web's own controller still does.
func TestMemberClientSendsNoSession(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get(SessionHeader) + r.Header.Get("Cookie")
		mu.Unlock()
		if r.URL.Path == "/internal/v1/config" {
			w.Write([]byte(`{"Release":"box"}`))
			return
		}
		w.Write([]byte(`{"r":[null]}`))
	}))
	defer ts.Close()
	signed := auth.WithCookie(ctx, "HUB-SESSION")
	for _, member := range []bool{true, false} {
		clear(seen)
		c, err := Dial(signed, ClientOptions{URL: ts.URL, Member: member})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Job(signed, "0a1b2c", 12345); err != nil {
			t.Fatal(err)
		}
		if _, err := c.PipelineInfo(signed, "0a1b2c", 7); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		for path, got := range seen {
			if member && got != "" {
				t.Errorf("member: %s received %q", path, got)
			}
			if !member && path != "/internal/v1/config" && got != "HUB-SESSION" {
				t.Errorf("the web's controller: %s received %q, want the session", path, got)
			}
		}
		mu.Unlock()
	}
}

// A member that answers without end is cut off at MaxMemberAnswer.
func TestMemberAnswerIsBounded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/config" {
			w.Write([]byte(`{"Release":"box"}`))
			return
		}
		w.Write([]byte(`{"r":["`))
		chunk := []byte(strings.Repeat("a", 1<<20))
		for range 4 * MaxMemberAnswer >> 20 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer ts.Close()
	c, err := Dial(ctx, ClientOptions{URL: ts.URL, Member: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Output(ctx, 1, 0)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("an endless answer: %v", err)
	}
	// Answers within the bound are read as before.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/config" {
			w.Write([]byte(`{"Release":"box"}`))
			return
		}
		b, _ := json.Marshal(strings.Repeat("b", 4<<20))
		w.Write([]byte(`{"r":[` + string(b) + `]}`))
	}))
	defer ok.Close()
	c, err = Dial(ctx, ClientOptions{URL: ok.URL, Member: true})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := c.Output(ctx, 1, 0); err != nil || len(out) != 4<<20 {
		t.Fatalf("a 4 MiB answer: %d %v", len(out), err)
	}
}

// The hub's pages write nothing into a member's store.
func TestMemberClientWritesNothing(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/internal/v1/config" {
			w.Write([]byte(`{"Release":"box"}`))
			return
		}
		w.Write([]byte(`{"r":[]}`))
	}))
	defer ts.Close()
	c, err := Dial(ctx, ClientOptions{URL: ts.URL, Member: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SaveCompare(ctx, store.ProjectKey{Instance: "0a1b2c", ID: 1}, "a", "b", nil, true); err != nil {
		t.Fatal(err)
	}
	if err := c.SettleElsewhere(ctx, []store.Settled{{ID: 1, Kind: "files-changed"}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("calls %v, want the config only", calls)
	}
}

// cookieSeen records the session a call reached the controller's parts
// with.
type cookieSeen struct {
	daemons
	mu  sync.Mutex
	got []string
}

func (c *cookieSeen) LiveBuild(ctx context.Context, daemon, ref string) (*collector.LiveBuild, error) {
	c.mu.Lock()
	c.got = append(c.got, auth.CookieFrom(ctx))
	c.mu.Unlock()
	return nil, nil
}

// A hub over the link reads what the web reads, but none of the store's
// writes or the sign-in, and a session it sends is not taken.
func TestHubRole(t *testing.T) {
	seen := &cookieSeen{}
	role := RoleHub
	srv := &Server{Local: &Local{Store: newStore(t), Live: seen}, Config: Config{Release: "box"},
		Authorize: func(*http.Request) (Role, error) { return role, nil }}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	post := func(method, body string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/internal/v1/call/"+method, strings.NewReader(body))
		req.Header.Set(SessionHeader, "A-SESSION")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for _, m := range []string{"SaveCompare", "SettleElsewhere"} {
		if got := post(m, `[]`); got != http.StatusForbidden {
			t.Errorf("hub: %s answered %d, want 403", m, got)
		}
	}
	if got := post("Projects", `[]`); got != http.StatusOK {
		t.Errorf("hub: Projects answered %d", got)
	}
	if got := post("LiveBuild", `["d1","r1"]`); got != http.StatusOK {
		t.Errorf("hub: LiveBuild answered %d", got)
	}
	res, err := http.Get(ts.URL + "/internal/v1/config")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("hub: config answered %d", res.StatusCode)
	}
	// The web keeps its writes and its session.
	role = RoleWeb
	if got := post("SettleElsewhere", `[[]]`); got != http.StatusOK {
		t.Errorf("web: SettleElsewhere answered %d", got)
	}
	if got := post("LiveBuild", `["d1","r1"]`); got != http.StatusOK {
		t.Errorf("web: LiveBuild answered %d", got)
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if len(seen.got) != 2 || seen.got[0] != "" || seen.got[1] != "A-SESSION" {
		t.Fatalf("sessions the calls carried: %q, want none for the hub, the web's for the web", seen.got)
	}
	if !RoleHub.may(RoleWeb) || RoleHub.may(RoleRouter) || RoleWeb.may(RoleHub) {
		t.Error("roles")
	}
}

// Stating the API's version leaves the setup digest as releases before it
// computed it: a web and a controller of the two still agree.
func TestAPIVersionKeepsTheSetup(t *testing.T) {
	type before struct {
		Version, Release     string
		GitLab, InstallToken bool
		Known                []string
		SignIn               bool
		PublicURL            string
		Instances            []auth.Choice
		Idle                 time.Duration
	}
	old := before{Release: "box", GitLab: true, Known: []string{"0a1b2c"}, Idle: time.Hour}
	b, _ := json.Marshal(old)
	sum := sha256.Sum256(b)
	want := hex.EncodeToString(sum[:8])
	now := Config{Version: "v2", Release: "box", API: APIVersion, GitLab: true, Known: []string{"0a1b2c"}, Idle: time.Hour}
	if got := now.Setup(); got != want {
		t.Fatalf("setup %s, want %s as before", got, want)
	}
	if ConfigFor("v", "box", nil, nil).API != APIVersion {
		t.Error("the controller does not state its API version")
	}
}

// A member's client bounds what it reads: the configuration at dial and
// a call's results (the box's own client reads its controller whole).
func TestEveryAnswerIsBounded(t *testing.T) {
	endless := func(w http.ResponseWriter, prefix string) {
		w.Write([]byte(prefix))
		chunk := []byte(strings.Repeat("a", 1<<20))
		for range 2 * MaxMemberAnswer >> 20 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/config" {
			w.Write([]byte(`{"Release":"box"}`))
			return
		}
		endless(w, `{"r":["`)
	}))
	defer ts.Close()
	c, err := Dial(ctx, ClientOptions{URL: ts.URL, Member: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Output(ctx, 1, 0); err == nil || !strings.Contains(err.Error(), "larger than 64 MiB") {
		t.Fatalf("an endless call answer: %v", err)
	}
	cfg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endless(w, `{"Release":"`)
	}))
	defer cfg.Close()
	plain := &Client{base: cfg.URL, http: http.DefaultClient, member: true}
	if _, err := plain.config(ctx); err == nil || !strings.Contains(err.Error(), "larger than 64 MiB") {
		t.Fatalf("an endless configuration: %v", err)
	}
}
