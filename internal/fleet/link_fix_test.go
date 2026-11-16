package fleet

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// lockedBuffer collects a logger's lines from several goroutines.
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

// up connects d to p's hub and returns the hub's side of the link.
func up(t *testing.T, p *pair, d *Dialer) *Link {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)
	select {
	case l := <-p.seen:
		return l
	case <-time.After(10 * time.Second):
		t.Fatal("the member did not connect")
	}
	return nil
}

// A member that sends its headers and then keeps its answer open must not
// hold the hub's caller past the caller's deadline.
func TestLinkDeadlineEndsAnAnswerKeptOpen(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	trickle := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// Hostile: it ignores the request's end and writes on.
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
			if _, err := w.Write([]byte(" ")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	})
	holders := map[string]string{}
	p, hubPub := startHub(t, holders)
	d, fp := member(t, "eu", p.srv.URL, hubPub, trickle)
	holders[fp] = "eu"
	l := up(t, p, d)
	began := time.Now()
	done := make(chan error, 1)
	go func() {
		resp, err := l.HTTP(time.Second).Get("http://eu/x")
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the answer kept open ended without an error")
		}
		if took := time.Since(began); took > 5*time.Second {
			t.Fatalf("a 1 s deadline ended the call after %s", took)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a 1 s deadline had not ended the call after 15 s")
	}
}

// A member whose key the hub does not know is told so, and never logs that
// it connected.
func TestLinkRefusedKeyIsNamed(t *testing.T) {
	p, hubPub := startHub(t, map[string]string{})
	d, fp := member(t, "eu", p.srv.URL, hubPub, nil)
	var logs lockedBuffer
	d.Log = slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && d.status().LastError == "" {
		time.Sleep(20 * time.Millisecond)
	}
	// Two attempts at least: the first one's lines are all there.
	time.Sleep(2 * time.Second)
	s := d.status()
	if !strings.Contains(s.LastError, "refused the key "+fp) {
		t.Errorf("the member's error does not say the hub refused its key: %q", s.LastError)
	}
	if strings.Contains(logs.String(), `msg="fleet: connected to the hub"`) {
		t.Errorf("a refused member logged that it connected:\n%s", logs.String())
	}
}

// What the hub refuses before the link, it explains; the member keeps it.
func TestLinkRefusalCarriesTheHubsReason(t *testing.T) {
	holders := map[string]string{}
	p, hubPub := startHub(t, holders)
	d, fp := member(t, "eu", p.srv.URL, hubPub, nil)
	holders[fp] = "eu"
	d.Store = "store aaaa -> bbbb\nforged"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && d.status().LastError == "" {
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case l := <-p.seen:
		t.Fatalf("a link came up with store %q", l.Store)
	default:
	}
	if s := d.status(); !strings.Contains(s.LastError, "invalid store") {
		t.Errorf("the member's error: %q", s.LastError)
	}
}

// The connect request's own values are checked before the connection is
// upgraded: a name, store and version of the forms members send.
func TestConnectChecksItsQuery(t *testing.T) {
	p, _ := startHub(t, map[string]string{})
	for _, c := range []struct{ name, store, version, want string }{
		{strings.Repeat("a", 5000), "0123456789ab", "test", "invalid name"},
		{"eu", strings.Repeat("x", 5000), "test", "invalid store"},
		{"eu", "0123456789ab", "0.7.0\nforged line", "invalid version"},
		{"eu", "0123456789ab", strings.Repeat("9", 65), "invalid version"},
	} {
		q := url.Values{"name": {c.name}, "store": {c.store}, "version": {c.version}, "protocol": {fmt.Sprint(Protocol)}}
		resp, err := http.Get(p.srv.URL + ConnectPath + "?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), c.want) {
			t.Errorf("%.20q %.20q %.20q: %d %.80s, want 400 %q", c.name, c.store, c.version, resp.StatusCode, b, c.want)
		}
	}
}

// skipAccepted drops the hub's one-byte confirmation if there is one, so
// that the raw member below speaks to a hub with or without it.
type skipAccepted struct {
	net.Conn
	once sync.Once
	head []byte
}

func (s *skipAccepted) Read(b []byte) (int, error) {
	var err error
	s.once.Do(func() {
		var one [1]byte
		if _, err = io.ReadFull(s.Conn, one[:]); err == nil && one[0] != 0x01 {
			s.head = one[:]
		}
	})
	if err != nil {
		return 0, err
	}
	if len(s.head) > 0 {
		n := copy(b, s.head)
		s.head = s.head[n:]
		return n, nil
	}
	return s.Conn.Read(b)
}

// A member that opens streams to the hub (the hub never accepts any)
// loses its link at once instead of parking data in the hub.
func TestHubClosesALinkOnAMembersStream(t *testing.T) {
	holders := map[string]string{}
	p, _ := startHub(t, holders)
	priv, _, _ := GenerateKey()
	key, _ := ParsePrivateKey(priv)
	holders[Fingerprint(key.Public())] = "eu"
	cert, err := certificate("eu", key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	q := url.Values{"name": {"eu"}, "store": {"0123456789ab"}, "version": {"test"}, "protocol": {fmt.Sprint(Protocol)}}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(p.srv.URL, "http")+ConnectPath+"?"+q.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn := websocket.NetConn(ctx, c, websocket.MessageBinary)
	defer conn.Close()
	tc := tls.Client(conn, pinned(cert, false, func(string) error { return nil }))
	if err := tc.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	sess, err := yamux.Server(&skipAccepted{Conn: tc}, muxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var l *Link
	select {
	case l = <-p.seen:
	case <-time.After(10 * time.Second):
		t.Fatal("the hub did not take the link")
	}
	s, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	go s.Write(bytes.Repeat([]byte("x"), 64<<10))
	select {
	case <-l.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the hub kept a link whose member opened a stream to it")
	}
}

// However many requests with a body the hub sends at once, the member
// serves a few at a time.
func TestMemberBoundsRequestsWithABody(t *testing.T) {
	var active, most atomic.Int32
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			n := active.Add(1)
			for {
				m := most.Load()
				if n <= m || most.CompareAndSwap(m, n) {
					break
				}
			}
			<-release
			active.Add(-1)
		}
		io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, "ok")
	})
	holders := map[string]string{}
	p, hubPub := startHub(t, holders)
	d, fp := member(t, "eu", p.srv.URL, hubPub, h)
	holders[fp] = "eu"
	l := up(t, p, d)
	c := l.HTTP(20 * time.Second)
	var wg sync.WaitGroup
	codes := make(chan int, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := c.Post("http://eu/call", "application/json", strings.NewReader(`{"a":1}`))
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && active.Load() < 8 {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if m := most.Load(); m > 8 {
		t.Errorf("%d requests with a body were served at once", m)
	}
	close(release)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Errorf("a request waiting its turn got %d", code)
		}
	}
	// Requests without a body are not held behind them.
	resp, err := c.Get("http://eu/page")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET after the calls: %v %v", resp, err)
	}
	resp.Body.Close()
}

// A member speaking no protocol the hub knows (protocol=0 or none) is told
// to upgrade before the connection is upgraded.
func TestConnectRefusesProtocolZero(t *testing.T) {
	p, _ := startHub(t, map[string]string{})
	for _, proto := range []string{"0", ""} {
		q := url.Values{"name": {"eu"}, "store": {testStore}, "version": {"test"}, "protocol": {proto}}
		resp, err := http.Get(p.srv.URL + ConnectPath + "?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict || !strings.Contains(string(b), "upgrade the older side") {
			t.Errorf("protocol %q: %d %.80s, want 409", proto, resp.StatusCode, b)
		}
	}
}

// The link's own TLS is 1.3 on both ends: a peer that offers at most 1.2
// fails the handshake.
func TestPinnedNeedsTLS13(t *testing.T) {
	mk := func() tls.Certificate {
		priv, _, _ := GenerateKey()
		key, _ := ParsePrivateKey(priv)
		c, err := certificate("x", key)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	yes := func(string) error { return nil }
	for _, old := range []string{"server", "client"} {
		client, server := pinned(mk(), false, yes), pinned(mk(), true, yes)
		if old == "server" {
			server.MaxVersion = tls.VersionTLS12
		} else {
			client.MaxVersion = tls.VersionTLS12
		}
		a, b := net.Pipe()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		done := make(chan error, 1)
		go func() {
			done <- tls.Server(b, server).HandshakeContext(ctx)
			b.Close()
		}()
		err := tls.Client(a, client).HandshakeContext(ctx)
		a.Close()
		serr := <-done
		cancel()
		if err == nil && serr == nil {
			t.Errorf("a %s capped at TLS 1.2 completed the link's handshake", old)
		}
	}
}

// After a refusal the member waits up to 5 minutes; after anything else up
// to 30 s.
func TestRetryLimit(t *testing.T) {
	refused := fmt.Errorf("after: %w", refusedError{fmt.Errorf("the hub refused")})
	if got := retryLimit(refused); got != 5*time.Minute {
		t.Errorf("refused: %s, want 5m", got)
	}
	if got := retryLimit(fmt.Errorf("dial: %w", io.ErrUnexpectedEOF)); got != 30*time.Second {
		t.Errorf("network: %s, want 30s", got)
	}
}

// Only the hub's own answer at the confirmation counts as a refusal: a TLS
// alert or a clean close. A timeout or a reset gets the short back-off.
func TestRefusedAtConfirm(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&net.OpError{Op: "remote error", Err: fmt.Errorf("tls: bad certificate")}, true},
		{io.EOF, true},
		{fmt.Errorf("failed to read: %w", context.DeadlineExceeded), false},
		{&net.OpError{Op: "read", Err: syscall.ECONNRESET}, false},
		{fmt.Errorf("failed to get reader: %w", io.ErrUnexpectedEOF), false},
	} {
		if got := refusedAtConfirm(c.err); got != c.want {
			t.Errorf("%v: %v, want %v", c.err, got, c.want)
		}
	}
}
