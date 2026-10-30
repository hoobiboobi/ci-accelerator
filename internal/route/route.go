// Package route decides which build daemon serves a CI job's connection.
//
// A daemon's cache lives on its node (hostPath), so a project's builds are
// fast where it built before. The policy, in order:
//
//  1. the daemon on the job's own node, if the project is warm there;
//  2. the warmest ready daemon of the job's zone;
//  3. no warm daemon in the zone: a rendezvous hash of the project over the
//     zone's ready daemons. Every router computes the same answer, so the
//     parallel jobs of a new project meet on one daemon and BuildKit
//     deduplicates their common steps;
//  4. no ready daemon in the zone: the hash over every ready daemon, or no
//     daemon when crossing zones is not allowed.
//
// A job on a node whose zone is not known (no zone label, or no daemon ever
// seen there) has every daemon as its zone.
//
// A connection whose job is unknown goes to the daemon on its own node, as
// a plain Service with PreferSameNode would send it. Decide is a pure
// function: the router feeds it what it watches.
package route

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"strconv"
	"time"
)

// Project names a CI project: its GitLab instance (trace-id prefix) and id.
type Project struct {
	Instance string `json:"instance"`
	ID       uint64 `json:"id"`
}

// Key is the project's stable text form, the input of the hash.
func (p Project) Key() string { return p.Instance + "/" + strconv.FormatUint(p.ID, 10) }

// Daemon is a build daemon the router can send connections to.
type Daemon struct {
	Name  string // pod name
	Node  string
	Zone  string // "" when the node has no zone label
	Addr  string // pod IP
	Ready bool
	// Active is the number of builds it runs now, as far as known.
	Active int
}

// Activity says a project built (or was sent) on a node at a time.
type Activity struct {
	Node string    `json:"node"`
	At   time.Time `json:"at"`
}

// Request is one connection to route.
type Request struct {
	// Project is the job's project; nil when the connection's job is not
	// known (a pod that is no CI job, or not watched yet).
	Project *Project
	// Node and Zone are where the job runs.
	Node, Zone string
}

// Policy tunes the decision.
type Policy struct {
	// CrossZone lets a job use a daemon of another zone when its own zone
	// has none ready.
	CrossZone bool
	// Overload skips a daemon running this many builds or more, when
	// another one is free (0: never).
	Overload int
	// WarmFor is how long activity keeps a project warm on a node; 0: as
	// long as it is listed (the controller already keeps only the window it
	// was told).
	WarmFor time.Duration
}

// Reason says why a daemon was chosen.
type Reason string

const (
	LocalWarm    Reason = "local-warm"    // the job's node, whose cache holds the project's layers
	ZoneWarm     Reason = "zone-warm"     // the zone's node with the project's most recently used layers
	ZoneHash     Reason = "zone-hash"     // nobody in the zone built it: the project's daemon by hash
	CrossZone    Reason = "cross-zone"    // no daemon ready in the job's zone
	UnknownLocal Reason = "unknown-local" // job not known: the daemon on its node
	UnknownHash  Reason = "unknown-hash"  // job not known and no daemon on its node: by hash
)

// Decision is the daemon chosen and why.
type Decision struct {
	Daemon Daemon
	Reason Reason
	// Candidates is how many daemons were considered for the last rule.
	Candidates int
}

// ErrNoDaemon: no daemon can take the connection.
var ErrNoDaemon = errors.New("route: no ready build daemon")

// Decide picks the daemon for r. warm is the project's activity by node
// (any order, older entries are ignored); now is the time of the decision.
func Decide(r Request, daemons []Daemon, warm []Activity, p Policy, now time.Time) (Decision, error) {
	ready := make([]Daemon, 0, len(daemons))
	for _, d := range daemons {
		if d.Ready {
			ready = append(ready, d)
		}
	}
	if len(ready) == 0 {
		return Decision{}, ErrNoDaemon
	}
	var local *Daemon
	for i := range ready {
		if ready[i].Node == r.Node {
			local = &ready[i]
			break
		}
	}
	if r.Project == nil {
		if local != nil {
			return Decision{Daemon: *local, Reason: UnknownLocal, Candidates: 1}, nil
		}
		d, n := hash("", free(inZone(ready, r.Zone, true), p))
		return Decision{Daemon: d, Reason: UnknownHash, Candidates: n}, nil
	}
	key := r.Project.Key()
	last := map[string]time.Time{}
	for _, a := range warm {
		if (p.WarmFor == 0 || now.Sub(a.At) <= p.WarmFor) && a.At.After(last[a.Node]) {
			last[a.Node] = a.At
		}
	}
	if local != nil && !last[local.Node].IsZero() && !busy(*local, p) {
		return Decision{Daemon: *local, Reason: LocalWarm, Candidates: 1}, nil
	}
	zone := inZone(ready, r.Zone, false)
	if len(zone) > 0 {
		var warmest []Daemon
		var at time.Time
		for _, d := range free(zone, p) {
			t := last[d.Node]
			switch {
			case t.IsZero() || t.Before(at):
			case t.After(at):
				warmest, at = []Daemon{d}, t
			default:
				warmest = append(warmest, d)
			}
		}
		if len(warmest) > 0 {
			d, _ := hash(key, warmest)
			return Decision{Daemon: d, Reason: ZoneWarm, Candidates: len(zone)}, nil
		}
		d, n := hash(key, free(zone, p))
		return Decision{Daemon: d, Reason: ZoneHash, Candidates: n}, nil
	}
	if !p.CrossZone {
		return Decision{}, ErrNoDaemon
	}
	d, n := hash(key, free(ready, p))
	return Decision{Daemon: d, Reason: CrossZone, Candidates: n}, nil
}

// inZone keeps the daemons of zone; an unknown zone ("") is every daemon's.
// all: an empty result falls back to every daemon.
func inZone(ds []Daemon, zone string, all bool) []Daemon {
	if zone == "" {
		return ds
	}
	var out []Daemon
	for _, d := range ds {
		if d.Zone == zone || d.Zone == "" {
			out = append(out, d)
		}
	}
	if len(out) == 0 && all {
		return ds
	}
	return out
}

func busy(d Daemon, p Policy) bool { return p.Overload > 0 && d.Active >= p.Overload }

// free drops the overloaded daemons, unless that would drop them all.
func free(ds []Daemon, p Policy) []Daemon {
	var out []Daemon
	for _, d := range ds {
		if !busy(d, p) {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return ds
	}
	return out
}

// hash is rendezvous hashing: the daemon whose node scores highest for
// key. Keyed by node, not pod, so a daemon's restart keeps its projects.
func hash(key string, ds []Daemon) (Daemon, int) {
	sorted := append([]Daemon(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Node < sorted[j].Node })
	var best Daemon
	var top uint64
	for i, d := range sorted {
		sum := sha256.Sum256([]byte(key + "|" + d.Node))
		if s := binary.BigEndian.Uint64(sum[:8]); i == 0 || s > top {
			best, top = d, s
		}
	}
	return best, len(ds)
}
