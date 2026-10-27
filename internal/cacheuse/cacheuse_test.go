package cacheuse

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// The fixtures are a real daemon's disk usage and the two builds of one
// pipeline that ran on it (api and lint; lint received the shared base
// steps from api, so api executed them).
func load(t *testing.T) ([]Record, []Step) {
	t.Helper()
	var recs []Record
	b, err := os.ReadFile("testdata/du.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &recs); err != nil {
		t.Fatal(err)
	}
	var builds []struct {
		Build  int64
		Target string
		Steps  []struct {
			Key, Name, State   string
			Started, Completed time.Time
		}
	}
	b, err = os.ReadFile("testdata/builds.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &builds); err != nil {
		t.Fatal(err)
	}
	var steps []Step
	for _, bd := range builds {
		for _, s := range bd.Steps {
			if s.State != "built" {
				continue
			}
			steps = append(steps, Step{Key: s.Key, Name: s.Name, Started: s.Started, Completed: s.Completed,
				Owner: Owner{Instance: "993196", Project: 7, Target: bd.Target, Build: bd.Build}})
		}
	}
	return recs, steps
}

func TestDescribe(t *testing.T) {
	for _, c := range []struct{ key, name, want string }{
		{"[deps 2/2]", "RUN --mount=type=cache,target=/root/.cache/pip pip install -r requirements.txt",
			"mount / from exec /bin/sh -c pip install -r requirements.txt"},
		{"[x 1/1]", "RUN --network=none --mount=type=secret,id=a  make", "mount / from exec /bin/sh -c make"},
		{"[base 4/4]", "WORKDIR /app", "[base 4/4] WORKDIR /app"},
		{"[stage-0 3/5]", "COPY f.txt /f.txt", "[stage-0 3/5] COPY f.txt /f.txt"},
		{"[base 1/4]", "FROM docker.io/library/alpine:3.22", ""},
		{"[x 1/1]", `RUN ["make", "all"]`, ""},
		{"[x 1/1]", "RUN --mount=type=cache", ""},
	} {
		if got := Describe(c.key, c.name); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.key, c.name, got, c.want)
		}
	}
}

func TestKinds(t *testing.T) {
	recs, _ := load(t)
	count := map[Kind]int{}
	for _, r := range recs {
		count[KindOf(r)]++
	}
	if count[Base] != 4 || count[Context] != 6 || count[Mount] != 1 || count[Layer] != 13 {
		t.Errorf("kinds %v", count)
	}
}

// Every layer of the real daemon is the pipeline's project's; the steps
// lint received from api are api's (it executed them).
func TestRealDaemon(t *testing.T) {
	recs, steps := load(t)
	targets := map[string]string{}
	for _, r := range recs {
		if KindOf(r) != Layer {
			continue
		}
		o := Match(r, steps)
		if len(o) != 1 || o[0].Project != 7 {
			t.Errorf("%s %q: owners %+v", r.ID, r.Description, o)
			continue
		}
		targets[r.Description] = o[0].Target
	}
	for desc, want := range map[string]string{
		"mount / from exec /bin/sh -c pip install -r requirements.txt":     "api",
		"mount / from exec /bin/sh -c pip install -r requirements-dev.txt": "lint",
		"mount / from exec /bin/sh -c ruff check .":                        "lint",
		"[base 4/4] WORKDIR /app":                                          "api",
	} {
		if targets[desc] != want {
			t.Errorf("%q made by %q, want %q", desc, targets[desc], want)
		}
	}
}

// Two projects executing the same step at once make one record, which
// both own; a step at another time is no match.
func TestSharedAndTime(t *testing.T) {
	recs, steps := load(t)
	var rec Record
	for _, r := range recs {
		if r.Description == "mount / from exec /bin/sh -c useradd --create-home app" {
			rec = r
		}
	}
	var mine Step
	for _, s := range steps {
		if s.Name == "RUN useradd --create-home app" {
			mine = s
		}
	}
	other := mine
	other.Owner = Owner{Instance: "993196", Project: 9, Target: "web", Build: 99}
	if o := Match(rec, append(steps, other)); len(o) != 2 || o[0].Project != 7 || o[1].Project != 9 {
		t.Errorf("shared: %+v", o)
	}
	late := other
	late.Started, late.Completed = mine.Completed.Add(time.Minute), mine.Completed.Add(2*time.Minute)
	if o := Match(rec, append(steps, late)); len(o) != 1 || o[0].Project != 7 {
		t.Errorf("a later run of the step: %+v", o)
	}
	if o := Match(rec, nil); o != nil {
		t.Errorf("no builds: %+v", o)
	}
	// Two builds of one project: the earlier owns it.
	again := mine
	again.Owner.Build = 0
	if o := Match(rec, append(steps, again)); len(o) != 1 || o[0].Build != 0 {
		t.Errorf("same project twice: %+v", o)
	}
}
