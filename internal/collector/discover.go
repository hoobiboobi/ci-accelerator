package collector

import (
	"context"
	"errors"
	"log/slog"
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
	Log   *slog.Logger
}

// Run supervises until ctx ends.
func (s *Supervisor) Run(ctx context.Context) {
	every := s.Every
	if every == 0 {
		every = 30 * time.Second
	}
	var wg sync.WaitGroup
	running := map[string]context.CancelCauseFunc{}
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
			for _, a := range addrs {
				seen[a] = true
				if running[a] == nil {
					actx, cancel := context.WithCancelCause(ctx)
					running[a] = cancel
					wg.Add(1)
					go func() {
						defer wg.Done()
						s.stream(actx, a)
					}()
				}
			}
			for a, cancel := range running {
				if !seen[a] {
					cancel(ErrGone)
					delete(running, a)
				}
			}
		}
		select {
		case <-ctx.Done():
			for _, cancel := range running {
				cancel(nil)
			}
			wg.Wait()
			return
		case <-time.After(wait):
		}
	}
}

// stream reads one daemon's history, reconnecting with a growing delay.
func (s *Supervisor) stream(ctx context.Context, addr string) {
	delay := time.Second
	for ctx.Err() == nil {
		src, err := s.Dial(ctx, addr)
		if err == nil {
			s.Log.Info("reading build history", "daemon", src.Name(), "addr", addr)
			err = s.Collector.Run(ctx, src)
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
