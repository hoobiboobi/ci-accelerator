package route

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// A project warm on two nodes (one job was sent elsewhere while its
// node's daemon restarted, or a dial to it failed once) comes back to one
// node: the one it used most recently, wherever its jobs run. Before, a
// job on each warm node stayed there, and the project's parallel jobs
// were split for good.
func TestSplitProjectConverges(t *testing.T) {
	ds := daemons("n1/a", "n2/a", "n3/a")
	warm := []Activity{ago("n1", 6*24*time.Hour), ago("n2", time.Minute)}
	for _, node := range []string{"n1", "n2", "n3"} {
		d, err := Decide(Request{Project: projA, Node: node, Zone: "a"}, ds, warm, Policy{WarmFor: 7 * 24 * time.Hour}, now)
		if err != nil {
			t.Fatal(err)
		}
		if d.Daemon.Node != "n2" {
			t.Errorf("a job on %s went to %s (%s), want n2, where the project built last", node, d.Daemon.Node, d.Reason)
		}
	}
	// The diversion: n1's daemon not ready, the job goes to n2; once n1
	// is back, the project's jobs stay on n2 (its decision is the most
	// recent), not split between n1 and n2.
	warm = []Activity{ago("n1", time.Hour)}
	d, _ := Decide(Request{Project: projA, Node: "n1", Zone: "a"}, daemons("!n1/a", "n2/a", "n3/a"), warm, Policy{}, now)
	warm = append(warm, Activity{Node: d.Daemon.Node, At: now})
	seen := map[string]bool{}
	for _, node := range []string{"n1", "n2", "n3"} {
		d, _ := Decide(Request{Project: projA, Node: node, Zone: "a"}, ds, warm, Policy{}, now.Add(time.Minute))
		seen[d.Daemon.Node] = true
	}
	if len(seen) != 1 {
		t.Errorf("after one diversion the project's jobs go to %v", seen)
	}
}

// Connections of unknown jobs keep to the policy: no other zone when
// crossing zones is not allowed, not the local daemon when it is
// overloaded and another is free, and spread by their node, not all to
// one daemon.
func TestUnknownJobsKeepThePolicy(t *testing.T) {
	ds := daemons("n1/a", "n2/a", "n3/b")
	if d, err := Decide(Request{Node: "n9", Zone: "z9"}, ds, nil, Policy{}, now); !errors.Is(err, ErrNoDaemon) {
		t.Errorf("an unknown job of a zone without daemons, crossZone off: %+v, %v", d, err)
	}
	if d, err := Decide(Request{Node: "n9", Zone: "z9"}, ds, nil, Policy{CrossZone: true}, now); err != nil || d.Reason != UnknownHash {
		t.Errorf("an unknown job of a zone without daemons, crossZone on: %+v, %v", d, err)
	}
	busy := daemons("n1/a", "n2/a")
	busy[0].Active = 50
	if d, _ := Decide(Request{Node: "n1", Zone: "a"}, busy, nil, Policy{Overload: 4}, now); d.Daemon.Node != "n2" {
		t.Errorf("an unknown job on an overloaded node went to %s (%s)", d.Daemon.Node, d.Reason)
	}
	busy[1].Active = 50
	if d, _ := Decide(Request{Node: "n1", Zone: "a"}, busy, nil, Policy{Overload: 4}, now); d.Daemon.Node != "n1" || d.Reason != UnknownLocal {
		t.Errorf("every daemon overloaded: an unknown job went to %s (%s), want its own node", d.Daemon.Node, d.Reason)
	}
	many := daemons("n1/a", "n2/a", "n3/a", "n4/a")
	got := map[string]int{}
	for i := range 1000 {
		d, err := Decide(Request{Node: fmt.Sprintf("x%d", i), Zone: "a"}, many, nil, Policy{}, now)
		if err != nil {
			t.Fatal(err)
		}
		got[d.Daemon.Node]++
	}
	if len(got) != 4 {
		t.Errorf("unknown jobs of 1000 nodes without a daemon: %v", got)
	}
}
