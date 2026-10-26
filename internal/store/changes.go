package store

import (
	"fmt"
	"sort"
	"sync"
)

// PipelineKey names a pipeline in change notifications: its instance and
// id, as in the console's addresses ("0a1b2c/123").
func PipelineKey(instance string, pipeline uint64) string {
	return fmt.Sprintf("%s/%d", instance, pipeline)
}

// Subscription receives the pipelines whose builds or jobs changed. C
// receives when Take has something; changes in between are merged, none
// is lost and a slow reader never blocks a writer.
type Subscription struct {
	C <-chan struct{}

	c       chan struct{}
	mu      sync.Mutex
	pending map[string]bool
}

// Take returns the pipelines changed since the last Take, sorted.
func (sub *Subscription) Take() []string {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	out := make([]string, 0, len(sub.pending))
	for k := range sub.pending {
		out = append(out, k)
	}
	clear(sub.pending)
	sort.Strings(out)
	return out
}

// Changes hands change notifications to subscribers. The store has one;
// a process that reads another process's store keeps its own and feeds it
// from that store's stream.
type Changes struct {
	mu   sync.Mutex
	subs map[*Subscription]bool
}

// Subscribe starts receiving changes; Unsubscribe ends it.
func (c *Changes) Subscribe() *Subscription {
	ch := make(chan struct{}, 1)
	sub := &Subscription{C: ch, c: ch, pending: map[string]bool{}}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.subs == nil {
		c.subs = map[*Subscription]bool{}
	}
	c.subs[sub] = true
	return sub
}

// Unsubscribe ends sub.
func (c *Changes) Unsubscribe(sub *Subscription) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.subs, sub)
}

// Announce tells every subscriber that keys changed.
func (c *Changes) Announce(keys ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for sub := range c.subs {
		sub.mu.Lock()
		for _, k := range keys {
			sub.pending[k] = true
		}
		sub.mu.Unlock()
		select {
		case sub.c <- struct{}{}:
		default:
		}
	}
}

// Subscribe starts receiving the store's changes; Unsubscribe ends it.
func (s *Store) Subscribe() *Subscription { return s.changes.Subscribe() }

// Unsubscribe ends sub.
func (s *Store) Unsubscribe(sub *Subscription) { s.changes.Unsubscribe(sub) }

// changed tells the subscribers, after a commit, which pipelines changed.
// Builds without a pipeline are on no page that updates itself.
func (s *Store) changed(keys ...string) { s.changes.Announce(keys...) }
