package web

import (
	"strings"
	"testing"
	"time"
)

// The critical path follows the stages back from the job that ended last:
// each job's gate is the job of an earlier stage that ended last before it
// could be queued; a job that started before the previous stage ended
// (needs:) ends the chain.
func TestCriticalPath(t *testing.T) {
	t0 := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	job := func(name, stage string, queued, start, end int) wfJob {
		return wfJob{Name: name, Stage: stage, Times: jobTimes{Queued: at(queued), Start: at(start), End: at(end), Known: true}}
	}
	jobs := []wfJob{
		job("lint", "check", 0, 1, 10),
		job("unit", "check", 0, 2, 20), // the check stage ends here
		job("build: [api]", "build", 20, 25, 40),
		job("build: [web]", "build", 20, 22, 30),
		job("package", "deploy", 40, 41, 50), // ended last
	}
	cp := criticalPath(jobs)
	if got := strings.Join(cp.Names, " > "); got != "unit > build: [api] > package" {
		t.Errorf("chain %q", got)
	}
	// waited: unit 2 + api 5 + package 1; ran: 18 + 15 + 9
	if cp.Waited != 8*time.Second || cp.Ran != 42*time.Second {
		t.Errorf("waited %s ran %s", cp.Waited, cp.Ran)
	}
	for _, j := range jobs {
		want := j.Name == "unit" || j.Name == "build: [api]" || j.Name == "package"
		if j.Crit != want {
			t.Errorf("%s on the path: %v", j.Name, j.Crit)
		}
	}

	// A job with needs: [] queued before the previous stage ended is not
	// gated by it: the chain stops there.
	early := []wfJob{
		job("lint", "check", 0, 1, 30),
		job("fast-build", "build", 0, 2, 45), // started while check still ran
	}
	cp = criticalPath(early)
	if got := strings.Join(cp.Names, " > "); got != "fast-build" {
		t.Errorf("needs chain %q", got)
	}

	// Without GitLab's wait times the queue time is the start.
	pod := []wfJob{
		{Name: "a", Stage: "s1", Times: jobTimes{Start: at(0), End: at(10)}},
		{Name: "b", Stage: "s2", Times: jobTimes{Start: at(11), End: at(20)}},
	}
	cp = criticalPath(pod)
	if got := strings.Join(cp.Names, " > "); got != "a > b" || cp.Waited != 0 || cp.Ran != 19*time.Second {
		t.Errorf("pod chain %q waited %s ran %s", got, cp.Waited, cp.Ran)
	}
}
