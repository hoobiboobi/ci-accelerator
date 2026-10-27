package control

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

var ctx = context.Background()

type daemons []collector.DaemonState

func (d daemons) Daemons() []collector.DaemonState { return d }

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(ctx, t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// serve starts a controller over st and a client of it.
func serve(t *testing.T, st *store.Store, authorize func(*http.Request) error, token string) (*Client, *httptest.Server) {
	t.Helper()
	srv := &Server{Local: &Local{Store: st, Live: daemons{{Name: "d1", Connected: true}}}, Authorize: authorize,
		Config: Config{Version: "v-test", Release: "box"}, Heartbeat: 50 * time.Millisecond}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := Dial(cctx, ClientOptions{URL: ts.URL, Token: func() (string, error) { return token, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return c, ts
}

func TestCallsThroughTheAPI(t *testing.T) {
	st := newStore(t)
	c, _ := serve(t, st, StaticToken("secret-token-123456"), "secret-token-123456")
	if c.Config().Release != "box" || c.Config().Version != "v-test" {
		t.Fatalf("config %+v", c.Config())
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	ps, err := c.Projects(ctx)
	if err != nil || len(ps) != 0 {
		t.Fatalf("projects %v %v", ps, err)
	}
	// The store's sentinel errors keep their identity across the API.
	if _, err := c.BuildPage(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing build: %v", err)
	}
	if d := c.Daemons(); len(d) != 1 || d[0].Name != "d1" {
		t.Fatalf("daemons %+v", d)
	}
	// Two results, a pointer and a bool.
	prev, known, err := c.Previous(ctx, &history.Build{Target: "api", Started: time.Now()})
	if err != nil || prev != nil || known {
		t.Fatalf("previous %v %v %v", prev, known, err)
	}
}

func TestOnlyTheListedMethods(t *testing.T) {
	st := newStore(t)
	_, ts := serve(t, st, nil, "")
	for _, m := range []string{"SaveBuild", "SaveJob", "Prune", "DeleteSession", "SaveSession", "SetReason", "Close", "Announce"} {
		res, err := http.Post(ts.URL+"/internal/v1/call/"+m, "application/json", strings.NewReader("[]"))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", m, res.StatusCode)
		}
	}
	// Wrong arguments are refused, not guessed.
	res, err := http.Post(ts.URL+"/internal/v1/call/BuildPage", "application/json", strings.NewReader(`["x"]`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("bad argument: %d", res.StatusCode)
	}
}

func TestCallersMustProveThemselves(t *testing.T) {
	st := newStore(t)
	srv := &Server{Local: &Local{Store: st}, Authorize: StaticToken("secret-token-123456")}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	try := func(path, token string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader("[]"))
		if strings.HasSuffix(path, "/config") || strings.HasSuffix(path, "/healthz") {
			req.Method = http.MethodGet
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for _, p := range []string{"/internal/v1/call/Ping", "/internal/v1/config"} {
		if s := try(p, ""); s != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d", p, s)
		}
		if s := try(p, "secret-token-12345X"); s != http.StatusUnauthorized {
			t.Errorf("%s with a wrong token: %d", p, s)
		}
		if s := try(p, "secret-token-123456"); s != http.StatusOK {
			t.Errorf("%s with the token: %d", p, s)
		}
	}
	// The kubelet's probe has no token.
	if s := try("/healthz", ""); s != http.StatusOK {
		t.Errorf("health check: %d", s)
	}
}

func TestTokenReview(t *testing.T) {
	kube := fake.NewSimpleClientset()
	asked := 0
	kube.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		asked++
		tr := a.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview)
		if len(tr.Spec.Audiences) != 1 || tr.Spec.Audiences[0] != Audience {
			t.Errorf("audiences %v", tr.Spec.Audiences)
		}
		switch tr.Spec.Token {
		case "web-token":
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, Audiences: []string{Audience},
				User: authnv1.UserInfo{Username: "system:serviceaccount:box:box-web"}}
		case "other-token":
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true,
				User: authnv1.UserInfo{Username: "system:serviceaccount:box:default"}}
		case "user-token":
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{Username: "alice"}}
		}
		return true, tr, nil
	})
	r := &TokenReviewer{Kube: kube, Audience: Audience, Allow: map[string]bool{"box/box-web": true}}
	check := func(token string) error {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return r.Authorize(req)
	}
	if err := check("web-token"); err != nil {
		t.Fatalf("the web: %v", err)
	}
	if err := check("web-token"); err != nil || asked != 1 {
		t.Fatalf("kept answer: %v, asked %d", err, asked)
	}
	if err := check("other-token"); !errors.Is(err, ErrForbidden) {
		t.Errorf("another ServiceAccount: %v", err)
	}
	if err := check("user-token"); !errors.Is(err, ErrForbidden) {
		t.Errorf("a person's token: %v", err)
	}
	if err := check("garbage"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("an unknown token: %v", err)
	}
	if err := check(""); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("no token: %v", err)
	}
}

func TestEventsAndReconnect(t *testing.T) {
	st := newStore(t)
	srv := &Server{Local: &Local{Store: st}, Heartbeat: 50 * time.Millisecond}
	ts := httptest.NewServer(srv.Handler())
	c, err := Dial(ctx, ClientOptions{URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sub := c.Subscribe()
	defer c.Unsubscribe(sub)
	go c.Run(rctx)
	wait := func(want string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case <-sub.C:
				for _, k := range sub.Take() {
					if k == want {
						return
					}
				}
			case <-deadline:
				t.Fatalf("no %q", want)
			}
		}
	}
	// On connecting: anything may have changed.
	wait("*")
	st.Announce("0a1b2c/7")
	wait("0a1b2c/7")
	// The controller goes away and comes back at the same address.
	addr := ts.Listener.Addr().String()
	ts.CloseClientConnections()
	ts.Close()
	time.Sleep(200 * time.Millisecond)
	ts2 := httptest.NewUnstartedServer(srv.Handler())
	ln, err := netListen(addr)
	if err != nil {
		t.Skipf("address not free again: %v", err)
	}
	ts2.Listener = ln
	ts2.Start()
	defer func() {
		cancel()
		ts2.CloseClientConnections()
		ts2.Close()
	}()
	wait("*")
	st.Announce("0a1b2c/8")
	wait("0a1b2c/8")
}

func TestUnavailable(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	srv := &Server{Local: &Local{Store: newStore(t)}}
	ts.Config.Handler = srv.Handler()
	c, err := Dial(ctx, ClientOptions{URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	ts.Close()
	_, err = c.Projects(ctx)
	var away interface{ Unavailable() bool }
	if !errors.As(err, &away) || !away.Unavailable() {
		t.Fatalf("controller away: %v", err)
	}
	if d := c.Daemons(); d != nil {
		t.Fatalf("daemons while away: %v", d)
	}
}

func TestRateLimitedCarriesItsTime(t *testing.T) {
	until := time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC)
	err := fromWire(toWire(&gitlab.RateLimited{Until: until}))
	var rl *gitlab.RateLimited
	if !errors.As(err, &rl) || !rl.Until.Equal(until) {
		t.Fatalf("%v", err)
	}
	b, _ := json.Marshal(toWire(errors.New("x")))
	if !bytes.Contains(b, []byte(`"msg":"x"`)) || bytes.Contains(b, []byte("until")) {
		t.Fatalf("%s", b)
	}
}

func TestReloadingCert(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	// Any key pair will do: write the test server's own.
	cert := srv.TLS.Certificates[0]
	writePair(t, dir, cert)
	get, err := ReloadingCert(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := get(nil)
	if err != nil || c == nil {
		t.Fatalf("%v %v", c, err)
	}
	if _, err := ReloadingCert(filepath.Join(dir, "none.crt"), filepath.Join(dir, "tls.key")); err == nil {
		t.Fatal("a missing certificate was accepted")
	}
}

func netListen(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }

func writePair(t *testing.T, dir string, c tls.Certificate) {
	t.Helper()
	var certPEM []byte
	for _, der := range c.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSetupChange: a controller whose setup changed (sign-in turned on)
// refuses the web that started under the old one, and the web learns it.
func TestSetupChange(t *testing.T) {
	st := newStore(t)
	srv := &Server{Local: &Local{Store: st}, Config: Config{Release: "box"}}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c, err := Dial(ctx, ClientOptions{URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	changed := make(chan struct{})
	c.OnSetupChange(func() { close(changed) })
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	// Only the version differs: still the same setup.
	srv.Config.Version = "v9"
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("another version: %v", err)
	}
	srv.Config.SignIn, srv.Config.PublicURL = true, "https://ci.example.com"
	_, err = c.Projects(ctx)
	var away interface{ Unavailable() bool }
	if !errors.As(err, &away) {
		t.Fatalf("a call under the old setup: %v", err)
	}
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("the web was not told the setup changed")
	}
}
