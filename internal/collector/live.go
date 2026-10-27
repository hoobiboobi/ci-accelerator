package collector

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"

	"github.com/hoobiboobi/ci-accelerator/internal/diskprobe"
)

// Active is a build a daemon is running now.
type Active struct {
	Daemon  string
	Ref     string
	Target  string
	Started time.Time
}

// GCRule is one of a daemon's cache policies, as it reports it.
type GCRule struct {
	Filters  []string
	All      bool
	Keep     time.Duration
	Reserved int64
	MaxUsed  int64
	MinFree  int64
}

// Mount is one cache mount in a daemon's cache.
type Mount struct {
	Description string
	Size        int64
	LastUsed    time.Time
}

// Usage is a daemon's cache by kind of record, in bytes, as BuildKit
// counts it.
type Usage struct {
	Total, Images, Layers, Mounts, Contexts, Other int64
	// ImageSizes is the size of each base image the cache holds, by name.
	ImageSizes map[string]int64
	MountList  []Mount
}

// Inspection is what a daemon says about itself.
type Inspection struct {
	Node string // the Kubernetes node, when the daemon is labelled with it
	// Persists: the daemon's cache survives its restart (hostPath).
	Persists bool
	Version  string
	GC       []GCRule
	Usage    Usage
	// Disk is the cache's filesystem, from the disk probe next to the
	// daemon; nil without one. DiskErr: the probe did not answer.
	Disk    *diskprobe.Disk
	DiskErr string
}

// Inspector is a Source that can describe its daemon.
type Inspector interface {
	Inspect(ctx context.Context) (Inspection, error)
}

// DaemonState is what the console knows about a daemon now.
type DaemonState struct {
	Name        string
	Connected   bool
	Since       time.Time // connected or disconnected since
	Inspection  Inspection
	InspectedAt time.Time
	InspectErr  string
	Active      []Active
}

// Inspect is how often daemons are described again.
const InspectEvery = time.Minute

type live struct {
	mu      sync.Mutex
	daemons map[string]*DaemonState
	active  map[string]map[string]Active
	// changed is called when what pages show live changes.
	changed func()
}

func (l *live) state(name string) *DaemonState {
	if l.daemons == nil {
		l.daemons, l.active = map[string]*DaemonState{}, map[string]map[string]Active{}
	}
	d := l.daemons[name]
	if d == nil {
		d = &DaemonState{Name: name}
		l.daemons[name] = d
	}
	return d
}

func (l *live) connected(name string, on bool) {
	l.mu.Lock()
	d := l.state(name)
	d.Connected, d.Since = on, time.Now()
	// A new stream reports the running builds again.
	l.active[name] = map[string]Active{}
	l.mu.Unlock()
	l.notify()
}

// forget drops a daemon that is gone for good.
func (l *live) forget(name string) {
	l.mu.Lock()
	delete(l.daemons, name)
	delete(l.active, name)
	l.mu.Unlock()
	l.notify()
}

func (l *live) event(name string, t controlapi.BuildHistoryEventType, rec *controlapi.BuildHistoryRecord) {
	l.mu.Lock()
	l.state(name)
	switch t {
	case controlapi.BuildHistoryEventType_STARTED:
		a := Active{Daemon: name, Ref: rec.Ref, Target: rec.FrontendAttrs["target"]}
		if rec.CreatedAt != nil {
			a.Started = rec.CreatedAt.AsTime()
		}
		l.active[name][rec.Ref] = a
	case controlapi.BuildHistoryEventType_COMPLETE, controlapi.BuildHistoryEventType_DELETED:
		if _, ok := l.active[name][rec.Ref]; !ok {
			l.mu.Unlock()
			return
		}
		delete(l.active[name], rec.Ref)
	}
	l.mu.Unlock()
	l.notify()
}

// inspected records a daemon's inspection; pages hear of it only when it
// changed (the console inspects every minute).
func (l *live) inspected(name string, in Inspection, err error) {
	l.mu.Lock()
	d := l.daemons[name]
	if d == nil {
		// Forgotten while it was inspected: it stays forgotten.
		l.mu.Unlock()
		return
	}
	same := false
	if err != nil {
		same = d.InspectErr == err.Error()
		d.InspectErr = err.Error()
	} else {
		same = d.InspectErr == "" && !d.InspectedAt.IsZero() && reflect.DeepEqual(d.Inspection, in)
		d.Inspection, d.InspectedAt, d.InspectErr = in, time.Now(), ""
	}
	l.mu.Unlock()
	if !same {
		l.notify()
	}
}

func (l *live) notify() {
	if l.changed != nil {
		l.changed()
	}
}

// Daemons returns every daemon the console has reached, by name.
func (c *Collector) Daemons() []DaemonState {
	c.live.mu.Lock()
	defer c.live.mu.Unlock()
	out := make([]DaemonState, 0, len(c.live.daemons))
	for name, d := range c.live.daemons {
		s := *d
		s.Active = nil
		for _, a := range c.live.active[name] {
			s.Active = append(s.Active, a)
		}
		sort.Slice(s.Active, func(i, j int) bool { return s.Active[i].Started.Before(s.Active[j].Started) })
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// OnLiveChange sets what is called when running builds or daemon states
// change.
func (c *Collector) OnLiveChange(fn func()) {
	c.live.mu.Lock()
	defer c.live.mu.Unlock()
	c.live.changed = fn
}

// inspect describes src's daemon now and every InspectEvery until ctx ends.
func (c *Collector) inspect(ctx context.Context, name string, in Inspector) {
	node, persists := "", false
	for {
		ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
		info, err := in.Inspect(ictx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		// The workers' labels come first: stored even when the cache could
		// not be read this time.
		if info.Node != "" && (info.Node != node || info.Persists != persists) {
			// The routers keep a project where its cache is: on the node.
			if serr := c.store.SetDaemonNode(ctx, name, info.Node, info.Persists); serr != nil {
				c.log.Warn("daemon's node not stored", "daemon", name, "err", serr)
			} else {
				node, persists = info.Node, info.Persists
			}
		}
		c.live.inspected(name, info, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(InspectEvery):
		}
	}
}
