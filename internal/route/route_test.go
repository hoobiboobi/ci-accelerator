package route

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func daemons(spec ...string) []Daemon {
	// "node/zone" ready; "!node/zone" not ready.
	var out []Daemon
	for _, s := range spec {
		ready := s[0] != '!'
		if !ready {
			s = s[1:]
		}
		node, zone := s, ""
		for i := range s {
			if s[i] == '/' {
				node, zone = s[:i], s[i+1:]
			}
		}
		out = append(out, Daemon{Name: "bk-" + node, Node: node, Zone: zone, Addr: "10.0.0." + node, Ready: ready})
	}
	return out
}

func ago(node string, d time.Duration) Activity { return Activity{Node: node, At: now.Add(-d)} }

var projA = &Project{Instance: "0123456789ab", ID: 7}

func TestDecide(t *testing.T) {
	cold, _ := hash(projA.Key(), daemons("n1/a", "n2/a", "n3/a"))
	for _, tc := range []struct {
		name    string
		req     Request
		ds      []Daemon
		warm    []Activity
		pol     Policy
		node    string
		reason  Reason
		wantErr bool
	}{
		{name: "local warm", req: Request{Project: projA, Node: "n1", Zone: "a"}, ds: daemons("n1/a", "n2/a"),
			warm: []Activity{ago("n1", time.Hour), ago("n2", time.Minute)}, node: "n1", reason: LocalWarm},
		{name: "warm elsewhere in the zone beats a cold local daemon", req: Request{Project: projA, Node: "n1", Zone: "a"},
			ds: daemons("n1/a", "n2/a", "n3/a"), warm: []Activity{ago("n3", time.Hour)}, node: "n3", reason: ZoneWarm},
		{name: "the warmest of several", req: Request{Project: projA, Node: "n1", Zone: "a"}, ds: daemons("n1/a", "n2/a", "n3/a"),
			warm: []Activity{ago("n2", 3*time.Hour), ago("n3", time.Hour), ago("n2", 2*time.Hour)}, node: "n3", reason: ZoneWarm},
		{name: "warmth expires", req: Request{Project: projA, Node: "n1", Zone: "a"}, ds: daemons("n1/a", "n2/a", "n3/a"),
			warm: []Activity{ago("n2", 8*24*time.Hour)}, pol: Policy{WarmFor: 7 * 24 * time.Hour}, node: cold.Node, reason: ZoneHash},
		{name: "without a window, what is listed is warm", req: Request{Project: projA, Node: "n1", Zone: "a"}, ds: daemons("n1/a", "n2/a", "n3/a"),
			warm: []Activity{ago("n2", 30*24*time.Hour)}, node: "n2", reason: ZoneWarm},
		{name: "cold zone: the project's hash", req: Request{Project: projA, Node: "n1", Zone: "a"}, ds: daemons("n1/a", "n2/a", "n3/a"),
			node: cold.Node, reason: ZoneHash},
		{name: "a warm daemon in another zone is not used while the zone has one", req: Request{Project: projA, Node: "n1", Zone: "a"},
			ds: daemons("n1/a", "n2/b"), warm: []Activity{ago("n2", time.Minute)}, node: "n1", reason: ZoneHash},
		{name: "not ready daemons are skipped", req: Request{Project: projA, Node: "n1", Zone: "a"}, ds: daemons("!n1/a", "n2/a"),
			warm: []Activity{ago("n1", time.Minute)}, node: "n2", reason: ZoneHash},
		{name: "no daemon ready in the zone: cross zone when allowed", req: Request{Project: projA, Node: "n1", Zone: "a"},
			ds: daemons("!n1/a", "n2/b"), pol: Policy{CrossZone: true}, node: "n2", reason: CrossZone},
		{name: "no daemon ready in the zone: refused", req: Request{Project: projA, Node: "n1", Zone: "a"},
			ds: daemons("!n1/a", "n2/b"), wantErr: true},
		{name: "no daemon ready at all", req: Request{Project: projA, Node: "n1"}, ds: daemons("!n1", "!n2"), pol: Policy{CrossZone: true}, wantErr: true},
		{name: "unknown job: its node's daemon even if cold", req: Request{Node: "n2", Zone: "a"}, ds: daemons("n1/a", "n2/a"),
			node: "n2", reason: UnknownLocal},
		{name: "unknown job on a node without a daemon", req: Request{Node: "n9", Zone: "a"}, ds: daemons("n1/a"), node: "n1", reason: UnknownHash},
		{name: "nodes without zones are one zone", req: Request{Project: projA, Node: "n1"}, ds: daemons("n1", "n2", "n3"),
			warm: []Activity{ago("n2", time.Minute)}, node: "n2", reason: ZoneWarm},
		{name: "overloaded local warm daemon spills to a warm neighbour", req: Request{Project: projA, Node: "n1", Zone: "a"},
			ds: func() []Daemon { d := daemons("n1/a", "n2/a"); d[0].Active = 5; return d }(), pol: Policy{Overload: 4},
			warm: []Activity{ago("n1", time.Minute), ago("n2", time.Hour)}, node: "n2", reason: ZoneWarm},
		{name: "every daemon overloaded: still the warm one", req: Request{Project: projA, Node: "n1", Zone: "a"},
			ds: func() []Daemon { d := daemons("n1/a", "n2/a"); d[0].Active, d[1].Active = 5, 5; return d }(), pol: Policy{Overload: 4},
			warm: []Activity{ago("n2", time.Minute)}, node: "n2", reason: ZoneWarm},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Decide(tc.req, tc.ds, tc.warm, tc.pol, now)
			if tc.wantErr {
				if !errors.Is(err, ErrNoDaemon) {
					t.Fatalf("got %+v, %v; want ErrNoDaemon", d, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.Daemon.Node != tc.node || d.Reason != tc.reason {
				t.Errorf("got %s (%s), want %s (%s)", d.Daemon.Node, d.Reason, tc.node, tc.reason)
			}
		})
	}
}

// A daemon's restart gives it a new pod name and address on the same
// node: the project stays where its cache is.
func TestRolloutKeepsWarmth(t *testing.T) {
	before := daemons("n1/a", "n2/a", "n3/a")
	warm := []Activity{ago("n2", time.Hour)}
	d1, _ := Decide(Request{Project: projA, Node: "n1", Zone: "a"}, before, warm, Policy{}, now)
	after := daemons("n1/a", "n2/a", "n3/a")
	for i := range after {
		after[i].Name += "-new"
		after[i].Addr += "9"
	}
	d2, _ := Decide(Request{Project: projA, Node: "n1", Zone: "a"}, after, warm, Policy{}, now)
	if d1.Daemon.Node != "n2" || d2.Daemon.Node != "n2" || d2.Daemon.Name != "bk-n2-new" {
		t.Errorf("before %+v, after %+v", d1, d2)
	}
	// Cold projects keep their daemon too: the hash is over nodes.
	c1, _ := Decide(Request{Project: projA, Node: "n1", Zone: "a"}, before, nil, Policy{}, now)
	c2, _ := Decide(Request{Project: projA, Node: "n1", Zone: "a"}, after, nil, Policy{}, now)
	if c1.Daemon.Node != c2.Daemon.Node {
		t.Errorf("cold project moved from %s to %s", c1.Daemon.Node, c2.Daemon.Node)
	}
}

// The parallel jobs of a new project start on different nodes, through
// different routers, before any of them is known: they meet on one daemon.
func TestConcurrentColdJobsConverge(t *testing.T) {
	ds := daemons("n1/a", "n2/a", "n3/a", "n4/a")
	for p := uint64(1); p <= 50; p++ {
		proj := &Project{Instance: "0123456789ab", ID: p}
		seen := map[string]bool{}
		for _, node := range []string{"n1", "n2", "n3", "n4"} {
			// Each router lists the daemons in its own order.
			order := append([]Daemon(nil), ds...)
			for i := range order {
				j := (i + len(node)*int(p)) % len(order)
				order[i], order[j] = order[j], order[i]
			}
			d, err := Decide(Request{Project: proj, Node: node, Zone: "a"}, order, nil, Policy{}, now)
			if err != nil {
				t.Fatal(err)
			}
			seen[d.Daemon.Node] = true
		}
		if len(seen) != 1 {
			t.Fatalf("project %d spread over %v", p, seen)
		}
	}
}

// The hash spreads projects over the daemons and moves only the projects
// of a daemon that leaves.
func TestHashSpreadsAndMovesLittle(t *testing.T) {
	ds := daemons("n1", "n2", "n3", "n4")
	count := map[string]int{}
	moved := 0
	for p := 0; p < 2000; p++ {
		key := fmt.Sprintf("x/%d", p)
		d, _ := hash(key, ds)
		count[d.Node]++
		d2, _ := hash(key, ds[:3])
		if d.Node != "n4" && d2.Node != d.Node {
			moved++
		}
	}
	for n, c := range count {
		if c < 400 || c > 600 {
			t.Errorf("%s got %d of 2000 projects", n, c)
		}
	}
	if moved != 0 {
		t.Errorf("%d projects moved although their daemon stayed", moved)
	}
}
