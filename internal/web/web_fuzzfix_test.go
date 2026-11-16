package web

import (
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// hostileInstances are instances a cluster at a fleet hub could answer
// with: none of them is six hex digits.
var hostileInstances = []string{"", ".", "..", "../..", "../../~", "0a1b2", "0a1b2c3", "0a1b2g", "0a/1b2", "%2e%2e", "..%2f", `..\..`, "0a1b2c/.."}

// TestInstanceSegmentStaysUnderBase: an address built from a cluster's
// instance stays under the member's base after a browser's (or
// http.Redirect's) dot-segment cleaning.
func TestInstanceSegmentStaysUnderBase(t *testing.T) {
	for _, ok := range []string{"0a1b2c", "ABCDEF", "000000"} {
		if got := instSeg(ok); got != ok {
			t.Errorf("instSeg(%q) = %q, want it unchanged", ok, got)
		}
	}
	const base = "/m/eu"
	for _, in := range hostileInstances {
		got := instSeg(in)
		if got == in {
			t.Errorf("instSeg(%q) kept a value that is not an instance", in)
		}
		if _, ok := store.ParseProjectKey(got + "/1"); ok {
			t.Errorf("instSeg(%q) = %q, a segment the pages accept", in, got)
		}
		for _, u := range []string{
			base + "/pipelines/" + pipeKey(in, 7),
			base + "/projects/" + instSeg(in) + "/42",
			base + "/jobs/" + instSeg(in) + "/9/log",
		} {
			if c := path.Clean(strings.ReplaceAll(u, `\`, "/")); !strings.HasPrefix(c, base+"/") {
				t.Errorf("instance %q: %q resolves to %q, outside %s/", in, u, c, base)
			}
		}
	}
}

// TestPipeCardHostileInstance: a pipeline card's link and its jobs' links
// carry no instance that is not one.
func TestPipeCardHostileInstance(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return at }}
	for _, in := range hostileInstances {
		jobs := []store.JobInfo{{Instance: in, ID: 3, PipelineID: 7, Name: "build", Stage: "build", Status: "success",
			Started: at.Add(-time.Minute), Finished: at.Add(-30 * time.Second)}}
		c := s.pipeCard(store.PipeRow{Instance: in, ID: 7, Status: "success", Created: at.Add(-2 * time.Minute)}, jobs, nil, time.UTC)
		if c.URL != "/pipelines/invalid/7" {
			t.Errorf("instance %q: card links %q", in, c.URL)
		}
		for _, l := range c.Lanes {
			for _, j := range l.Jobs {
				if j.URL != c.URL {
					t.Errorf("instance %q: job links %q", in, j.URL)
				}
			}
		}
	}
}

// TestPipeCardPositionsClamped: a job that ends before it starts (times
// from another cluster) still lays out inside the card.
func TestPipeCardPositionsClamped(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return at }}
	t0 := at.Add(-time.Hour)
	jobs := []store.JobInfo{
		{Instance: "0a1b2c", ID: 1, PipelineID: 7, Name: "a", Stage: "build", Status: "success", Started: t0, Finished: t0.Add(time.Second)},
		{Instance: "0a1b2c", ID: 2, PipelineID: 7, Name: "b", Stage: "test", Status: "success", Started: t0.Add(10 * time.Minute),
			Finished: t0.Add(5 * time.Second), Waited: 20 * time.Minute},
	}
	c := s.pipeCard(store.PipeRow{Instance: "0a1b2c", ID: 7, Status: "success", Created: t0}, jobs, nil, time.UTC)
	n := 0
	for _, l := range c.Lanes {
		for _, j := range l.Jobs {
			for name, v := range map[string]string{"QL": j.QL, "QW": j.QW, "L": j.L, "W": j.W} {
				if v == "" {
					continue
				}
				n++
				f, err := strconv.ParseFloat(v, 64)
				if err != nil || f < 0 || f > 100 {
					t.Errorf("job %s: %s = %q, not in [0,100]", j.Title, name, v)
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no job laid out")
	}
}

// TestWaterfallLongAxis: an axis of years (times from another cluster)
// keeps a positive span instead of overflowing.
func TestWaterfallLongAxis(t *testing.T) {
	start := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, d := range []time.Duration{time.Nanosecond, time.Minute, 3 * 365 * 24 * time.Hour, 200 * 365 * 24 * time.Hour,
		time.Duration(1<<63 - 1)} {
		end := start.Add(d)
		w := waterfall("p", []wfJob{{Name: "j", Link: "/jobs/0a1b2c/1", Times: jobTimes{Start: start, End: end, Known: true}}}, start)
		if w.Span <= 0 {
			t.Errorf("axis of %v: span %v", d, w.Span)
		}
		for _, r := range w.Rows {
			for _, b := range r.Bars {
				for _, v := range []string{b.L, b.W} {
					if f, err := strconv.ParseFloat(v, 64); err != nil || f < 0 || f > 100 {
						t.Errorf("axis of %v: row %s bar at %q", d, r.ID, v)
					}
				}
			}
		}
	}
}

// TestLayoutProjectLinksUseSegment: the project switcher builds its links
// through instSeg.
func TestLayoutProjectLinksUseSegment(t *testing.T) {
	b, err := files.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `href="/projects/{{instSeg .Instance}}/`) || strings.Contains(string(b), `/projects/{{.Instance}}`) {
		t.Error("layout.html links a project by its raw instance")
	}
}
