package router

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/route"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// serveWith starts r on a fresh listener and returns its address.
func serveWith(t *testing.T, r *Router) string {
	t.Helper()
	ln, err := Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.Serve(ctx, ln, 2*time.Second)
	return ln.Addr().String()
}

// dialCounter dials the backends of addrs (daemon address -> listener
// address), failing for the daemons in fail, and counts every attempt.
type dialCounter struct {
	mu    sync.Mutex
	addrs map[string]string
	fail  map[string]bool
	tries map[string]int
}

func (d *dialCounter) dial(ctx context.Context, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.tries[addr]++
	fail, real := d.fail[addr], d.addrs[addr]
	d.mu.Unlock()
	if fail {
		return nil, errors.New("connection refused")
	}
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", real)
}

// count is the dials of addr; "" for all of them.
func (d *dialCounter) count(addr string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if addr == "" {
		n := 0
		for _, v := range d.tries {
			n += v
		}
		return n
	}
	return d.tries[addr]
}

// A connection makes at most three dials, however many daemons fail.
func TestDialAttemptsBounded(t *testing.T) {
	dc := &dialCounter{fail: map[string]bool{}, tries: map[string]int{}}
	var ds []route.Daemon
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		ds = append(ds, route.Daemon{Name: "bk-" + n, Node: "n" + n, Addr: "10.9.0." + n, Ready: true})
		dc.fail["10.9.0."+n+":1234"] = true
	}
	r := &Router{View: &view{daemons: ds}, Node: "n1", Port: "1234", IdentityWait: -1, Dial: dc.dial}
	addr := serveWith(t, r)
	if got, _ := exchange(t, addr, "build"); got != "" {
		t.Fatalf("answered %q with every daemon down", got)
	}
	if n := dc.count(""); n != 3 {
		t.Errorf("%d dials for one connection, want 3", n)
	}
}

// A daemon one connection could not reach is not dialed by the next ones
// while another daemon can take them.
func TestUnreachableDaemonNotDialedAgain(t *testing.T) {
	b1 := newBackend(t, "d1")
	dc := &dialCounter{addrs: map[string]string{"10.9.0.1:1234": b1.ln.Addr().String()},
		fail: map[string]bool{"10.9.0.2:1234": true}, tries: map[string]int{}}
	v := &view{jobs: map[string]Job{"127.0.0.1": {Project: projA, JobID: 99, Node: "n1"}},
		warm: map[route.Project][]route.Activity{projA: {{Node: "n2", At: time.Now().Add(-time.Hour)}}},
		daemons: []route.Daemon{{Name: "bk-1", Node: "n1", Addr: "10.9.0.1", Ready: true},
			{Name: "bk-2", Node: "n2", Addr: "10.9.0.2", Ready: true}}}
	r := &Router{View: v, Node: "n1", Port: "1234", IdentityWait: -1, Dial: dc.dial}
	addr := serveWith(t, r)
	for i := 0; i < 3; i++ {
		if got, err := exchange(t, addr, "build"); err != nil || got != "d1:BUILD" {
			t.Fatalf("connection %d: %q (%v)", i, got, err)
		}
	}
	if n := dc.count("10.9.0.2:1234"); n != 1 {
		t.Errorf("the unreachable daemon was dialed %d times, want once", n)
	}
}

// At shutdown every pending decision is sent, not only the first batch.
func TestReporterSendsTheWholeBacklogAtShutdown(t *testing.T) {
	var mu sync.Mutex
	sent := 0
	r := &Reporter{Every: time.Hour, Send: func(_ context.Context, ds []store.Decision) error {
		mu.Lock()
		defer mu.Unlock()
		sent += len(ds)
		return nil
	}}
	for i := 1; i <= 2500; i++ {
		r.Add(store.Decision{JobID: uint64(i), Reason: route.ZoneWarm})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	if sent != 2500 || r.Pending() != 0 {
		t.Errorf("sent %d of 2500, %d left", sent, r.Pending())
	}
}

// While the controller is away the reporter warns once, not at every try,
// and says when it reports again.
func TestReporterWarnsOnce(t *testing.T) {
	var mu sync.Mutex
	fail := true
	var logs syncBuffer
	r := &Reporter{Every: 5 * time.Millisecond, Log: slog.New(slog.NewTextHandler(&logs, nil)),
		Send: func(context.Context, []store.Decision) error {
			mu.Lock()
			defer mu.Unlock()
			if fail {
				return errors.New("away")
			}
			return nil
		}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	r.Add(store.Decision{JobID: 1, Reason: route.ZoneWarm})
	time.Sleep(200 * time.Millisecond)
	if n := logs.count("decisions not reported"); n != 1 {
		t.Errorf("warned %d times while the controller was away, want once", n)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	for deadline := time.Now().Add(3 * time.Second); logs.count("decisions reported again") != 1; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no word that the decisions are reported again")
		}
	}
	if r.Pending() != 0 {
		t.Errorf("pending %d", r.Pending())
	}
}
