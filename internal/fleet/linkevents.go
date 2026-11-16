package fleet

import (
	"sync"
	"time"
	"unicode/utf8"
)

// LinkEvents are the last things the hub saw of each member's link: a
// connection it refused, a link that closed. A cluster's page shows them,
// so an operator sees why it is not connected without the hub's logs. They
// live in the hub's memory only (a restart starts them anew) and are the
// hub's side alone: a member that refuses the hub's key says so only in
// its own log.
//
// A refused connection names the cluster it says it is, which nothing has
// proven yet: each event says where it came from, and the names kept are
// bounded, so made-up names cannot grow the hub's memory.
type LinkEvents struct {
	mu sync.Mutex
	by map[string][]LinkEvent
	// order: the names, the one written to longest ago first.
	order []string
	// now: time.Now when nil (tests set it).
	now func() time.Time
}

// LinkEvent is one thing the hub saw of a member's link.
type LinkEvent struct {
	At   time.Time
	Text string
}

const (
	// LinkEventsKept is how many events are kept per name.
	LinkEventsKept = 5
	// linkEventNames bounds the names kept; the one written to longest ago
	// goes first.
	linkEventNames = 1024
	// linkEventText bounds an event's text, in bytes.
	linkEventText = 300
)

// Add records an event for name; nil keeps nothing.
func (e *LinkEvents) Add(name, text string) {
	if e == nil || !NamePattern.MatchString(name) {
		return
	}
	if len(text) > linkEventText {
		cut := linkEventText
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "…"
	}
	now := time.Now
	if e.now != nil {
		now = e.now
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.by == nil {
		e.by = map[string][]LinkEvent{}
	}
	for i, n := range e.order {
		if n == name {
			e.order = append(e.order[:i], e.order[i+1:]...)
			break
		}
	}
	e.order = append(e.order, name)
	for len(e.order) > linkEventNames {
		delete(e.by, e.order[0])
		e.order = e.order[1:]
	}
	evs := append(e.by[name], LinkEvent{At: now().UTC(), Text: text})
	if len(evs) > LinkEventsKept {
		evs = append([]LinkEvent(nil), evs[len(evs)-LinkEventsKept:]...)
	}
	e.by[name] = evs
}

// Last are name's events, the newest first; nil keeps none.
func (e *LinkEvents) Last(name string) []LinkEvent {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	evs := e.by[name]
	out := make([]LinkEvent, 0, len(evs))
	for i := len(evs) - 1; i >= 0; i-- {
		out = append(out, evs[i])
	}
	return out
}

// Forget drops name's events (a cluster removed: a new one of that name
// starts with none).
func (e *LinkEvents) Forget(name string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.by, name)
	for i, n := range e.order {
		if n == name {
			e.order = append(e.order[:i], e.order[i+1:]...)
			break
		}
	}
}
