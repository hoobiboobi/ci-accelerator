package router

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/control"
	"github.com/hoobiboobi/ci-accelerator/internal/route"
)

// A connection that sends nothing is closed after FirstByte, and no daemon
// is dialed for it: silent connections cannot pile up in the router and on
// the daemons.
func TestSilentConnectionClosedBeforeAnyDial(t *testing.T) {
	b := newBackend(t, "d1")
	dc := &dialCounter{addrs: map[string]string{"10.9.0.1:1234": b.ln.Addr().String()}, tries: map[string]int{}}
	r := &Router{View: &view{daemons: []route.Daemon{{Name: "bk-1", Node: "n1", Addr: "10.9.0.1", Ready: true}}},
		Node: "n1", Port: "1234", IdentityWait: -1, FirstByte: 300 * time.Millisecond, Dial: dc.dial}
	addr := serveWith(t, r)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = io.ReadAll(c)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("a silent connection is still held after 5 s")
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Errorf("closed after %s, before FirstByte", d)
	}
	if n := dc.count(""); n != 0 {
		t.Errorf("%d daemon dials for a connection that sent nothing", n)
	}
	for deadline := time.Now().Add(time.Second); r.Open() != 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("still open: %d", r.Open())
		}
	}
	// A client that speaks gets its daemon, its first byte included.
	if got, err := exchange(t, addr, "build"); err != nil || got != "d1:BUILD" {
		t.Errorf("a speaking client got %q (%v)", got, err)
	}
}

// Connections above MaxPerSource from one address, or above MaxConns in
// all, are closed at once; the ones within the bounds are served.
func TestConnectionCaps(t *testing.T) {
	// Two source addresses: Linux answers on all of 127/8, macOS only on
	// 127.0.0.1 unless 127.0.0.2 is added as an alias.
	if l, err := net.Listen("tcp", "127.0.0.2:0"); err != nil {
		t.Skipf("127.0.0.2 is not a local address here: %v", err)
	} else {
		l.Close()
	}
	// The backend answers only once the client ends its sending half: the
	// connections stay open until the test closes them.
	b := newBackend(t, "d1")
	dc := &dialCounter{addrs: map[string]string{"10.9.0.1:1234": b.ln.Addr().String()}, tries: map[string]int{}}
	r := &Router{View: &view{daemons: []route.Daemon{{Name: "bk-1", Node: "n1", Addr: "10.9.0.1", Ready: true}}},
		Node: "n1", Port: "1234", IdentityWait: -1, MaxConns: 3, MaxPerSource: 2, Dial: dc.dial}
	addr := serveWith(t, r)
	open := func(from string) net.Conn {
		t.Helper()
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(from)}, Timeout: 2 * time.Second}
		c, err := d.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		c.Write([]byte("x"))
		return c
	}
	// refused says whether the router closed c without serving it.
	refused := func(c net.Conn) bool {
		c.SetReadDeadline(time.Now().Add(time.Second))
		_, err := c.Read(make([]byte, 1))
		var ne net.Error
		return !(errors.As(err, &ne) && ne.Timeout())
	}
	waitOpen := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Second); r.Open() != n; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("open %d, want %d", r.Open(), n)
			}
		}
	}
	a1, a2 := open("127.0.0.1"), open("127.0.0.1")
	waitOpen(2)
	if a3 := open("127.0.0.1"); !refused(a3) {
		t.Error("a third connection from one address was served (MaxPerSource 2)")
	}
	b1 := open("127.0.0.2")
	waitOpen(3)
	if b2 := open("127.0.0.2"); !refused(b2) {
		t.Error("a fourth connection in all was served (MaxConns 3)")
	}
	for _, c := range []net.Conn{a1, a2, b1} {
		if refused(c) {
			t.Error("a connection within the bounds was closed")
		}
	}
	// A connection that ends frees its place.
	a1.Close()
	waitOpen(2)
	open("127.0.0.1")
	waitOpen(3)
}

// The router's own decisions age out, controller or not; the controller's
// count of running builds is used only while it is fresh.
func TestClusterAges(t *testing.T) {
	now := time.Now()
	c := NewCluster(nil)
	old, recent := route.Project{Instance: "0a1b2c", ID: 1}, route.Project{Instance: "0a1b2c", ID: 2}
	c.Decided(old, "n2", now.Add(-90*24*time.Hour))
	c.Decided(recent, "n2", now.Add(-time.Hour))
	c.Expire(now, 7*24*time.Hour)
	if w := c.Warm(old); len(w) != 0 {
		t.Errorf("a decision of 90 days ago still warms: %+v", w)
	}
	if w := c.Warm(recent); len(w) != 1 {
		t.Errorf("a decision of an hour ago is gone: %+v", w)
	}
	c.mu.RLock()
	kept := len(c.own)
	c.mu.RUnlock()
	if kept != 1 {
		t.Errorf("%d projects kept, want 1", kept)
	}

	c.slices["s"] = []route.Daemon{{Name: "bk-1", Node: "n1", Ready: true}}
	c.SetWarm(control.Warm{Active: map[string]int{"n1": 4}}, now.Add(-2*time.Minute))
	if d := c.Daemons(); d[0].Active != 0 {
		t.Errorf("a count of running builds 2 minutes old is used: %+v", d[0])
	}
	c.Heard(now)
	if d := c.Daemons(); d[0].Active != 4 {
		t.Errorf("the count is not used after the controller said it is current: %+v", d[0])
	}
}
