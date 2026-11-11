package collector

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// ErrGone ends the stream of a daemon whose address the Service no longer
// has: its pod is gone, and the collector forgets it.
var ErrGone = errors.New("collector: the daemon is gone")

// Supervisor keeps one history stream per daemon: it resolves the daemons'
// addresses periodically, connects to new ones, reconnects after a broken
// stream and stops streams of daemons that are gone.
type Supervisor struct {
	Collector *Collector
	// Resolve returns the daemons' current addresses (tcp://host:port).
	Resolve func(ctx context.Context) ([]string, error)
	// Dial connects to one daemon.
	Dial func(ctx context.Context, addr string) (Source, error)
	// Every is how often addresses are resolved again (default 30 s).
	Every time.Duration
	// Drain is how long the history of a daemon whose address left the
	// Service is still read (default 35 min): a stopping daemon finishes
	// its running builds first (terminationGracePeriodSeconds), and their
	// records come after its address is gone. The stream ends sooner when
	// the daemon closes it; it is not opened again.
	Drain time.Duration
	Log   *slog.Logger
}

// following is one daemon's stream: cancel ends it, leaving is closed when
// its address left the Service.
type following struct {
	cancel  context.CancelCauseFunc
	leaving chan struct{}
}

// Run supervises until ctx ends.
func (s *Supervisor) Run(ctx context.Context) {
	every := s.Every
	if every == 0 {
		every = 30 * time.Second
	}
	drain := s.Drain
	if drain == 0 {
		drain = 35 * time.Minute
	}
	var (
		wg sync.WaitGroup
		mu sync.Mutex // running: streams remove themselves when they end
	)
	running := map[string]*following{}
	fails := 0
	for {
		wait := every
		addrs, err := s.Resolve(ctx)
		if err != nil {
			// Right after start the daemons' headless Service may have no
			// DNS records yet: retry soon and complain only when it lasts.
			fails++
			if fails <= 3 {
				s.Log.Info("daemons not resolvable yet", "err", err, "attempt", fails)
				wait = min(every, time.Duration(fails)*time.Second)
			} else {
				s.Log.Warn("cannot resolve the daemons", "err", err, "attempts", fails)
			}
		} else {
			fails = 0
			seen := map[string]bool{}
			var added, removed []string
			mu.Lock()
			for _, a := range addrs {
				seen[a] = true
				if running[a] == nil {
					added = append(added, a)
					actx, cancel := context.WithCancelCause(ctx)
					f := &following{cancel: cancel, leaving: make(chan struct{})}
					running[a] = f
					wg.Add(1)
					go func() {
						defer wg.Done()
						s.stream(actx, a, f)
						mu.Lock()
						if running[a] == f {
							delete(running, a)
						}
						mu.Unlock()
					}()
				}
			}
			for a, f := range running {
				select {
				case <-f.leaving:
					continue // already leaving
				default:
				}
				if !seen[a] {
					close(f.leaving)
					time.AfterFunc(drain, func() { f.cancel(ErrGone) })
					removed = append(removed, a)
				}
			}
			total := len(running) - len(removed) // the leaving ones are read on, not counted
			mu.Unlock()
			if len(added)+len(removed) > 0 {
				sort.Strings(removed)
				s.Log.Info("daemons changed", "added", added, "removed", removed, "total", total)
			}
		}
		select {
		case <-ctx.Done():
			mu.Lock()
			for _, f := range running {
				f.cancel(nil)
			}
			mu.Unlock()
			wg.Wait()
			return
		case <-time.After(wait):
		}
	}
}

// stream reads one daemon's history, reconnecting with a growing delay
// until its address leaves the Service; then it reads on until the daemon
// closes the stream (or Drain ends it) and does not reconnect.
func (s *Supervisor) stream(ctx context.Context, addr string, f *following) {
	// The address left the Service: forget the daemon read there, also when
	// its stream had broken before (a rollout stops the pod first) and this
	// one only waited to reconnect.
	var (
		name string
		n    uint64
	)
	defer func() {
		if name != "" && errors.Is(context.Cause(ctx), ErrGone) {
			s.Collector.gone(name, n)
		}
	}()
	delay := time.Second
	for ctx.Err() == nil {
		select {
		case <-f.leaving:
			// The address left and the stream ended (the stopping daemon
			// closed it after its last build) or was down: forget it.
			s.Log.Info("daemon gone, its history read to the end", "addr", addr)
			f.cancel(ErrGone)
			return
		default:
		}
		src, err := s.Dial(ctx, addr)
		if err == nil {
			name = src.Name()
			s.Log.Info("reading build history", "daemon", src.Name(), "addr", addr)
			n, err = s.Collector.run(ctx, src)
			if c, ok := src.(interface{ Close() error }); ok {
				c.Close()
			}
			if err == nil || ctx.Err() != nil {
				delay = time.Second // a clean end: the daemon restarted
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			s.Log.Warn("build history stream", "addr", addr, "err", err, "retry_in", delay.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < time.Minute {
			delay *= 2
		}
	}
}
