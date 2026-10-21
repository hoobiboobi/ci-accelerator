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

type changes struct {
	mu   sync.Mutex
	subs map[*Subscription]bool
}

// Subscribe starts receiving changes; Unsubscribe ends it.
func (s *Store) Subscribe() *Subscription {
	c := make(chan struct{}, 1)
	sub := &Subscription{C: c, c: c, pending: map[string]bool{}}
	s.changes.mu.Lock()
	defer s.changes.mu.Unlock()
	if s.changes.subs == nil {
		s.changes.subs = map[*Subscription]bool{}
	}
	s.changes.subs[sub] = true
	return sub
}

func (s *Store) Unsubscribe(sub *Subscription) {
	s.changes.mu.Lock()
	defer s.changes.mu.Unlock()
	delete(s.changes.subs, sub)
}

// changed tells the subscribers, after a commit, which pipelines changed.
// Builds without a pipeline are on no page that updates itself.
func (s *Store) changed(keys ...string) {
	s.changes.mu.Lock()
	defer s.changes.mu.Unlock()
	for sub := range s.changes.subs {
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
