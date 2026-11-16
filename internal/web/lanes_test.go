package web

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// parallelJobs is n jobs of a stage that ran side by side from at, each
// for d; failed of them failed.
func parallelJobs(first uint64, stage string, n, failed int, at time.Time, d time.Duration) []store.JobInfo {
	var out []store.JobInfo
	for i := 0; i < n; i++ {
		j := store.JobInfo{Instance: "0a1b2c", ID: first + uint64(i), PipelineID: 7, Name: fmt.Sprintf("%s-%02d", stage, i+1), Stage: stage,
			Status: "success", Started: at.Add(time.Duration(i) * time.Second), Finished: at.Add(time.Duration(i)*time.Second + d)}
		if i < failed {
			j.Status = "failed"
		}
		out = append(out, j)
	}
	return out
}

// A stage whose jobs ran more than laneRows side by side is one bar with
// its jobs counted, not bars drawn over each other; up to laneRows side
// by side each job keeps its bar.
func TestLaneAggregatesBeyondThreeRows(t *testing.T) {
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return at }}
	t0 := at.Add(-time.Hour)
	var jobs []store.JobInfo
	jobs = append(jobs, parallelJobs(100, "build", 3, 0, t0, 2*time.Minute)...)
	jobs = append(jobs, parallelJobs(200, "test", 12, 1, t0.Add(5*time.Minute), 3*time.Minute)...)
	jobs = append(jobs, parallelJobs(300, "e2e", 4, 0, t0.Add(10*time.Minute), time.Minute)...)
	// The images of e2e's jobs: two rebuilt, one from the cache; its last
	// job built none and failed.
	jobs[len(jobs)-1].Status = "failed"
	builds := []store.BuildRow{
		{ID: 1, Job: 300, Started: t0.Add(10 * time.Minute), Completed: t0.Add(10*time.Minute + 30*time.Second), Reason: history.FilesChanged, Built: 3},
		{ID: 2, Job: 301, Started: t0.Add(10 * time.Minute), Completed: t0.Add(10*time.Minute + 30*time.Second), Reason: history.FilesChanged, Built: 3},
		{ID: 3, Job: 302, Started: t0.Add(10 * time.Minute), Completed: t0.Add(10*time.Minute + 30*time.Second)},
	}
	c := s.pipeCard(store.PipeRow{Instance: "0a1b2c", ID: 7, Status: "failed", Created: t0}, jobs, builds, time.UTC)
	lanes := map[string]Lane{}
	for _, l := range c.Lanes {
		lanes[l.Stage] = l
	}
	if l := lanes["build"]; l.Agg != nil || l.Rows != 3 || len(l.Jobs) != 3 {
		t.Errorf("three side by side: %d rows, %d bars, aggregated %v", l.Rows, len(l.Jobs), l.Agg != nil)
	}
	l := lanes["test"]
	if l.Agg == nil || len(l.Jobs) != 0 || l.Rows != 1 {
		t.Fatalf("twelve side by side: %d rows, %d bars, aggregated %v", l.Rows, len(l.Jobs), l.Agg != nil)
	}
	a := l.Agg
	if a.Text != "12 jobs" || a.Failed != 1 || a.URL != c.URL {
		t.Errorf("twelve side by side: %q, %d failed, opens %q", a.Text, a.Failed, a.URL)
	}
	// From the first start (5:00 on the axis) to the last end (8:11).
	if !strings.HasPrefix(a.Title, "test: 12 jobs, 1 failed, from 5:00 to 8:11.") {
		t.Errorf("its title: %q", a.Title)
	}
	if a.L != pctText(5*time.Minute, 11*time.Minute+3*time.Second) {
		t.Errorf("its bar starts at %s%%", a.L)
	}
	e := lanes["e2e"].Agg
	if e == nil || e.Text != "4 jobs · 2 rebuilt, 1 from cache" || e.Failed != 1 {
		t.Fatalf("four side by side with images: %+v", e)
	}
	if !strings.Contains(e.Title, "4 jobs, 2 rebuilt, 1 from cache, 1 failed") {
		t.Errorf("its title: %q", e.Title)
	}
	// Its words do not fit after the bar at the right edge: before it.
	if e.LabelR == "" || e.LabelL != "" {
		t.Errorf("the words of a bar at the right edge: left %q, right %q", e.LabelL, e.LabelR)
	}
}

func pctText(d, span time.Duration) string { return fmt.Sprintf("%.2f", pctOf(d, span)) }

// The card's markup: the aggregated stage is one link the height of the
// row, its count and "failed" in words with a mark; no job bar of it.
func TestLaneAggregateMarkup(t *testing.T) {
	ts, st, _ := console(t)
	f := newRunningFixture(t, st)
	const pid = 3_000_000_901
	t0 := testNow.Add(-30 * time.Minute)
	key := f.job(pid, store.Job{JobID: 9_910_001, Name: "unit-01", Stage: "test", StartedAt: t0})
	var jobs []store.APIJob
	for i := 0; i < 6; i++ {
		j := store.APIJob{ID: 9_910_001 + uint64(i), Name: fmt.Sprintf("unit-%02d", i+1), Stage: "test", Status: "success",
			Started: t0, Finished: t0.Add(4 * time.Minute)}
		if i == 2 {
			j.Status = "failed"
		}
		jobs = append(jobs, j)
	}
	f.gitlab(pid, "failed", jobs...)
	card := pipeCardOf(t, ts, f.inst, key)
	for _, want := range []string{`<div class="pg-lane agg">`, `<a class="pg-agg has-failed" href="/pipelines/` + key + `"`,
		`6 jobs · <svg class="i"`, `<b class="af">1 failed</b>`, `<span class="sr">test: 6 jobs, 1 failed, from `} {
		if !strings.Contains(card, want) {
			t.Errorf("the card lacks %q: %s", want, card)
		}
	}
	if strings.Contains(card, `<a class="j `) {
		t.Errorf("job bars drawn on an aggregated stage: %s", card)
	}
	if strings.Contains(card, "waiting for a runner: usually") {
		t.Error("the card repeats the runner-wait figures")
	}
}

// listBackend is a store of pipelines for pipeList, newest first.
type listBackend struct {
	Empty
	rows  []store.PipeRow
	reads int
}

func (b *listBackend) PipelineList(_ context.Context, q store.PipeQuery) ([]store.PipeRow, error) {
	b.reads++
	var out []store.PipeRow
	for _, r := range b.rows {
		if !q.Before.IsZero() && !(r.Created.Before(q.Before) || (r.Created.Equal(q.Before) && r.ID < q.BeforeID)) {
			continue
		}
		if len(out) == q.Limit {
			break
		}
		out = append(out, r)
	}
	return out, nil
}

func pipeRows(n int, failed func(i int) bool) []store.PipeRow {
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	var rows []store.PipeRow
	for i := 0; i < n; i++ {
		r := store.PipeRow{Instance: "0a1b2c", ID: uint64(10_000 - i), Created: at.Add(-time.Duration(i/2) * time.Minute), Status: "success"}
		if failed(i) {
			r.Status = "failed"
		}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].Created.After(rows[b].Created) })
	return rows
}

// Failed only: a page of failed pipelines, and the next page begins after
// the last one shown; with few failed among many, the read stops at
// failedScan and the next page begins after the last one read.
func TestPipeListFailedOnly(t *testing.T) {
	ctx := context.Background()
	b := &listBackend{rows: pipeRows(1500, func(i int) bool { return i%7 == 3 })}
	s := &Server{data: b}
	list, next, scanned, err := s.pipeList(ctx, store.PipeQuery{}, true)
	if err != nil || len(list) != pipePage || scanned != 0 {
		t.Fatalf("a page of failed: %d, scanned %d, %v", len(list), scanned, err)
	}
	for _, p := range list {
		if p.Status != "failed" {
			t.Fatalf("pipeline %d (%s) on a failed-only page", p.ID, p.Status)
		}
	}
	if next == nil || next.ID != list[pipePage-1].ID {
		t.Fatalf("the next page begins after %v, not the last shown %d", next, list[pipePage-1].ID)
	}
	// The next page: the failed ones after it, none twice.
	more, _, _, err := s.pipeList(ctx, store.PipeQuery{Before: next.Created, BeforeID: next.ID}, true)
	if err != nil || len(more) != pipePage || more[0].ID != b.rows[3+7*pipePage].ID {
		t.Fatalf("the second page starts with %v, want %d (%v)", more[0].ID, b.rows[3+7*pipePage].ID, err)
	}
	// One failed, far down: the read stops at failedScan.
	b = &listBackend{rows: pipeRows(1500, func(i int) bool { return i == 1300 })}
	s.data = b
	list, next, scanned, err = s.pipeList(ctx, store.PipeQuery{}, true)
	if err != nil || len(list) != 0 || scanned != failedScan || next == nil || next.ID != b.rows[failedScan-1].ID || b.reads != failedScan/failedChunk {
		t.Fatalf("one failed at 1300: %d found, scanned %d, next %v, %d reads, %v", len(list), scanned, next, b.reads, err)
	}
	list, next, _, _ = s.pipeList(ctx, store.PipeQuery{Before: b.rows[failedScan-1].Created, BeforeID: b.rows[failedScan-1].ID}, true)
	if len(list) != 1 || list[0].ID != b.rows[1300].ID || next != nil {
		t.Errorf("reading on: %d found, next %v", len(list), next)
	}
	// Without the filter: a page and one more says an older one exists.
	b = &listBackend{rows: pipeRows(25, func(int) bool { return false })}
	s.data = b
	if list, next, _, _ = s.pipeList(ctx, store.PipeQuery{}, false); len(list) != pipePage || next == nil || next.ID != list[pipePage-1].ID {
		t.Errorf("all pipelines: %d, next %v", len(list), next)
	}
}

// The pipelines page: "Failed only" is a GET filter the pages keep; the
// list shows only what GitLab says failed.
func TestPipelinesFailedOnlyPage(t *testing.T) {
	ts, st, _ := console(t)
	f := newRunningFixture(t, st)
	t0 := testNow.Add(-20 * time.Minute)
	ok := f.job(3_000_000_911, store.Job{JobID: 9_920_001, Name: "lint", Stage: "test", StartedAt: t0})
	f.gitlab(3_000_000_911, "success", store.APIJob{ID: 9_920_001, Name: "lint", Stage: "test", Status: "success", Started: t0, Finished: t0.Add(time.Minute)})
	bad := f.job(3_000_000_912, store.Job{JobID: 9_920_002, Name: "lint", Stage: "test", StartedAt: t0})
	f.gitlab(3_000_000_912, "failed", store.APIJob{ID: 9_920_002, Name: "lint", Stage: "test", Status: "failed", Started: t0, Finished: t0.Add(time.Minute)})
	base := "/pipelines?project=" + url.QueryEscape(f.inst+"/42")
	all := get(t, ts, base)
	if !strings.Contains(all.body, `href="/pipelines/`+ok+`"`) || !strings.Contains(all.body, `href="/pipelines/`+bad+`"`) {
		t.Fatal("the fixture's pipelines are not listed")
	}
	if !regexp.MustCompile(`<input type="checkbox" name="status" value="failed" id="pFailed" >`).MatchString(all.body) {
		t.Errorf("no unchecked Failed only box: %s", between(all.body, `<form class="filters"`, "</form>"))
	}
	p := get(t, ts, base+"&status=failed")
	p.safe(t)
	if strings.Contains(p.body, `href="/pipelines/`+ok+`"`) || !strings.Contains(p.body, `href="/pipelines/`+bad+`"`) {
		t.Error("Failed only lists a pipeline that passed, or not the one that failed")
	}
	for _, c := range strings.Split(p.body, `<article class="pcard">`)[1:] {
		if !strings.Contains(c, `ps-failed`) {
			t.Errorf("a card on the failed-only page: %.300s", c)
		}
	}
	if !strings.Contains(p.body, `id="pFailed" checked`) {
		t.Error("the Failed only box is not checked")
	}
	if np := get(t, ts, base+"&status=failed&q=no-such-pipeline-anywhere"); !strings.Contains(np.body, "among failed pipelines") || !strings.Contains(np.body, "Clear the filters") {
		t.Errorf("no match among failed: %s", between(np.body, `<div class="empty">`, "</div>"))
	}
	v := &PipelinesView{Failed: true, Branch: "main"}
	if l := pipeLink(v, url.Values{"before": {"1"}}); !strings.Contains(l, "status=failed") || !strings.Contains(l, "branch=main") {
		t.Errorf("the pages' links drop Failed only: %s", l)
	}
}

// The pipeline page: the jobs and their time in the subtitle, the
// runner-wait figures once, no "every job of the pipeline", and a legend
// of exactly the classes on the bars.
func TestPipelinePageDensity(t *testing.T) {
	ts, st, _ := console(t)
	keys, _ := pipelines(t, st)
	bar := regexp.MustCompile(`<i class="bar c-([a-z-]+)"`)
	sw := regexp.MustCompile(`<i class="sw c-([a-z-]+)"></i>`)
	checked := 0
	for _, k := range keys {
		p := get(t, ts, "/pipelines/"+k)
		if !strings.Contains(p.body, `id="tlT"`) {
			continue
		}
		checked++
		if strings.Contains(p.body, "every job of the pipeline") {
			t.Errorf("%s: the timeline's header repeats what it shows", k)
		}
		drawn := map[string]bool{}
		for _, m := range bar.FindAllStringSubmatch(p.body, -1) {
			if m[1] != "pipe" {
				drawn[m[1]] = true
			}
		}
		shown := map[string]bool{}
		for _, m := range sw.FindAllStringSubmatch(between(p.body, `<div class="legend">`, "</div>"), -1) {
			shown[m[1]] = true
		}
		for c := range drawn {
			if !shown[c] {
				t.Errorf("%s: bars of class %s, not in the legend", k, c)
			}
		}
		for c := range shown {
			if !drawn[c] {
				t.Errorf("%s: the legend explains %s, drawn nowhere", k, c)
			}
		}
		if n := strings.Count(p.body, "waiting for a runner: usually"); n > 1 {
			t.Errorf("%s: the runner-wait figures %d times", k, n)
		}
	}
	if checked == 0 {
		t.Fatal("no pipeline page with a timeline")
	}
	// Jobs and time move to the subtitle.
	f := newRunningFixture(t, st)
	t0 := testNow.Add(-20 * time.Minute)
	key := f.job(3_000_000_921, store.Job{JobID: 9_930_001, Name: "lint", Stage: "test", StartedAt: t0})
	f.gitlab(3_000_000_921, "success", store.APIJob{ID: 9_930_001, Name: "lint", Stage: "test", Status: "success", Started: t0, Finished: t0.Add(time.Minute)},
		store.APIJob{ID: 9_930_002, Name: "deploy", Stage: "deploy", Status: "manual"})
	p := get(t, ts, "/pipelines/"+key)
	if sub := between(p.body, `<div class="sub">`, "</div>"); !strings.Contains(sub, `<span class="pjobs"><b>2</b> jobs in <b>`) || !strings.Contains(sub, "did not run") {
		t.Errorf("the subtitle: %s", sub)
	}
}

// The legend: the classes present, in the legend's order; every class a
// timeline draws (but the pipeline's own bar) has a line in it.
func TestWaterfallLegend(t *testing.T) {
	w := &Waterfall{Rows: []WFRow{
		{Bars: []WFBar{{Class: "pipe"}}},
		{Bars: []WFBar{{Class: "job-failed"}, {Class: "queue"}}},
		{Hidden: true, Bars: []WFBar{{Class: "after"}, {Class: "queue"}}},
	}}
	var got []string
	for _, it := range w.Legend() {
		got = append(got, it.Label)
	}
	if strings.Join(got, "|") != "Waiting for a runner|After the build|Job failed" {
		t.Errorf("legend: %q", got)
	}
	if (*Waterfall)(nil).Legend() != nil {
		t.Error("a missing timeline has a legend")
	}
	known := map[string]bool{"pipe": true}
	for _, it := range wfLegend {
		known[it.Class] = true
	}
	for _, c := range []string{"queue", "runner", "podwait", "xfer", "image", "received", "built", "push", "after", "job", "job-failed", "job-running", "cached", "running", "failed", "canceled"} {
		if !known[c] {
			t.Errorf("class %s has no line in the legend", c)
		}
	}
	for st := range stateLabels {
		if !known[st] {
			t.Errorf("step state %s has no line in the legend", st)
		}
	}
}
