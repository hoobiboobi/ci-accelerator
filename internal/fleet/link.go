package fleet

import (
	"context"
	"crypto"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// ConnectPath is where members connect, on the hub's console address.
const ConnectPath = "/fleet/v1/connect"

// Protocol is the version of what runs over the link. The hub speaks this
// one and the one before it, from 1 on (Protocols).
const Protocol = 1

// Protocols are the link protocols this hub accepts, oldest first.
func Protocols() []int {
	if Protocol > 1 {
		return []int{Protocol - 1, Protocol}
	}
	return []int{Protocol}
}

// accepted is the one byte the hub sends once it has checked the member's
// key. In TLS 1.3 the client's handshake ends before the server has judged
// the client's certificate, so without it a member the hub refuses would
// look connected for a moment.
const accepted = 0x01

// Why not TLS per stream: net/http stops its background read with a read
// deadline in the past, yamux reports that as a timeout that is not
// temporary, and crypto/tls then breaks the connection for good: every
// kept-alive request after the first failed (spike, 2026-11-15). One TLS
// session under the multiplexer has no such deadlines.

func muxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	// Well inside the 60 s an idle proxy connection usually lives.
	c.KeepAliveInterval = 15 * time.Second
	// Longer than the 10 s default: a link crosses a WAN and proxies.
	c.ConnectionWriteTimeout = 30 * time.Second
	// A stream moves at most one window per round trip: the default 256 KiB
	// held a distant member's large answers to a few MiB/s.
	c.MaxStreamWindowSize = 2 << 20
	// A closed stream whose peer never closes its side is reset after this
	// (default 5 minutes).
	c.StreamCloseTimeout = 30 * time.Second
	c.LogOutput = io.Discard
	return c
}

// stream ends reads when it is closed. yamux's Close only half-closes:
// reads would go on until StreamCloseTimeout, so a member that keeps an
// answer open would hold the hub's caller past every deadline (net/http
// ends a cancelled request by closing the connection).
type stream struct {
	*yamux.Stream
	closed atomic.Bool
}

func (s *stream) Read(b []byte) (int, error) {
	if s.closed.Load() {
		return 0, net.ErrClosed
	}
	return s.Stream.Read(b)
}

func (s *stream) Close() error {
	s.closed.Store(true)
	s.Stream.SetReadDeadline(time.Now()) // wakes a Read that waits
	return s.Stream.Close()
}

// What a hub can make a member hold over the link: requests at once, and
// requests with a body at once (a handler may buffer a body whole), each
// read within a deadline.
const (
	maxServed  = 256
	maxBodies  = 8
	bodyWithin = 2 * time.Minute
)

func bounded(h http.Handler) http.Handler {
	all := make(chan struct{}, maxServed)
	bodies := make(chan struct{}, maxBodies)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case all <- struct{}{}:
			defer func() { <-all }()
		default:
			http.Error(w, "too many requests over the link", http.StatusServiceUnavailable)
			return
		}
		if r.ContentLength != 0 {
			select {
			case bodies <- struct{}{}:
				defer func() { <-bodies }()
			case <-r.Context().Done():
				return
			}
			http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyWithin))
		}
		h.ServeHTTP(w, r)
	})
}

type overLink struct{}

// Over says whether a request came from the hub over the link: it was
// authenticated by the pinned key, not by a token.
func Over(ctx context.Context) bool { return ctx.Value(overLink{}) != nil }

// Dialer keeps a member connected to its hub and serves Handler to it.
type Dialer struct {
	// Hub is the hub's console address ("https://ci.example.com").
	Hub string
	// Name is the member's name at the hub.
	Name string
	// Store identifies the member's store: a new store under the same
	// name is shown as one.
	Store   string
	Version string
	Key     crypto.Signer
	// HubKeys are the hub's public keys the member accepts (several while
	// a key is being replaced).
	HubKeys []crypto.PublicKey
	// Outer is the TLS configuration for the hub's address (its Ingress),
	// which checks that address's certificate; nil: the system's roots.
	Outer   *tls.Config
	Handler http.Handler
	Log     *slog.Logger

	mu sync.Mutex
	st linkState
}

// linkState is how a member's link stands (what the tests watch).
type linkState struct {
	Connected bool
	Since     time.Time
	LastError string
}

func (m *Dialer) status() linkState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st
}

func (m *Dialer) set(f func(*linkState)) {
	m.mu.Lock()
	f(&m.st)
	m.mu.Unlock()
}

// Run keeps the member connected until ctx ends: after a failure it waits
// with jitter, longer after each failure in a row (at most 5 minutes when
// the hub refuses the member, 30 s otherwise).
func (m *Dialer) Run(ctx context.Context) {
	cert, err := certificate(m.Name, m.Key)
	if err != nil {
		m.Log.Error("fleet: the member's key", "err", err)
		return
	}
	hubs := map[string]bool{}
	for _, k := range m.HubKeys {
		hubs[Fingerprint(k)] = true
	}
	backoff := time.Second
	for {
		began := time.Now()
		err := m.session(ctx, cert, hubs)
		if ctx.Err() != nil {
			return
		}
		limit := retryLimit(err)
		if time.Since(began) > time.Minute {
			backoff = time.Second // it was up for a while: a fresh start
		}
		wait := retryWait(backoff, limit)
		m.set(func(s *linkState) { s.Connected, s.LastError = false, err.Error() })
		m.Log.Warn("fleet: not connected to the hub", "hub", m.Hub, "err", err, "retry_in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if backoff *= 2; backoff > limit {
			backoff = limit
		}
	}
}

// retryLimit is the longest wait before the next attempt: long when the hub
// refused the member (asking again soon will not change its answer), short
// for anything else (the network, a hub restarting).
func retryLimit(err error) time.Duration {
	var refused refusedError
	if errors.As(err, &refused) {
		return 5 * time.Minute
	}
	return 30 * time.Second
}

// retryWait is backoff with jitter, never over limit.
func retryWait(backoff, limit time.Duration) time.Duration {
	return min(backoff/2+rand.N(backoff), limit)
}

type refusedError struct{ error }

// refusedAtConfirm says whether the error reading the hub's confirmation is
// the hub refusing the member's key: a TLS alert from the hub, or the hub
// closing the link cleanly. A timeout or a reset is the network's.
func refusedAtConfirm(err error) bool {
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "remote error" {
		return true
	}
	return errors.Is(err, io.EOF)
}

// refusal is the hub's explanation of a refused connection, from its body.
func refusal(resp *http.Response) string {
	if resp.Body == nil {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return strings.TrimSpace(strings.ToValidUTF8(string(b), "?"))
}

func (m *Dialer) session(ctx context.Context, cert tls.Certificate, hubs map[string]bool) error {
	u, err := url.Parse(m.Hub)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = u.Path + ConnectPath
	q := url.Values{"name": {m.Name}, "store": {m.Store}, "version": {m.Version}, "protocol": {strconv.Itoa(Protocol)}}
	u.RawQuery = q.Encode()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if m.Outer != nil {
		tr.TLSClientConfig = m.Outer
	}
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	c, resp, err := websocket.Dial(dctx, u.String(), &websocket.DialOptions{HTTPClient: &http.Client{Transport: tr}, CompressionMode: websocket.CompressionDisabled})
	cancel()
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusBadRequest, http.StatusForbidden, http.StatusConflict:
				return refusedError{fmt.Errorf("the hub refused: %s: %s", resp.Status, refusal(resp))}
			}
		}
		return err
	}
	c.SetReadLimit(-1)
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	conn := websocket.NetConn(sctx, c, websocket.MessageBinary)
	distrusted := false
	tc := tls.Client(conn, pinned(cert, false, func(fp string) error {
		if !hubs[fp] {
			distrusted = true
			return fmt.Errorf("the hub's key %s is not one this member trusts", fp)
		}
		return nil
	}))
	hctx, hcancel := context.WithTimeout(ctx, 20*time.Second)
	err = tc.HandshakeContext(hctx)
	hcancel()
	if err != nil {
		conn.Close()
		if distrusted {
			return refusedError{fmt.Errorf("the link's TLS: %w", err)}
		}
		return fmt.Errorf("the link's TLS: %w", err)
	}
	// The hub judges this member's key only now: it confirms with one byte
	// or ends the connection.
	tc.SetReadDeadline(time.Now().Add(20 * time.Second))
	var b [1]byte
	_, err = io.ReadFull(tc, b[:])
	tc.SetReadDeadline(time.Time{})
	if err != nil || b[0] != accepted {
		tc.Close()
		if err == nil {
			return fmt.Errorf("the hub confirmed the link with %#x, not %#x", b[0], accepted)
		}
		if !refusedAtConfirm(err) || ctx.Err() != nil {
			return fmt.Errorf("the hub did not confirm the link: %w", err)
		}
		return refusedError{fmt.Errorf("the hub refused the key %s for %q (%w): register it at the hub, or join with a token",
			Fingerprint(m.Key.Public()), m.Name, err)}
	}
	sess, err := yamux.Server(tc, muxConfig())
	if err != nil {
		tc.Close()
		return err
	}
	since := time.Now()
	m.set(func(s *linkState) { s.Connected, s.Since, s.LastError = true, since, "" })
	m.Log.Info("fleet: connected to the hub", "hub", m.Hub, "name", m.Name)
	srv := &http.Server{Handler: bounded(m.Handler), ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return context.WithValue(ctx, overLink{}, true) }}
	go func() {
		select {
		case <-ctx.Done():
		case <-sess.CloseChan():
		}
		srv.Close()
	}()
	err = srv.Serve(sess)
	sess.Close()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, io.EOF) {
		err = errors.New("the link closed")
	}
	return fmt.Errorf("after %s: %w", time.Since(since).Round(time.Second), err)
}

// Link is a member connected to the hub.
type Link struct {
	Name, Store, Version, Remote string
	Protocol                     int
	Since                        time.Time
	sess                         *yamux.Session
	transport                    *http.Transport
}

// HTTP is a client whose requests go to the member's API over the link
// (any host in the URL).
func (l *Link) HTTP(timeout time.Duration) *http.Client {
	return &http.Client{Transport: l.transport, Timeout: timeout}
}

// Done is closed when the link ends.
func (l *Link) Done() <-chan struct{} { return l.sess.CloseChan() }

// Close ends the link.
func (l *Link) Close() error { return l.sess.Close() }

// What a member sends in the connect request besides its name: a store id
// as the member makes it (12 hex digits) and a short version text.
var (
	storePattern   = regexp.MustCompile(`^[0-9a-f]{12}$`)
	versionPattern = regexp.MustCompile(`^[0-9A-Za-z.+_() -]{0,64}$`)
)

// maxPending bounds the connections whose key the hub has not judged yet.
const maxPending = 64

// Hub accepts members at ConnectPath.
type Hub struct {
	Key crypto.Signer
	// Member names the member that holds a key (by fingerprint); "" when
	// none does, and why.
	Member func(fingerprint string) (name string, why error)
	// Up is called with each new link and returns once it is no longer
	// used (it may block until l.Done()); a link with the same name
	// replaces the one before it.
	Up  func(l *Link)
	Log *slog.Logger
	// Events keeps what the hub saw of each member's link (nil: nothing).
	Events *LinkEvents

	once    sync.Once
	cert    tls.Certificate
	err     error
	pending atomic.Int32
}

// ServeHTTP accepts one member's connection and keeps it until it ends.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.once.Do(func() { h.cert, h.err = certificate("hub", h.Key) })
	if h.err != nil {
		http.Error(w, "the hub has no usable key", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	name, store, version := q.Get("name"), q.Get("store"), q.Get("version")
	proto, _ := strconv.Atoi(q.Get("protocol"))
	if !slices.Contains(Protocols(), proto) {
		var speaks []string
		for _, p := range Protocols() {
			speaks = append(speaks, strconv.Itoa(p))
		}
		http.Error(w, fmt.Sprintf("this hub speaks protocol %s; the member speaks %d: upgrade the older side", strings.Join(speaks, " and "), proto), http.StatusConflict)
		h.Events.Add(name, fmt.Sprintf("refused, from %s: it speaks protocol %d; this hub speaks %s", r.RemoteAddr, proto, strings.Join(speaks, " and ")))
		return
	}
	switch {
	case !NamePattern.MatchString(name):
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	case !storePattern.MatchString(store):
		http.Error(w, "invalid store: 12 hex digits", http.StatusBadRequest)
		h.Events.Add(name, "refused, from "+r.RemoteAddr+": its store id is not 12 hex digits")
		return
	case !versionPattern.MatchString(version):
		http.Error(w, "invalid version", http.StatusBadRequest)
		h.Events.Add(name, "refused, from "+r.RemoteAddr+": its version text is not valid")
		return
	}
	if h.pending.Add(1) > maxPending {
		h.pending.Add(-1)
		http.Error(w, "too many members connecting at once", http.StatusServiceUnavailable)
		h.Events.Add(name, "refused, from "+r.RemoteAddr+": too many members connecting at once")
		return
	}
	judged := false
	judge := func() {
		if !judged {
			judged = true
			h.pending.Add(-1)
		}
	}
	defer judge()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		h.Log.Warn("fleet: a member's connection", "remote", r.RemoteAddr, "err", err)
		h.Events.Add(name, "refused, from "+r.RemoteAddr+": "+err.Error())
		return
	}
	c.SetReadLimit(-1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := websocket.NetConn(ctx, c, websocket.MessageBinary)
	var proven string
	tc := tls.Server(conn, pinned(h.cert, true, func(fp string) error {
		n, why := h.Member(fp)
		if n == "" {
			return fmt.Errorf("key %s: %w", fp, why)
		}
		if n != name {
			return fmt.Errorf("key %s is %q's, the member said %q", fp, n, name)
		}
		proven = n
		return nil
	}))
	hctx, hcancel := context.WithTimeout(ctx, 10*time.Second)
	err = tc.HandshakeContext(hctx)
	hcancel()
	judge()
	if err != nil {
		h.Log.Warn("fleet: a member was refused", "said", name, "remote", r.RemoteAddr, "err", err)
		h.Events.Add(name, "refused, from "+r.RemoteAddr+": "+err.Error())
		conn.Close()
		return
	}
	tc.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = tc.Write([]byte{accepted})
	tc.SetWriteDeadline(time.Time{})
	if err != nil {
		h.Log.Warn("fleet: confirming a member's link", "name", proven, "remote", r.RemoteAddr, "err", err)
		h.Events.Add(proven, "closed at once, from "+r.RemoteAddr+": confirming the link: "+err.Error())
		tc.Close()
		return
	}
	sess, err := yamux.Client(tc, muxConfig())
	if err != nil {
		tc.Close()
		return
	}
	// The hub opens every stream; one the member opens breaks the protocol
	// (and would sit in the hub's memory, never accepted).
	go func() {
		s, err := sess.Accept()
		if err != nil {
			return
		}
		s.Close()
		h.Log.Warn("fleet: a member opened a stream to the hub: link closed", "name", proven, "remote", r.RemoteAddr)
		h.Events.Add(proven, "closed, from "+r.RemoteAddr+": the member opened a stream to the hub, which the protocol does not allow")
		sess.Close()
	}()
	l := &Link{Name: proven, Store: store, Version: version, Protocol: proto, Remote: r.RemoteAddr, Since: time.Now(), sess: sess}
	l.transport = &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			s, err := sess.OpenStream()
			if err != nil {
				return nil, err
			}
			return &stream{Stream: s}, nil
		},
		MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 2 * time.Minute,
	}
	h.Log.Info("fleet: member connected", "name", l.Name, "store", l.Store, "version", l.Version, "remote", l.Remote)
	h.Up(l)
	<-sess.CloseChan()
	l.transport.CloseIdleConnections()
	h.Log.Info("fleet: member disconnected", "name", l.Name, "after", time.Since(l.Since).Round(time.Second))
	h.Events.Add(l.Name, "closed after "+time.Since(l.Since).Round(time.Second).String()+", from "+l.Remote)
}
