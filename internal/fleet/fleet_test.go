package fleet

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func pemOf(t *testing.T, typ string, der []byte) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func TestKeyForms(t *testing.T) {
	priv, pub, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParsePrivateKey(priv)
	if err != nil {
		t.Fatalf("PKCS#8 Ed25519: %v", err)
	}
	pubs, err := ParsePublicKeys(pub)
	if err != nil || len(pubs) != 1 {
		t.Fatalf("PKIX Ed25519: %v %d", err, len(pubs))
	}
	if Fingerprint(k.Public()) != Fingerprint(pubs[0]) || !strings.HasPrefix(Fingerprint(pubs[0]), "SHA256:") || len(Fingerprint(pubs[0])) != 50 {
		t.Fatalf("fingerprints %q %q", Fingerprint(k.Public()), Fingerprint(pubs[0]))
	}
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	sec1, _ := x509.MarshalECPrivateKey(ec)
	if _, err := ParsePrivateKey(pemOf(t, "EC PRIVATE KEY", sec1)); err != nil {
		t.Errorf("SEC1 P-256: %v", err)
	}
	p8, _ := x509.MarshalPKCS8PrivateKey(ec)
	if _, err := ParsePrivateKey(pemOf(t, "PRIVATE KEY", p8)); err != nil {
		t.Errorf("PKCS#8 P-256: %v", err)
	}
	// Two public keys in one file: a hub's key being replaced.
	ecPub, _ := x509.MarshalPKIXPublicKey(&ec.PublicKey)
	two, err := ParsePublicKeys(append(pub, pemOf(t, "PUBLIC KEY", ecPub)...))
	if err != nil || len(two) != 2 {
		t.Errorf("two public keys: %v %d", err, len(two))
	}
	// Refused: RSA, P-224 (Terraform's tls provider's default curve), nothing.
	r, _ := rsa.GenerateKey(rand.Reader, 2048)
	rp8, _ := x509.MarshalPKCS8PrivateKey(r)
	if _, err := ParsePrivateKey(pemOf(t, "PRIVATE KEY", rp8)); err == nil {
		t.Error("an RSA key was accepted")
	}
	p224, _ := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	p224pub, _ := x509.MarshalPKIXPublicKey(&p224.PublicKey)
	if _, err := ParsePublicKeys(pemOf(t, "PUBLIC KEY", p224pub)); err == nil || !strings.Contains(err.Error(), "P-224") {
		t.Errorf("P-224: %v", err)
	}
	if _, err := ParsePublicKeys([]byte("ssh-ed25519 AAAA")); err == nil {
		t.Error("a non-PEM key was accepted")
	}
}

// pair is a hub on httptest and what it saw.
type pair struct {
	srv   *httptest.Server
	hub   *Hub
	mu    sync.Mutex
	links []*Link
	seen  chan *Link
}

func startHub(t *testing.T, holders map[string]string) (*pair, []byte) {
	t.Helper()
	priv, pub, _ := GenerateKey()
	key, _ := ParsePrivateKey(priv)
	p := &pair{seen: make(chan *Link, 8)}
	p.hub = &Hub{Key: key, Log: quiet,
		Member: func(fp string) (string, error) {
			if n, ok := holders[fp]; ok {
				return n, nil
			}
			return "", errors.New("no member holds this key")
		},
		Up: func(l *Link) {
			p.mu.Lock()
			p.links = append(p.links, l)
			p.mu.Unlock()
			p.seen <- l
		}}
	mux := http.NewServeMux()
	mux.Handle("GET "+ConnectPath, p.hub)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p, pub
}

// testStore is a store id as a member makes it.
const testStore = "0123456789ab"

func member(t *testing.T, name, hub string, hubPub []byte, h http.Handler) (*Dialer, string) {
	t.Helper()
	priv, _, _ := GenerateKey()
	key, _ := ParsePrivateKey(priv)
	keys, _ := ParsePublicKeys(hubPub)
	if h == nil {
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "member %s, over the link: %v", name, Over(r.Context()))
		})
	}
	return &Dialer{Hub: hub, Name: name, Store: testStore, Version: "test", Key: key, HubKeys: keys, Handler: h, Log: quiet}, Fingerprint(key.Public())
}

func TestLink(t *testing.T) {
	holders := map[string]string{}
	p, hubPub := startHub(t, holders)
	d, fp := member(t, "eu", p.srv.URL, hubPub, nil)
	holders[fp] = "eu"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	var l *Link
	select {
	case l = <-p.seen:
	case <-time.After(10 * time.Second):
		t.Fatal("the member did not connect")
	}
	if l.Name != "eu" || l.Store != testStore || l.Version != "test" || l.Protocol != Protocol {
		t.Fatalf("link %+v", l)
	}
	c := l.HTTP(10 * time.Second)
	// Several requests on kept-alive connections, and some at once: per
	// stream TLS broke exactly this.
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := c.Get("http://any/x")
			if err != nil {
				t.Error(err)
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) != "member eu, over the link: true" {
				t.Errorf("answer %q", b)
			}
		}()
		if i%10 == 0 {
			wg.Wait()
		}
	}
	wg.Wait()
	if s := d.status(); !s.Connected {
		t.Fatalf("member state %+v", s)
	}
	// The hub drops the link: the member comes back by itself.
	l.Close()
	select {
	case l2 := <-p.seen:
		if l2.Name != "eu" {
			t.Fatalf("reconnected as %q", l2.Name)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the member did not reconnect")
	}
}

func TestLinkRefusesUnknownKeysAndNames(t *testing.T) {
	holders := map[string]string{}
	p, hubPub := startHub(t, holders)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A key nobody registered.
	stranger, _ := member(t, "eu", p.srv.URL, hubPub, nil)
	// A registered key that claims another member's name.
	liar, fp := member(t, "us", p.srv.URL, hubPub, nil)
	holders[fp] = "eu"
	// A member that does not trust this hub's key.
	_, otherHub, _ := GenerateKey()
	wrongHub, fp3 := member(t, "ap", p.srv.URL, otherHub, nil)
	holders[fp3] = "ap"
	// The hub reads holders during the handshakes: filled before any dials.
	go stranger.Run(ctx)
	go liar.Run(ctx)
	go wrongHub.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (stranger.status().LastError == "" || liar.status().LastError == "" || wrongHub.status().LastError == "") {
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case l := <-p.seen:
		t.Fatalf("a link came up for %q", l.Name)
	default:
	}
	for n, d := range map[string]*Dialer{"stranger": stranger, "liar": liar, "wrong hub": wrongHub} {
		s := d.status()
		if s.Connected || s.LastError == "" {
			t.Errorf("%s: %+v", n, s)
		}
	}
	if !strings.Contains(wrongHub.status().LastError, "not one this member trusts") {
		t.Errorf("wrong hub: %s", wrongHub.status().LastError)
	}
}

func TestLinkProtocolMismatch(t *testing.T) {
	p, _ := startHub(t, map[string]string{})
	resp, err := http.Get(p.srv.URL + ConnectPath + "?name=eu&store=" + testStore + "&version=test&protocol=" + fmt.Sprint(Protocol+1))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(b), "upgrade the older side") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	// It names only the protocols the hub accepts, from 1 on.
	var speaks []string
	for _, v := range Protocols() {
		if v < 1 {
			t.Errorf("protocols %v", Protocols())
		}
		speaks = append(speaks, fmt.Sprint(v))
	}
	if !strings.Contains(string(b), "speaks protocol "+strings.Join(speaks, " and ")+";") || !strings.Contains(fmt.Sprint(Protocols()), fmt.Sprint(Protocol)) {
		t.Errorf("%s (protocols %v)", b, Protocols())
	}
}

func TestLinkBounds(t *testing.T) {
	// Connections the hub has not judged yet are bounded.
	p, _ := startHub(t, map[string]string{})
	p.hub.pending.Store(maxPending)
	resp, err := http.Get(p.srv.URL + ConnectPath + "?name=eu&store=" + testStore + "&version=test&protocol=" + fmt.Sprint(Protocol))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("over the pending bound: %d", resp.StatusCode)
	}
	// The wait before the next attempt keeps within its limit, jitter and all.
	for _, backoff := range []time.Duration{time.Second, 20 * time.Second, 30 * time.Second, 5 * time.Minute} {
		for i := 0; i < 1000; i++ {
			limit := 30 * time.Second
			if backoff > limit {
				limit = backoff
			}
			if w := retryWait(backoff, limit); w > limit || w < backoff/2 {
				t.Fatalf("retryWait(%s, %s) = %s", backoff, limit, w)
			}
		}
	}
	// A stream's window lets a distant member's answer move, and a stream
	// closed on one side is reset well before yamux's 5 minutes.
	c := muxConfig()
	if c.MaxStreamWindowSize < 1<<20 || c.StreamCloseTimeout > time.Minute {
		t.Errorf("window %d, close timeout %s", c.MaxStreamWindowSize, c.StreamCloseTimeout)
	}
}

func openReg(t *testing.T) (*Registry, *time.Time) {
	t.Helper()
	r, err := OpenRegistry(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	now := time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	return r, &now
}

func TestRegistry(t *testing.T) {
	ctx := context.Background()
	r, now := openReg(t)
	_, pubA, _ := GenerateKey()
	_, pubB, _ := GenerateKey()
	m, err := r.Add(ctx, "token:bootstrap", "api", "prod-eu", string(pubA))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := r.Holder(m.Fingerprint); n != "prod-eu" {
		t.Fatalf("holder %q", n)
	}
	for _, c := range []struct{ name, key, want string }{
		{"prod-eu", string(pubB), "exists"},
		{"prod-us", string(pubA), "this key is \"prod-eu\"'s"},
		{"Prod", string(pubB), "invalid"},
		{"-x", string(pubB), "invalid"},
		{"prod-us", "not a key", "invalid"},
		{"prod-us", string(pubA) + string(pubB), "give one public key"},
	} {
		if _, err := r.Add(ctx, "a", "api", c.name, c.key); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Add(%q): %v, want %q", c.name, err, c.want)
		}
	}
	// First visit, the same store again, then another store.
	for i, c := range []struct {
		store string
		want  bool
	}{{"s1", false}, {"s1", false}, {"s2", true}} {
		got, err := r.Seen(ctx, "prod-eu", c.store, "0.8.0")
		if err != nil || got != c.want {
			t.Fatalf("visit %d: replaced=%v %v", i, got, err)
		}
	}
	if err := r.Remove(ctx, "gitlab:x/1", "prod-eu"); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove(ctx, "gitlab:x/1", "prod-eu"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}
	if n, _ := r.Holder(m.Fingerprint); n != "" {
		t.Fatal("a removed member's key still connects")
	}
	// The name comes back with a new key: its last store is remembered, so
	// the same store is the same member at once and another one is flagged.
	if _, err := r.Add(ctx, "token:bootstrap", "api", "prod-eu", string(pubB)); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Seen(ctx, "prod-eu", "s2", "0.8.0"); got {
		t.Fatal("the same store came back flagged as replaced")
	}
	ms, _ := r.List(ctx)
	if len(ms) != 1 || ms[0].Version != "0.8.0" || ms[0].SeenAt.IsZero() {
		t.Fatalf("list %+v", ms)
	}
	ev, _ := r.Audit(ctx, 100)
	var actions []string
	for _, e := range ev {
		actions = append(actions, e.Actor+" "+e.Action)
	}
	want := "token:bootstrap member.add,gitlab:x/1 member.remove,member:prod-eu member.store-replaced,token:bootstrap member.add"
	if strings.Join(actions, ",") != want {
		t.Fatalf("audit %q", strings.Join(actions, ","))
	}
	// Removed members and old audit records go after their keep.
	r.Remove(ctx, "a", "prod-eu")
	*now = now.Add(31 * 24 * time.Hour)
	if err := r.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if ms, _ := r.List(ctx); len(ms) != 0 {
		t.Fatalf("after prune %+v", ms)
	}
}

func TestAPI(t *testing.T) {
	r, _ := openReg(t)
	var removed []string
	a := &API{Registry: r, Version: "test",
		Who:     testWho,
		Status:  func(name string) Status { return Status{Connected: name == "eu"} },
		Removed: func(name string) { removed = append(removed, name) }}
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	do := func(method, path, token, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := do("GET", "/api/v1/fleet/members", "", ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := do("GET", "/api/v1/fleet/members", "bad", ""); code != 401 {
		t.Errorf("bad token: %d", code)
	}
	if code, e := do("GET", "/api/v1/fleet/members", "view", ""); code != 403 || e["error"].(map[string]any)["code"] != "forbidden" {
		t.Errorf("a viewer's token: %d %v", code, e)
	}
	if code, _ := do("GET", "/api/v1/fleet/version", "view", ""); code != 200 {
		t.Errorf("a viewer's version: %d", code)
	}
	_, pub, _ := GenerateKey()
	body, _ := json.Marshal(map[string]string{"name": "eu", "public_key": string(pub)})
	code, m := do("POST", "/api/v1/fleet/members", "good", string(body))
	if code != 201 || m["name"] != "eu" || m["origin"] != "api" || m["added_by"] != "token:t" || m["status"].(map[string]any)["connected"] != true {
		t.Fatalf("add: %d %v", code, m)
	}
	if code, e := do("POST", "/api/v1/fleet/members", "good", string(body)); code != 409 || e["error"].(map[string]any)["code"] != "exists" {
		t.Errorf("again: %d %v", code, e)
	}
	// A member that would be valid but for one field the API does not know.
	_, pub2, _ := GenerateKey()
	extra, _ := json.Marshal(map[string]any{"name": "us", "public_key": string(pub2), "extra": 1})
	if code, _ := do("POST", "/api/v1/fleet/members", "good", string(extra)); code != 400 {
		t.Errorf("unknown field: %d", code)
	}
	if code, l := do("GET", "/api/v1/fleet/members", "good", ""); code != 200 || len(l["members"].([]any)) != 1 {
		t.Errorf("list: %d %v", code, l)
	}
	if code, _ := do("GET", "/api/v1/fleet/members/none", "good", ""); code != 404 {
		t.Errorf("missing: %d", code)
	}
	for i := 0; i < 2; i++ {
		if code, _ := do("DELETE", "/api/v1/fleet/members/eu", "good", ""); code != 204 {
			t.Errorf("delete %d: %d", i, code)
		}
	}
	if len(removed) != 1 {
		t.Errorf("links closed: %v", removed)
	}
	if code, ev := do("GET", "/api/v1/fleet/audit", "good", ""); code != 200 || len(ev["events"].([]any)) != 2 {
		t.Errorf("audit: %d %v", code, ev)
	}
	if code, v := do("GET", "/api/v1/fleet/version", "good", ""); code != 200 || v["api"] != APIVersion {
		t.Errorf("version: %d %v", code, v)
	}
}

// testWho: "Bearer good" is an owner's token, "Bearer view" a viewer's,
// "Bearer op" an operator's.
func testWho(req *http.Request) Principal {
	switch req.Header.Get("Authorization") {
	case "Bearer good":
		return Principal{Subject: "token:t", Role: Owner}
	case "Bearer op":
		return Principal{Subject: "token:op", Role: Operator}
	case "Bearer view":
		return Principal{Subject: "token:v", Role: Viewer}
	}
	return Principal{}
}
