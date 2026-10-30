package collector

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/moby/buildkit/client"

	"github.com/hoobiboobi/ci-accelerator/internal/diskprobe"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// LiveBuild is a build that is still running, as its page shows it.
type LiveBuild struct {
	Active
	Steps        []history.LiveStep
	ContextBytes int64
	// Done: BuildKit's stream of the build ended: it completed, failed or
	// was canceled, and the console stores it in a moment.
	Done bool
	// Err: the build's progress could not be read; Steps are what was
	// read before.
	Err string
	// Load is the daemon's node now, from the probe next to the daemon
	// (at most LoadEvery old); nil without one.
	Load *diskprobe.Load
	// Running is how many builds the daemon runs now, this one included.
	Running int
}

// Statuser streams a running build's progress from its first update on
// (BuildKit's Status call replays what was sent before).
type Statuser interface {
	Status(ctx context.Context, ref string, fn func(*client.SolveStatus)) error
}

// LoadReader reads the load of the daemon's node from its probe; nil
// without one.
type LoadReader interface {
	ReadLoad(ctx context.Context) (*diskprobe.Load, error)
}

const (
	// MaxWatched bounds the builds whose progress is read at once.
	MaxWatched = 32
	// LoadEvery is how often the load of a followed build's node is read.
	LoadEvery = 10 * time.Second
)

// Variables for the tests.
var (
	// watchFor is how long a running build's progress is read after its
	// page last asked for it; watchTick how often that is checked.
	watchFor  = time.Minute
	watchTick = time.Second
	// firstWait is how long a page waits for a build's progress the first
	// time it asks (the replay of what the build did so far).
	firstWait = 2 * time.Second
)

// ErrTooManyWatched: MaxWatched running builds are followed already.
var ErrTooManyWatched = errors.New("collector: too many running builds are followed at once")

// ErrNoProgress: the daemon's stream cannot read a build's progress.
var ErrNoProgress = errors.New("collector: the daemon's progress cannot be read")

type watches struct {
	mu sync.Mutex
	m  map[string]*watcher
}

// watcher reads one running build's progress while its page is open.
type watcher struct {
	first chan struct{} // closed with the first update, or the end

	mu    sync.Mutex
	live  *history.Live
	done  bool
	err   string
	until time.Time
	// ended: the reading stopped (the build ended, it failed, or nobody
	// asked), at endedAt.
	ended   bool
	endedAt time.Time
	load    *diskprobe.Load
}

// retryAfter is how long a build whose progress could not be read shows
// that, before it is read again.
const retryAfter = 30 * time.Second

// LiveBuild is a running build of daemon now. Its progress is read from
// the first call on, for a minute after the last; nil when the daemon runs
// no build ref (any more).
func (c *Collector) LiveBuild(ctx context.Context, daemon, ref string) (*LiveBuild, error) {
	c.live.mu.Lock()
	a, ok := c.live.active[daemon][ref]
	src := c.live.sources[daemon]
	args := c.live.args[daemon][ref]
	running := len(c.live.active[daemon])
	c.live.mu.Unlock()
	if !ok {
		return nil, nil
	}
	st, ok := src.(Statuser)
	if !ok {
		return nil, ErrNoProgress
	}
	now := time.Now()
	key := daemon + "\x00" + ref
	c.watch.mu.Lock()
	if c.watch.m == nil {
		c.watch.m = map[string]*watcher{}
	}
	for k, w := range c.watch.m {
		w.mu.Lock()
		stale := w.ended && now.After(w.until)
		w.mu.Unlock()
		if stale {
			delete(c.watch.m, k)
		}
	}
	w := c.watch.m[key]
	if w != nil {
		w.mu.Lock()
		if w.ended && !w.done && (w.err == "" || now.Sub(w.endedAt) > retryAfter) {
			// Nobody asked for a while and the reading stopped, or it
			// failed a while ago: read the build again from its start.
			w = nil
		} else {
			w.until = now.Add(watchFor)
		}
		if w != nil {
			w.mu.Unlock()
		}
	}
	if w == nil {
		if len(c.watch.m) >= MaxWatched {
			c.watch.mu.Unlock()
			return nil, ErrTooManyWatched
		}
		w = &watcher{first: make(chan struct{}), live: history.NewLive(args), until: now.Add(watchFor)}
		c.watch.m[key] = w
		lr, _ := src.(LoadReader)
		go c.follow(w, st, lr, daemon, ref, watchTick)
	}
	c.watch.mu.Unlock()
	select {
	case <-w.first:
	case <-time.After(firstWait):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	lb := &LiveBuild{Active: a, Running: running}
	w.mu.Lock()
	lb.Steps, lb.ContextBytes = w.live.Steps(), w.live.ContextBytes
	lb.Done, lb.Err, lb.Load = w.done, w.err, w.load
	w.mu.Unlock()
	return lb, nil
}

// follow reads a build's progress, and its node's load, until the build
// ends or nobody asked for watchFor.
func (c *Collector) follow(w *watcher, st Statuser, lr LoadReader, daemon, ref string, tick time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once
	opened := func() { once.Do(func() { close(w.first) }) }
	defer opened()
	go func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				w.mu.Lock()
				over := now.After(w.until)
				w.mu.Unlock()
				if over {
					cancel()
					return
				}
			}
		}
	}()
	if lr != nil {
		go func() {
			for {
				lctx, done := context.WithTimeout(ctx, 3*time.Second)
				l, err := lr.ReadLoad(lctx)
				done()
				if err == nil && l != nil {
					w.mu.Lock()
					w.load = l
					w.mu.Unlock()
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(LoadEvery):
				}
			}
		}()
	}
	err := st.Status(ctx, ref, func(s *client.SolveStatus) {
		w.mu.Lock()
		w.live.Add(s)
		w.mu.Unlock()
		opened()
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ended, w.endedAt = true, time.Now()
	if ctx.Err() != nil {
		return // nobody asks any more
	}
	if err != nil {
		w.err = err.Error()
		if c.log != nil {
			c.log.Debug("running build's progress not read", "daemon", daemon, "ref", ref, "err", err)
		}
		return
	}
	w.done = true
}
