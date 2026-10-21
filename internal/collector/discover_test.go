package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
)

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
