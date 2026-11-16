package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
	"github.com/hoobiboobi/ci-accelerator/internal/web"
)

// A cluster's address at the hub: its script and stylesheet always from
// the hub, its pages from the cluster, and while it is away the page the
// same person saw before, dated, or the away page.
func TestHubMemberPages(t *testing.T) {
	reg, err := fleet.OpenRegistry(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &hub{reg: reg, log: log, links: map[string]*memberLink{}, seen: fleet.NewLastKnown(100, 1<<20)}
	root, err := web.New(web.Empty{}, log, web.Options{Fleet: view{h, ""}})
	if err != nil {
		t.Fatal(err)
	}
	h.root, h.rootMux = root, http.NewServeMux()
	root.Register(h.rootMux)
	mux := http.NewServeMux()
	mux.HandleFunc("/m/{name}/", h.member)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	if r := get("/m/eu/static/console.css"); r.Code != 200 || !strings.Contains(r.Header().Get("Content-Type"), "css") {
		t.Fatalf("static while away: %d %q", r.Code, r.Header().Get("Content-Type"))
	}
	if r := get("/m/eu/builds"); r.Code != http.StatusNotFound || !strings.Contains(r.Body.String(), "No cluster named eu") {
		t.Fatalf("never seen: %d", r.Code)
	}
	// Connected: its pages answer and are kept.
	h.links["eu"] = &memberLink{pages: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<div class="page">builds of eu at `+r.URL.Path+`</div>`)
	})}
	if r := get("/m/eu/builds"); r.Code != 200 || !strings.Contains(r.Body.String(), "builds of eu at /builds") {
		t.Fatalf("connected: %d %q", r.Code, r.Body)
	}
	delete(h.links, "eu")
	r := get("/m/eu/builds")
	if r.Code != 200 || !strings.Contains(r.Body.String(), "eu is not connected now.") || !strings.Contains(r.Body.String(), "builds of eu at /builds") || r.Header().Get("X-Fleet-Last-Known") == "" {
		t.Fatalf("away, seen before: %d %q", r.Code, r.Body)
	}
	if r := get("/m/eu/images"); r.Code != http.StatusNotFound {
		t.Fatalf("away, a page not seen before: %d", r.Code)
	}
	// Removed: nothing kept is shown.
	h.drop("eu")
	if r := get("/m/eu/builds"); r.Code != http.StatusNotFound {
		t.Fatalf("removed: %d", r.Code)
	}
}

// A box joining with a join token makes its key, keeps it in its Secret
// first, then trades the token (memberKey keeps trading until the hub
// answers); after a restart it uses the kept key and trading again is done
// already.
func TestMemberJoinsWithAToken(t *testing.T) {
	ctx := context.Background()
	reg, err := fleet.OpenRegistry(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	mux := http.NewServeMux()
	mux.Handle("POST "+fleet.JoinPath, reg.JoinHandler())
	ts := httptest.NewTLSServer(mux)
	defer ts.Close()
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	outer := &tls.Config{RootCAs: pool}

	_, secret, err := reg.IssueJoin(ctx, "gitlab:g/1", "eu", 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	os.WriteFile(tokenFile, []byte(secret+"\n"), 0o600)
	c := config{fleetHub: ts.URL, fleetName: "eu", fleetKey: filepath.Join(dir, "absent", "key"), fleetJoin: tokenFile,
		fleetKeySecret: "fleet-key", namespace: "box"}
	kube := kubefake.NewClientset()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	key, err := memberKey(ctx, c, kube, outer, log)
	if err != nil {
		t.Fatal(err)
	}
	fp := fleet.Fingerprint(key.Public())
	if n, err := reg.Holder(fp); err != nil || n != "eu" {
		t.Fatalf("registered: %q %v", n, err)
	}
	s, err := kube.CoreV1().Secrets("box").Get(ctx, "fleet-key", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if k, err := fleet.ParsePrivateKey(s.Data["key"]); err != nil || fleet.Fingerprint(k.Public()) != fp {
		t.Fatalf("the kept key is another one: %v", err)
	}
	// A restart: the Secret not mounted yet, the token used.
	again, err := memberKey(ctx, c, kube, outer, log)
	if err != nil || fleet.Fingerprint(again.Public()) != fp {
		t.Errorf("after a restart: %v", err)
	}
	// The key file (the Secret, mounted) wins, and the token is traded
	// again harmlessly.
	keyFile := filepath.Join(dir, "key")
	os.WriteFile(keyFile, s.Data["key"], 0o600)
	c.fleetKey = keyFile
	if k, err := memberKey(ctx, c, kube, outer, log); err != nil || fleet.Fingerprint(k.Public()) != fp {
		t.Errorf("from the mounted key: %v", err)
	}
	// Without a key and without a token: a clear error.
	c2 := config{fleetKey: filepath.Join(dir, "absent", "key")}
	if _, err := memberKey(ctx, c2, kube, outer, log); err == nil {
		t.Error("no key, no token: no error")
	}
	// A crash after keeping the key, before trading: the next start trades
	// with the kept key.
	_, s2, _ := reg.IssueJoin(ctx, "gitlab:g/1", "us", 0)
	priv, _, _ := fleet.GenerateKey()
	kube.CoreV1().Secrets("box").Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "us-key", Namespace: "box"},
		Data: map[string][]byte{"key": priv}}, metav1.CreateOptions{})
	os.WriteFile(tokenFile, []byte(s2), 0o600)
	c3 := config{fleetHub: ts.URL, fleetName: "us", fleetKey: filepath.Join(dir, "absent", "key"), fleetJoin: tokenFile,
		fleetKeySecret: "us-key", namespace: "box"}
	k3, err := memberKey(ctx, c3, kube, outer, log)
	if err != nil {
		t.Fatal(err)
	}
	if kept, _ := fleet.ParsePrivateKey(priv); fleet.Fingerprint(k3.Public()) != fleet.Fingerprint(kept.Public()) {
		t.Error("the kept key was not used")
	}
	if n, _ := reg.Holder(fleet.Fingerprint(k3.Public())); n != "us" {
		t.Errorf("us not registered with its kept key: %q", n)
	}
}

// A cluster's pages at the hub know the hub's roles: an owner sees the
// Fleet links and is named an owner there too (found on the stand: they
// called an owner an operator).
func TestMemberPagesKnowTheHub(t *testing.T) {
	h := &hub{admin: &web.Hub{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	opt := h.memberOptions("eu", "box")
	if opt.Hub != h.admin || opt.Base != "/m/eu" || opt.Fleet.Current() != "eu" {
		t.Errorf("a cluster's pages: hub %v, base %q", opt.Hub != nil, opt.Base)
	}
}

// pagesHub is a hub with its root pages and /m/<name>/ served, without
// links; quiet logs.
func pagesHub(t *testing.T) (*hub, *http.ServeMux, *fleet.Registry) {
	t.Helper()
	reg, err := fleet.OpenRegistry(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &hub{reg: reg, log: log, links: map[string]*memberLink{}, seen: fleet.NewLastKnown(100, 1<<20)}
	root, err := web.New(web.Empty{}, log, web.Options{Fleet: view{h, ""}})
	if err != nil {
		t.Fatal(err)
	}
	h.root, h.rootMux = root, http.NewServeMux()
	root.Register(h.rootMux)
	mux := http.NewServeMux()
	mux.HandleFunc("/m/{name}/", h.member)
	return h, mux, reg
}

// register adds a member of that name with a new key.
func register(t *testing.T, reg *fleet.Registry, name string) {
	t.Helper()
	_, pub, _ := fleet.GenerateKey()
	if _, err := reg.Add(context.Background(), "x", fleet.OriginAPI, name, string(pub)); err != nil {
		t.Fatal(err)
	}
}

// A kept page is the same person's, with the role they had; shown, with
// its note at the start of the content whatever the page's wrapper, only
// to a page opened in the browser (not to the page's own refresh, nor to
// a form sent).
func TestHubKeptPages(t *testing.T) {
	h, mux, reg := pagesHub(t)
	register(t, reg, "eu")
	h.links["eu"] = &memberLink{pages: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		who := "nobody"
		if v := auth.From(r.Context()); v != nil {
			who = v.Username
		}
		if r.URL.Path == "/plain" {
			io.WriteString(w, `<p>a page without the page wrapper</p>`)
			return
		}
		io.WriteString(w, `<div id="main"><div class="page inpipe"><h1>pipeline of eu for `+who+`</h1></div></div>`)
	})}
	alice := &auth.Viewer{Host: "g.example.com", UserID: 1, Username: "alice", Admin: true}
	bob := &auth.Viewer{Host: "g.example.com", UserID: 2, Username: "bob"}
	do := func(method, path string, v *auth.Viewer, mode string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if v != nil {
			req = req.WithContext(auth.With(req.Context(), v))
		}
		if mode != "" {
			req.Header.Set("Sec-Fetch-Mode", mode)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	const p = "/m/eu/pipelines/g/7"
	if r := do("GET", p, alice, "navigate"); r.Code != 200 {
		t.Fatalf("connected: %d", r.Code)
	}
	do("GET", "/m/eu/plain", alice, "")
	delete(h.links, "eu")

	r := do("GET", p, alice, "navigate")
	body := r.Body.String()
	if r.Code != 200 || !strings.Contains(body, "for alice") || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("away, alice: %d %q %q", r.Code, r.Header().Get("Cache-Control"), body)
	}
	note := strings.Index(body, "eu is not connected now.")
	if wrap := strings.Index(body, `<div class="page inpipe">`); note < 0 || wrap < 0 || note < wrap {
		t.Errorf("the note is not at the start of a pipeline page's content: %q", body)
	}
	if !strings.Contains(body, "(seconds ago)") || strings.Contains(body, "0s ago") {
		t.Errorf("the copy's age: %q", body)
	}
	// Another person never gets alice's copy.
	if r := do("GET", p, bob, ""); r.Code != http.StatusBadGateway || strings.Contains(r.Body.String(), "for alice") {
		t.Errorf("bob, who did not open it: %d", r.Code)
	}
	// alice, a viewer now: what she saw with every project is not shown.
	demoted := *alice
	demoted.Admin = false
	if r := do("GET", p, &demoted, ""); r.Code != http.StatusBadGateway {
		t.Errorf("alice demoted: %d", r.Code)
	}
	// The page's own refresh, and a form sent, are told the cluster is
	// away.
	if r := do("GET", p, alice, "cors"); r.Code != http.StatusBadGateway {
		t.Errorf("the page's refresh while away: %d", r.Code)
	}
	if r := do("POST", p, alice, "navigate"); r.Code != http.StatusBadGateway {
		t.Errorf("a POST while away: %d", r.Code)
	}
	// A page with no place for the note is not shown as if current.
	if r := do("GET", "/m/eu/plain", alice, ""); r.Code != http.StatusBadGateway {
		t.Errorf("a kept page without a place for the note: %d %q", r.Code, r.Body)
	}
	// The away page itself: try again, not kept.
	if r := do("GET", "/m/eu/images", alice, ""); r.Code != http.StatusBadGateway || r.Header().Get("Retry-After") == "" || r.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("away page: %d %v", r.Code, r.Header())
	}
	// A name written escaped goes to its plain address, query kept.
	if r := do("GET", "/m/%65u/builds?project=1", alice, ""); r.Code/100 != 3 || r.Header().Get("Location") != "/m/eu/builds?project=1" {
		t.Errorf("escaped name: %d %q", r.Code, r.Header().Get("Location"))
	}
}

// Clusters registered but not connected are named as not answering: a
// pipeline or job may be theirs.
func TestHubLocateNamesOfflineMembers(t *testing.T) {
	h, _, reg := pagesHub(t)
	for _, n := range []string{"us", "eu", "old"} {
		register(t, reg, n)
	}
	if _, err := reg.RemoveCluster(context.Background(), "x", "old"); err != nil {
		t.Fatal(err)
	}
	hits, silent := view{h, ""}.Locate(context.Background(), "g", 7, 0)
	if len(hits) != 0 || strings.Join(silent, ",") != "eu,us" {
		t.Errorf("pipeline: hits %d, silent %q", len(hits), silent)
	}
	if _, silent := (view{h, ""}).Locate(context.Background(), "g", 0, 9); strings.Join(silent, ",") != "eu,us" {
		t.Errorf("job: silent %q", silent)
	}
}

// The hub refuses a negative keep and leftover arguments (a command after
// a flag) instead of starting as a hub.
func TestHubFlagsRefused(t *testing.T) {
	if err := hubMain([]string{"-keep-removed=-1h"}); err == nil || !strings.Contains(err.Error(), "-keep-removed") {
		t.Errorf("negative -keep-removed: %v", err)
	}
	if err := hubMain([]string{"-data", t.TempDir(), "grant", "owner", "gitlab:g.example.com/1"}); err == nil || !strings.Contains(err.Error(), "hub <command>") {
		t.Errorf("a command after a flag: %v", err)
	}
}

// While the registry does not answer, the pages show the member list as
// last read, not an empty fleet.
func TestHubMembersWhileTheRegistryFails(t *testing.T) {
	h, _, reg := pagesHub(t)
	register(t, reg, "eu")
	if ms := (view{h, ""}).Members(); len(ms) != 1 {
		t.Fatalf("members: %+v", ms)
	}
	reg.Close()
	if ms := (view{h, ""}).Members(); len(ms) != 1 || ms[0].Name != "eu" {
		t.Errorf("members, the registry failing: %+v", ms)
	}
}
