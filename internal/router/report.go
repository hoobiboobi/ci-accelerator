package router

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/control"
	"github.com/hoobiboobi/ci-accelerator/internal/route"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// Reporter sends the router's decisions to the controller in batches, at
// once for a project that was cold (the other routers learn where it went
// on their next read), and keeps them while the controller is away.
type Reporter struct {
	Send  func(ctx context.Context, ds []store.Decision) error
	Every time.Duration // default 2 s
	Max   int           // decisions kept while the controller is away (default 10 000)
	Log   *slog.Logger

	mu      sync.Mutex
	pending []store.Decision
	// base is how many decisions left the front of pending so far (sent
	// or dropped): a batch is cut by position, not by count, so drops while
	// it is in flight never cost unsent decisions.
	base    int
	kick    chan struct{}
	dropped int
}

func (r *Reporter) init() {
	if r.kick == nil {
		r.kick = make(chan struct{}, 1)
	}
}

// Add queues a decision.
func (r *Reporter) Add(d store.Decision) {
	r.mu.Lock()
	r.init()
	max := r.Max
	if max == 0 {
		max = 10000
	}
	if len(r.pending) >= max {
		r.pending = r.pending[1:]
		r.base++
		r.dropped++
	}
	r.pending = append(r.pending, d)
	r.mu.Unlock()
	if d.Reason == route.ZoneHash || d.Reason == route.CrossZone {
		select {
		case r.kick <- struct{}{}:
		default:
		}
	}
}

// Pending is the number of decisions not sent yet.
func (r *Reporter) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// Run sends until ctx ends, then once more.
func (r *Reporter) Run(ctx context.Context) {
	r.mu.Lock()
	r.init()
	r.mu.Unlock()
	every := r.Every
	if every == 0 {
		every = 2 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			r.flush(fctx)
			cancel()
			return
		case <-t.C:
		case <-r.kick:
		}
		r.flush(ctx)
	}
}

func (r *Reporter) flush(ctx context.Context) {
	r.mu.Lock()
	batch := r.pending
	if len(batch) > 1000 {
		batch = batch[:1000]
	}
	batch = append([]store.Decision(nil), batch...)
	end := r.base + len(batch)
	dropped := r.dropped
	r.dropped = 0
	r.mu.Unlock()
	if dropped > 0 && r.Log != nil {
		r.Log.Warn("decisions dropped while the controller was away", "dropped", dropped)
	}
	if len(batch) == 0 {
		return
	}
	if err := r.Send(ctx, batch); err != nil {
		if r.Log != nil {
			r.Log.Warn("decisions not reported", "pending", r.Pending(), "err", err)
		}
		return
	}
	r.mu.Lock()
	// Remove what was sent and is still queued: Add may have dropped some
	// of it from the front meanwhile.
	if n := min(end-r.base, len(r.pending)); n > 0 {
		r.pending = r.pending[n:]
		r.base += n
	}
	r.mu.Unlock()
}

// FollowWarm reads the controller's view every interval until ctx ends.
func FollowWarm(ctx context.Context, c *control.RouterClient, view *Cluster, every time.Duration, log *slog.Logger) {
	failing := false
	for {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		w, changed, err := c.Warm(rctx)
		cancel()
		switch {
		case err != nil && !failing:
			log.Warn("controller not reached: routing on the last view", "err", err)
			failing = true
		case err == nil:
			if failing {
				log.Info("controller reached again")
			}
			failing = false
			if changed {
				view.SetWarm(w, time.Now())
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
