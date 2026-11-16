package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func openTestRegistry(t *testing.T) *fleet.Registry {
	t.Helper()
	reg, err := fleet.OpenRegistry(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	return reg
}

// lockedBuffer is a log written by handlers and read by the test.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// linked registers a member of that name and links it, in this process,
// to a hub whose new links go to up; the member answers the hub with h.
func linked(t *testing.T, reg *fleet.Registry, name string, up func(*fleet.Link), h http.Handler) {
	t.Helper()
	hpriv, hpub, _ := fleet.GenerateKey()
	hubKey, _ := fleet.ParsePrivateKey(hpriv)
	mpriv, mpub, _ := fleet.GenerateKey()
	memberKey, _ := fleet.ParsePrivateKey(mpriv)
	if _, err := reg.Add(context.Background(), "x", fleet.OriginAPI, name, string(mpub)); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET "+fleet.ConnectPath, &fleet.Hub{Key: hubKey, Member: reg.Holder, Up: up, Log: quiet()})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	keys, _ := fleet.ParsePublicKeys(hpub)
	d := &fleet.Dialer{Hub: srv.URL, Name: name, Store: "0123456789ab", Version: "test", Key: memberKey, HubKeys: keys, Handler: h, Log: quiet()}
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	// The member stops before the server and the registry close (cleanups
	// run last first): a link still dialing would otherwise race the
	// removal of the registry's directory.
	t.Cleanup(func() {
		cancel()
		select {
		case <-ran:
		case <-time.After(10 * time.Second):
			t.Error("the member's dialer did not stop")
		}
	})
	go func() {
		defer close(ran)
		d.Run(ctx)
	}()
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal(what)
}

// The hub's own authentication, as it serves it: the bootstrap token
// while its file holds it, issued tokens with their roles, a person with
// theirs; refused tokens are logged with the caller's address.
func TestHubAuthentication(t *testing.T) {
	ctx := context.Background()
	reg := openTestRegistry(t)
	logs := &lockedBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	tokFile := filepath.Join(t.TempDir(), "token")
	first := strings.Repeat("a", 30)
	os.WriteFile(tokFile, []byte(first+"\n"), 0o600)
	priv, _, _ := fleet.GenerateKey()
	key, _ := fleet.ParsePrivateKey(priv)
	h, top, err := newHub(ctx, hubConfig{reg: reg, log: log, key: key, token: &bootstrapToken{path: tokFile, log: log}, version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	call := func(authz, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		top.ServeHTTP(rec, req)
		return rec
	}
	const members = "/api/v1/fleet/members"
	if r := call("Bearer "+first, members); r.Code != 200 {
		t.Fatalf("bootstrap token: %d %s", r.Code, r.Body)
	}
	if r := call("bearer "+first, members); r.Code != 200 {
		t.Errorf("the scheme in lower case: %d", r.Code)
	}
	if r := call("", members); r.Code != 401 || !strings.HasPrefix(r.Header().Get("Content-Type"), "application/json") {
		t.Errorf("no credential: %d %q", r.Code, r.Header().Get("Content-Type"))
	}
	if r := call("Bearer ", members); r.Code != 401 {
		t.Errorf("an empty token: %d", r.Code)
	}
	if r := call("Bearer "+strings.Repeat("b", 30), members); r.Code != 401 {
		t.Errorf("a wrong token: %d", r.Code)
	}
	if l := logs.String(); !strings.Contains(l, "an API token was refused") || !strings.Contains(l, "192.0.2.1") {
		t.Errorf("a refused token is not logged with its address: %s", l)
	}
	_, viewerTok, err := reg.IssueToken(ctx, "x", "v", fleet.Viewer, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, opTok, _ := reg.IssueToken(ctx, "x", "op", fleet.Operator, 0)
	if r := call("Bearer "+viewerTok, members); r.Code != 403 {
		t.Errorf("a viewer's token on members: %d", r.Code)
	}
	if r := call("Bearer "+opTok, members); r.Code != 200 {
		t.Errorf("an operator's token on members: %d", r.Code)
	}
	if r := call("Bearer "+opTok, "/api/v1/fleet/tokens"); r.Code != 403 {
		t.Errorf("an operator's token on tokens: %d", r.Code)
	}
	// The Secret changed: the old token ends at once, the new one works.
	second := strings.Repeat("c", 30)
	os.WriteFile(tokFile, []byte(second), 0o600)
	if r := call("Bearer "+first, members); r.Code != 401 {
		t.Errorf("the replaced bootstrap token: %d", r.Code)
	}
	if r := call("Bearer "+second, members); r.Code != 200 {
		t.Errorf("the new bootstrap token: %d", r.Code)
	}
	os.WriteFile(tokFile, []byte("short"), 0o600)
	if r := call("Bearer short", members); r.Code != 401 {
		t.Errorf("a token too short: %d", r.Code)
	}
	os.Remove(tokFile)
	if r := call("Bearer "+second, members); r.Code != 401 {
		t.Errorf("the bootstrap token after its file went: %d", r.Code)
	}
	// A person signed in has their own role.
	subject := fleet.PersonSubject("g.example.com", 7)
	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(auth.With(req.Context(), &auth.Viewer{Host: "g.example.com", UserID: 7, Username: "bob"}))
	if p := h.who(req); p.Subject != subject || p.Role != fleet.Viewer {
		t.Errorf("a person without a binding: %+v", p)
	}
	if _, err := reg.Grant(ctx, fleet.OriginLocal, fleet.OriginLocal, subject, fleet.Operator); err != nil {
		t.Fatal(err)
	}
	if p := h.who(req); p.Role != fleet.Operator {
		t.Errorf("an operator: %+v", p)
	}
	// A cluster's address without its slash keeps its query.
	if r := call("", "/m/eu?project=1"); r.Code/100 != 3 || r.Header().Get("Location") != "/m/eu/?project=1" {
		t.Errorf("/m/eu: %d %q", r.Code, r.Header().Get("Location"))
	}
	// Ready while the registry answers.
	if r := call("", "/healthz"); r.Code != 200 {
		t.Errorf("healthz: %d", r.Code)
	}
	reg.Close()
	if r := call("", "/healthz"); r.Code != http.StatusServiceUnavailable {
		t.Errorf("healthz, the registry closed: %d", r.Code)
	}
}

// A link whose controller never answers is not reported connected, and
// goes from the hub's table when it ends.
func TestHubForgetsALinkWhoseControllerFails(t *testing.T) {
	reg := openTestRegistry(t)
	h := &hub{reg: reg, log: quiet(), links: map[string]*memberLink{}, seen: fleet.NewLastKnown(100, 1<<20),
		delivering: map[string]*sync.Mutex{}, retry: map[string]*retryState{}, forced: map[string]bool{}, dialWait: 300 * time.Millisecond}
	first := make(chan *fleet.Link, 1)
	var n atomic.Int32
	up := func(l *fleet.Link) {
		if n.Add(1) > 1 {
			l.Close()
			return
		}
		h.up(l)
		first <- l
	}
	linked(t, reg, "eu", up, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not now", http.StatusInternalServerError)
	}))
	var l *fleet.Link
	select {
	case l = <-first:
	case <-time.After(10 * time.Second):
		t.Fatal("the member did not connect")
	}
	joined, err := reg.Get(context.Background(), "eu")
	if err != nil {
		t.Fatal(err)
	}
	if s := h.status("eu"); s.Connected {
		t.Errorf("connected while its controller is asked: %+v", s)
	}
	select {
	case <-l.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the link stayed up")
	}
	eventually(t, "the ended link stays in the hub's table", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.links["eu"] == nil
	})
	// The hub records the visit's end after it takes the link out of its
	// table: wait for that write, or the registry closes under it and its
	// directory cannot be removed.
	eventually(t, "the link's end is recorded", func() bool {
		m, err := reg.Get(context.Background(), "eu")
		return err == nil && m.SeenAt.After(joined.SeenAt)
	})
	if s := h.status("eu"); s.Connected {
		t.Errorf("connected after the link ended: %+v", s)
	}
	for _, m := range (view{h, ""}).Members() {
		if m.Name == "eu" && (m.State != "offline" || m.SeenAt.IsZero()) {
			t.Errorf("Members: %+v", m)
		}
	}
}

// An open page's stream ends with its member's link: the page then
// reconnects to the next one.
func TestHubLinkEndEndsItsPageStreams(t *testing.T) {
	h, mux, reg := pagesHub(t)
	register(t, reg, "eu")
	lctx, lcancel := context.WithCancel(context.Background())
	defer lcancel()
	started := make(chan struct{})
	h.links["eu"] = &memberLink{ctx: lctx, stop: lcancel, pages: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		close(started)
		<-r.Context().Done()
	})}
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/m/eu/events", nil))
		close(done)
	}()
	<-started
	lcancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the page's stream outlived its link")
	}
}

// A page request that ends after its cluster was removed leaves no kept
// copy behind.
func TestHubRemovalForgetsAPageInFlight(t *testing.T) {
	h, mux, reg := pagesHub(t)
	register(t, reg, "eu")
	entered, release := make(chan struct{}), make(chan struct{})
	h.links["eu"] = &memberLink{pages: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<div class="page">builds of eu</div>`)
	})}
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/m/eu/builds", nil))
		close(done)
	}()
	<-entered
	if _, err := reg.RemoveCluster(context.Background(), "x", "eu"); err != nil {
		t.Fatal(err)
	}
	h.drop("eu")
	close(release)
	<-done
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/m/eu/builds", nil))
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "builds of eu") {
		t.Errorf("a removed cluster's page: %d %q", rec.Code, rec.Body)
	}
}

// A failed delivery is tried again while the link is up; only what was
// sent counts as written; a delivery that changes nothing is not audited.
func TestHubDeliveryRetried(t *testing.T) {
	ctx := context.Background()
	reg := openTestRegistry(t)
	sealer, err := fleet.NewSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	h := &hub{reg: reg, log: quiet(), sealer: sealer, links: map[string]*memberLink{}, seen: fleet.NewLastKnown(100, 1<<20),
		delivering: map[string]*sync.Mutex{}, retry: map[string]*retryState{}, forced: map[string]bool{}}
	var puts atomic.Int32
	member := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != SecretsPath {
			http.NotFound(w, r)
			return
		}
		var in struct {
			Secrets map[string]map[string]string `json:"secrets"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if puts.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"written":[],"error":"the cluster's API did not answer"}`)
			return
		}
		names := []string{"made-up"}
		for n := range in.Secrets {
			names = append(names, n)
		}
		json.NewEncoder(w).Encode(map[string]any{"written": names})
	})
	links := make(chan *fleet.Link, 4)
	linked(t, reg, "eu", func(l *fleet.Link) { links <- l }, member)
	var l *fleet.Link
	select {
	case l = <-links:
	case <-time.After(10 * time.Second):
		t.Fatal("the member did not connect")
	}
	if _, err := reg.SaveSettings(ctx, sealer, "x", "eu", map[string]string{"gitlab_url": "https://gitlab.example.com/",
		"storage": "emptyDir", "runner_token": "glrt-not-a-real-token"}); err != nil {
		t.Fatal(err)
	}
	lctx, lcancel := context.WithCancel(ctx)
	defer lcancel()
	h.links["eu"] = &memberLink{link: l, ctx: lctx, stop: lcancel, pages: http.NotFoundHandler()}

	if h.deliver(ctx, "eu", false) {
		t.Error("a failed delivery counts as done")
	}
	if u, _ := reg.Undelivered(ctx, "eu"); !u {
		t.Fatal("delivered after a failure")
	}
	// Not due yet: nothing sent.
	h.retryDeliveries(ctx, time.Now())
	time.Sleep(200 * time.Millisecond)
	if n := puts.Load(); n != 1 {
		t.Errorf("tried again before its time: %d deliveries", n)
	}
	h.retryDeliveries(ctx, time.Now().Add(retryFirst+time.Second))
	eventually(t, "the failed delivery was not tried again", func() bool {
		u, err := reg.Undelivered(ctx, "eu")
		return err == nil && !u
	})
	// Wait for the delivery in flight to finish.
	func() {
		mu := h.deliveryLock("eu")
		mu.Lock()
		defer mu.Unlock()
	}()
	h.mu.Lock()
	if h.retry["eu"] != nil {
		t.Error("a delivery that went through is still due")
	}
	h.mu.Unlock()
	deliveries := func() (n int, detail string) {
		evs, _ := reg.AuditQuery(ctx, fleet.AuditQuery{Target: "eu"})
		for _, e := range evs {
			if e.Action == "secrets.deliver" {
				n, detail = n+1, e.Detail
			}
		}
		return n, detail
	}
	if n, detail := deliveries(); n != 1 || strings.Contains(detail, "made-up") || !strings.Contains(detail, fleet.SecretRunner) {
		t.Errorf("audit of the delivery: %d %q", n, detail)
	}
	// Sent again (a member installed anew): written, not audited again.
	if !h.deliver(ctx, "eu", true) || puts.Load() != 3 {
		t.Errorf("a forced delivery: %d deliveries", puts.Load())
	}
	if n, _ := deliveries(); n != 1 {
		t.Errorf("a delivery that changed nothing was audited: %d records", n)
	}
	// The wait doubles up to its bound.
	h.retried("us", false, time.Now())
	h.retried("us", false, time.Now())
	if w := h.retry["us"].wait; w != 2*retryFirst {
		t.Errorf("second wait %v", w)
	}
	for range 10 {
		h.retried("us", false, time.Now())
	}
	if w := h.retry["us"].wait; w != retryMax {
		t.Errorf("wait bound %v", w)
	}
}

// A restore's tokens are void at the hub's start: a file put in place by
// hand, and one staged whose first start died before the registry was
// open.
func TestOpenHubRegistryVoidsRestoredTokens(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	reg, err := fleet.OpenRegistry(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := reg.IssueToken(ctx, "x", "tf", fleet.Owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	reg.Close()
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := hubCommand("backup", []string{"-data", dir, backup}); err != nil {
		t.Fatal(err)
	}
	reg, _ = fleet.OpenRegistry(ctx, dir)
	if err := reg.RevokeToken(ctx, "x", "tf"); err != nil {
		t.Fatal(err)
	}
	reg.Close()

	b, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "fleet.db.restore"), b, 0o600)
	reg, err = openHubRegistry(ctx, dir, time.Now(), quiet())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.TokenPrincipal(ctx, secret); ok {
		t.Error("a token of a backup put in place by hand is valid after the start")
	}
	reg.Close()

	if err := hubCommand("restore", []string{"-data", dir, backup}); err != nil {
		t.Fatal(err)
	}
	if _, applied, err := fleet.ApplyRestore(dir, time.Now().Add(time.Second)); err != nil || !applied {
		t.Fatalf("apply: %v %v", applied, err)
	}
	// That start died here; the next one finds nothing staged.
	reg, err = openHubRegistry(ctx, dir, time.Now().Add(2*time.Second), quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if _, ok := reg.TokenPrincipal(ctx, secret); ok {
		t.Error("a token of a staged backup is valid after a start that died")
	}
}
