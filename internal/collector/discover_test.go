package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
)

// lockedLog collects log lines written by the supervisor's goroutine while
// the test reads them.
type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *lockedLog) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Reset()
}

// blocking is a daemon whose stream stays open until it is cancelled, or
// breaks once when told to.
type blocking struct {
	addr   string
	break_ chan struct{}
}

func (b *blocking) Name() string { return b.addr }
func (b *blocking) Listen(ctx context.Context, fn func(controlapi.BuildHistoryEventType, *controlapi.BuildHistoryRecord) error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.break_:
		return errors.New("stream broke")
	}
}
func (b *blocking) Blob(context.Context, *controlapi.Descriptor) ([]byte, error) { return nil, nil }

func TestSupervisorFollowsTheDaemons(t *testing.T) {
	c, _ := newCollector(t)
	var mu sync.Mutex
	addrs := []string{"tcp://a:1234", "tcp://b:1234"}
	dials := map[string]int{}
	breaks := map[string]chan struct{}{"tcp://a:1234": make(chan struct{}, 1), "tcp://b:1234": make(chan struct{}, 1), "tcp://c:1234": make(chan struct{}, 1)}
	active := map[string]bool{}
	sup := &Supervisor{
		Collector: c,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Every:     20 * time.Millisecond,
		Resolve: func(context.Context) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), addrs...), nil
		},
		Dial: func(ctx context.Context, addr string) (Source, error) {
			mu.Lock()
			dials[addr]++
			active[addr] = true
			mu.Unlock()
			go func() { <-ctx.Done(); mu.Lock(); active[addr] = false; mu.Unlock() }()
			return &blocking{addr: addr, break_: breaks[addr]}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	wait := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 300; i++ {
			mu.Lock()
			r := ok()
			mu.Unlock()
			if r {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting: %s (dials %v, active %v)", what, dials, active)
	}
	wait("both daemons read", func() bool { return active["tcp://a:1234"] && active["tcp://b:1234"] })
	breaks["tcp://a:1234"] <- struct{}{}
	wait("a reconnected after its stream broke", func() bool { return dials["tcp://a:1234"] == 2 })
	mu.Lock()
	addrs = []string{"tcp://a:1234", "tcp://c:1234"}
	mu.Unlock()
	wait("c read and b stopped", func() bool { return active["tcp://c:1234"] && !active["tcp://b:1234"] })
	// A daemon whose address is gone is forgotten, not shown as down.
	known := func(name string) bool {
		for _, d := range c.Daemons() {
			if d.Name == name {
				return true
			}
		}
		return false
	}
	wait("b forgotten", func() bool { return !known("tcp://b:1234") && known("tcp://a:1234") && known("tcp://c:1234") })
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if dials["tcp://b:1234"] != 1 || dials["tcp://c:1234"] != 1 {
		t.Errorf("dials: %v", dials)
	}
}

// Right after start the daemons' headless Service may have no records yet:
// the first failed lookups are retried within seconds and logged as
// information; a lasting failure is a warning.
func TestSupervisorRetriesFailedLookups(t *testing.T) {
	c, _ := newCollector(t)
	var mu sync.Mutex
	logs := &lockedLog{}
	calls := 0
	dialed := make(chan string, 4)
	sup := &Supervisor{
		Collector: c,
		Log:       slog.New(slog.NewTextHandler(logs, nil)),
		Every:     time.Hour,
		Resolve: func(context.Context) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls <= 2 {
				return nil, errors.New("lookup: no such host")
			}
			return []string{"tcp://a:1234"}, nil
		},
		Dial: func(ctx context.Context, addr string) (Source, error) {
			dialed <- addr
			return &blocking{addr: addr, break_: make(chan struct{})}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	select {
	case <-dialed:
	case <-time.After(20 * time.Second):
		t.Fatal("the daemon was not read within 20 s of two failed lookups (Every is an hour)")
	}
	cancel()
	<-done
	if out := logs.String(); strings.Count(out, "daemons not resolvable yet") != 2 || strings.Contains(out, "level=WARN") {
		t.Errorf("two failed lookups should be information, not warnings:\n%s", out)
	}

	// A lookup that keeps failing becomes a warning after the quick retries.
	logs.Reset()
	sup.Every = 10 * time.Millisecond
	sup.Resolve = func(context.Context) ([]string, error) { return nil, errors.New("lookup: no such host") }
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	for i := 0; i < 300 && !strings.Contains(logs.String(), "cannot resolve the daemons"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if out := logs.String(); !strings.Contains(out, `level=WARN msg="cannot resolve the daemons"`) || strings.Count(out, "daemons not resolvable yet") != 3 {
		t.Errorf("a lasting failure should warn after three quick retries:\n%s", out)
	}

	// A successful lookup starts the count over: failures after it are quick
	// retries logged as information again, not warnings.
	logs.Reset()
	calls = 0
	script := []bool{false, false, true, false, false, false} // ok from then on
	sup.Resolve = func(context.Context) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= len(script) && !script[calls-1] {
			return nil, errors.New("lookup: no such host")
		}
		return []string{"tcp://a:1234"}, nil
	}
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	for i := 0; i < 300; i++ {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n > len(script) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if out := logs.String(); strings.Count(out, "daemons not resolvable yet") != 5 || strings.Contains(out, "level=WARN") {
		t.Errorf("failures after a successful lookup should be quick retries again, not warnings:\n%s", out)
	}
}

// A rollout: the old daemon's stream breaks (its pod stops) and only then
// does its address leave the Service, while the stream waits to reconnect.
// It is forgotten all the same, not left as "unreachable" for ever.
func TestSupervisorForgetsADaemonGoneWhileReconnecting(t *testing.T) {
	c, _ := newCollector(t)
	var mu sync.Mutex
	addrs := []string{"tcp://old:1234"}
	broke := make(chan struct{}, 1)
	dials := 0
	sup := &Supervisor{
		Collector: c,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Every:     20 * time.Millisecond,
		Resolve: func(context.Context) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), addrs...), nil
		},
		Dial: func(ctx context.Context, addr string) (Source, error) {
			mu.Lock()
			defer mu.Unlock()
			dials++
			if dials > 1 {
				return nil, errors.New("connection refused") // the pod is stopping
			}
			return &blocking{addr: addr, break_: broke}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	state := func() (known, connected bool) {
		for _, d := range c.Daemons() {
			if d.Name == "tcp://old:1234" {
				return true, d.Connected
			}
		}
		return false, false
	}
	until := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 300; i++ {
			if ok() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out: %s", what)
	}
	until("connected", func() bool { k, c := state(); return k && c })
	broke <- struct{}{}
	until("shown as down", func() bool { k, c := state(); return k && !c })
	mu.Lock()
	addrs = nil
	mu.Unlock()
	until("forgotten", func() bool { k, _ := state(); return !k })
}
