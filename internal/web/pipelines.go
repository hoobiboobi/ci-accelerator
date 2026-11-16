package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// PipelinesView is a project's pipelines, each with its jobs on a time
// axis: when each waited for a runner and when it ran.
type PipelinesView struct {
	Shell
	Branch   string
	Mine     bool
	CanMine  bool
	Branches []Option
	// Q is the search; Period one of periods' values; From and To the
	// dates (UTC, YYYY-MM-DD) of a custom period, To included.
	Q, Period, From, To string
	Periods             []Option
	// BadDates: a custom period's dates could not be read; Backwards: its
	// end is before its start.
	BadDates, Backwards bool
	// Failed: only pipelines GitLab says failed (status=failed).
	Failed bool
	// Scanned: Failed only read this many pipelines for the page and
	// stopped before the oldest; Older reads on.
	Scanned int
	// Filtered: a search, a period or Failed narrows the list.
	Filtered bool
	Cards    []PipeCard
	Older    string
	Newest   string
	Newer    bool
	// NoAPI: the console cannot read GitLab, so how long jobs waited for a
	// runner is not known.
	NoAPI bool
}

// PipeCard is one pipeline of the list.
type PipeCard struct {
	URL, GitLabURL       string
	Title, Ref, SHA, Who string
	When                 Stamp
	State                PipeState
	Dur                  string
	// While the pipeline is in progress (Running; Dur is so far), jobs
	// that have not started wait (Waiting), or wait for a person
	// (Manual): they are not jobs that did not run.
	Jobs, NotRun, Waiting, Manual int
	Running                       bool
	Waited, Longest, LongJ        string // how long jobs waited for a runner: the median, the longest and its job
	WaitedLong                    int    // jobs that waited longer than longWait
	Images                        string
	Lanes                         []Lane
	Ticks                         []Tick
}

// PipeState is how a pipeline stands, in GitLab's words.
type PipeState struct{ Key, Label, Icon string }

var pipeStates = map[string]PipeState{
	"success":  {"passed", "Passed", "check"},
	"failed":   {"failed", "Failed", "x"},
	"running":  {"running", "Running", "clock"},
	"pending":  {"running", "Pending", "clock"},
	"canceled": {"canceled", "Canceled", "x"},
	"skipped":  {"canceled", "Skipped", "x"},
	"manual":   {"manual", "Manual", "job"},
}

// Lane is one stage of a pipeline: its jobs in up to laneRows rows, or,
// when more of them ran side by side, one bar for the stage (Agg).
type Lane struct {
	Stage string
	Rows  int
	Jobs  []LaneJob
	Agg   *LaneAgg
}

// LaneAgg is a stage whose jobs ran more than laneRows side by side: one
// bar from its first job's start to its last job's end, with its jobs
// counted in words. The whole row opens the pipeline, which lists them.
type LaneAgg struct {
	L, W string // the bar, percent of the axis
	// Text counts the jobs ("12 jobs · 4 rebuilt, 8 from cache"); Failed
	// is said after it, with a mark.
	Text   string
	Failed int
	// Title says it all: the stage, the counts, when on the axis.
	Title string
	// The words sit after the bar (LabelL, percent from the left) or
	// before it (LabelR, from the right), up to the row's edge; In: on
	// the bar, there being no room either side.
	LabelL, LabelR string
	In             bool
	URL            string
}

// LaneJob is one job on a lane: its wait for a runner (Q) and its run
// (L, W), percent of the axis; Row is its row in the lane.
type LaneJob struct {
	QL, QW, L, W string
	Row          int
	Class, Title string
	URL          string
}

// longWait is a wait for a runner worth counting.
const longWait = 10 * time.Second

// laneRows bounds the rows of a lane: a stage with more jobs side by side
// is one bar (LaneAgg), not bars drawn over each other.
const laneRows = 3

// pipePage is how many pipelines a page lists.
const pipePage = 20

// periods are the choices of the pipelines page's time filter.
var periods = []struct {
	Value, Label string
	Span         time.Duration
}{
	{"", "Any time", 0},
	{"24h", "Last 24 hours", 24 * time.Hour},
	{"7d", "Last 7 days", 7 * 24 * time.Hour},
	{"30d", "Last 30 days", 30 * 24 * time.Hour},
	{"custom", "Dates…", 0},
}

// pipeFilter reads the search and the period of a pipelines request into
// the view and the query.
func pipeFilter(q url.Values, now time.Time, v *PipelinesView, pq *store.PipeQuery) {
	v.Q = strings.TrimSpace(q.Get("q"))
	if q := []rune(v.Q); len(q) > 200 {
		v.Q = string(q[:200]) // whole letters
	}
	pq.Search = v.Q
	v.Period = q.Get("period")
	known := false
	for _, p := range periods {
		if p.Value == v.Period {
			known = true
			if p.Span > 0 {
				pq.Since = now.Add(-p.Span)
			}
		}
	}
	if !known {
		v.Period = ""
	}
	for _, p := range periods {
		v.Periods = append(v.Periods, Option{Value: p.Value, Label: p.Label, On: p.Value == v.Period})
	}
	if v.Period == "custom" {
		v.From, v.To = q.Get("from"), q.Get("to")
		if v.From != "" {
			if t, err := time.ParseInLocation(time.DateOnly, v.From, now.Location()); err == nil {
				pq.Since = t
			} else {
				v.BadDates = true
			}
		}
		if v.To != "" {
			if t, err := time.ParseInLocation(time.DateOnly, v.To, now.Location()); err == nil {
				pq.Until = t.AddDate(0, 0, 1) // the day included, as long as it is there
			} else {
				v.BadDates = true
			}
		}
	}
	v.Backwards = !pq.Since.IsZero() && !pq.Until.IsZero() && !pq.Until.After(pq.Since)
	v.Failed = q.Get("status") == "failed"
	v.Filtered = v.Q != "" || !pq.Since.IsZero() || !pq.Until.IsZero() || v.Failed
}

// failedChunk is how many pipelines Failed only reads at a time, and
// failedScan how many at most for one page: the store keeps a status per
// pipeline but no index on it, and a page is read in one request.
const (
	failedChunk = 200
	failedScan  = 1000
)

// pipeList reads a page of pipelines, newest first, and where the next
// page begins (nil: no older pipeline). With failed, only the pipelines
// GitLab says failed: read failedChunk at a time, up to failedScan; then
// the next page begins after the last one read (scanned says how many).
func (s *Server) pipeList(ctx context.Context, pq store.PipeQuery, failed bool) (list []store.PipeRow, next *store.PipeRow, scanned int, err error) {
	if !failed {
		pq.Limit = pipePage + 1 // one more than a page: whether an older one exists
		if list, err = s.data.PipelineList(ctx, pq); err != nil {
			return nil, nil, 0, err
		}
		if len(list) > pipePage {
			list = list[:pipePage]
			next = &list[pipePage-1]
		}
		return list, next, 0, nil
	}
	pq.Limit = failedChunk
	for {
		rows, err := s.data.PipelineList(ctx, pq)
		if err != nil {
			return nil, nil, 0, err
		}
		for i := range rows {
			if rows[i].Status != "failed" {
				continue
			}
			if len(list) == pipePage {
				return list, &list[pipePage-1], 0, nil
			}
			list = append(list, rows[i])
		}
		scanned += len(rows)
		if len(rows) < failedChunk {
			return list, nil, 0, nil
		}
		last := rows[len(rows)-1]
		if scanned >= failedScan {
			return list, &last, scanned, nil
		}
		pq.Before, pq.BeforeID = last.Created, last.ID
	}
}

// pipeLink is the pipelines page with the view's filters and extra.
func pipeLink(v *PipelinesView, extra url.Values) string {
	u := url.Values{}
	for k, val := range map[string]string{"branch": v.Branch, "q": v.Q, "period": v.Period, "from": v.From, "to": v.To} {
		if val != "" {
			u.Set(k, val)
		}
	}
	if v.Mine {
		u.Set("mine", "1")
	}
	if v.Failed {
		u.Set("status", "failed")
	}
	for k, vals := range extra {
		u[k] = vals
	}
	if len(u) == 0 {
		return "/pipelines"
	}
	return "/pipelines?" + u.Encode()
}

func (s *Server) pipelines(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sh, err := s.projectPage(r, "pipelines")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.Title = "Pipelines"
	q := r.URL.Query()
	v := &PipelinesView{Branch: q.Get("branch"), Mine: q.Get("mine") == "1", CanMine: sh.Viewer != nil}
	pq := store.PipeQuery{Project: sh.Project.ProjectKey, Ref: v.Branch}
	pipeFilter(q, inZone(s.now(), zone(r)), v, &pq)
	if sh.Project.ID == 0 {
		v.Shell = sh
		s.render(w, "pipelines", http.StatusOK, v)
		return
	}
	s.settleLater(r, sh.Project.ProjectKey)
	if b, _ := strconv.ParseInt(q.Get("before"), 10, 64); b > 0 {
		pq.Before, v.Newer = time.UnixMilli(b), true
		pq.BeforeID, _ = strconv.ParseUint(q.Get("bid"), 10, 64)
	}
	if v.Mine && sh.Viewer != nil {
		pq.Author = sh.Viewer.Name
		if pq.Author == "" {
			pq.Author = sh.Viewer.Username
		}
	}
	branches, err := s.data.Branches(ctx, sh.Project.ProjectKey, s.now().Add(-31*24*time.Hour))
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, b := range branches {
		v.Branches = append(v.Branches, Option{Value: b, Label: b, On: b == v.Branch})
	}
	list, next, scanned, err := s.pipeList(ctx, pq, v.Failed)
	if err != nil {
		s.fail(w, err)
		return
	}
	// GitLab says how long each job waited for a runner: read what is
	// missing for the pipelines on the page, together, briefly.
	api := s.api(r)
	v.NoAPI = api == nil
	if api != nil {
		var wg sync.WaitGroup
		for _, p := range list {
			if store.Finished(p.Status) && !p.APIAt.IsZero() {
				continue
			}
			// Waits only for what GitLab was never asked about; what it
			// said before is shown while it is read again.
			wait := time.Duration(0)
			if p.APIAt.IsZero() {
				wait = 1500 * time.Millisecond
			}
			wg.Add(1)
			go func(p store.PipeRow) {
				defer wg.Done()
				api.Ensure(ctx, p.Instance, sh.Project.ID, p.ID, p.Status, p.APIAt, wait)
			}(p)
		}
		wg.Wait()
		if list, next, scanned, err = s.pipeList(ctx, pq, v.Failed); err != nil {
			s.fail(w, err)
			return
		}
	}
	v.Scanned = scanned
	ids := make([]uint64, len(list))
	for i, p := range list {
		ids[i] = p.ID
	}
	jobs, err := s.data.JobsOf(ctx, sh.Project.Instance, ids)
	if err != nil {
		s.fail(w, err)
		return
	}
	builds, err := s.data.BuildsOf(ctx, sh.Project.ProjectKey, ids)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, p := range list {
		v.Cards = append(v.Cards, s.pipeCard(p, jobs[p.ID], builds[p.ID], zone(r)))
	}
	if next != nil {
		v.Older = pipeLink(v, url.Values{"before": {strconv.FormatInt(next.Created.UnixMilli(), 10)}, "bid": {strconv.FormatUint(next.ID, 10)}})
	}
	v.Newest = pipeLink(v, nil)
	v.Shell = sh
	s.render(w, "pipelines", http.StatusOK, v)
}

// pipeCard lays a pipeline's jobs out by stage on its time axis.
func (s *Server) pipeCard(p store.PipeRow, jobs []store.JobInfo, builds []store.BuildRow, loc *time.Location) PipeCard {
	c := PipeCard{URL: "/pipelines/" + pipeKey(p.Instance, p.ID), GitLabURL: webURL(p.WebURL),
		Title: p.Title, Ref: p.Ref, SHA: short(p.SHA, 8), When: clockStamp(p.Created, inZone(s.now(), loc))}
	if c.Title == "" {
		c.Title = fmt.Sprintf("Pipeline #%d", p.ID)
	}
	if name, _, ok := strings.Cut(p.Author, " <"); ok {
		c.Who = name
	} else {
		c.Who = p.Author
	}
	c.State, _ = pipeStates[p.Status]
	if c.State.Key == "" {
		c.State = PipeState{"unknown", "Result unknown", "info"}
	}
	byJob := map[uint64][]store.BuildRow{}
	for _, b := range builds {
		byJob[b.Job] = append(byJob[b.Job], b)
	}
	now := s.now()
	built := map[uint64]bool{}
	for id := range byJob {
		built[id] = true
	}
	js := jobStates(p.Status, jobs, built, now)
	c.Running = js.inProgress
	unknownEnd := 0
	// Images: how they came out.
	n := map[string]int{}
	for _, b := range builds {
		n[rowStatus(b).Key]++
	}
	if len(builds) > 0 {
		parts := []string{fmt.Sprintf("%d image%s", len(builds), plural(len(builds)))}
		if n["cached"] > 0 {
			parts = append(parts, fmt.Sprintf("%d from cache", n["cached"]))
		}
		if r := len(builds) - n["cached"] - n["running"]; r > 0 {
			parts = append(parts, fmt.Sprintf("%d rebuilt", r))
		}
		c.Images = strings.Join(parts, " · ")
	}
	type placed struct {
		j          store.JobInfo
		q, s, e    time.Time
		class, why string
		kind       string // what a stage's bar counts it as (laneKinds)
	}
	var all []placed
	var origin, end time.Time
	var waits []time.Duration
	var longest time.Duration
	for _, j := range jobs {
		// A job known only by its builds: their times.
		if bs := byJob[j.ID]; j.Started.IsZero() && len(bs) > 0 {
			j.Started = bs[len(bs)-1].Started
			for _, b := range bs {
				if b.Completed.IsZero() {
					j.Finished = time.Time{}
					break
				}
				if b.Completed.After(j.Finished) {
					j.Finished = b.Completed
				}
			}
		}
		if js.waiting[j.ID] {
			c.Waiting++
			continue
		}
		if j.Started.IsZero() {
			if c.Running && j.Status == "manual" {
				c.Manual++
			} else {
				c.NotRun++
			}
			continue
		}
		e := j.Finished
		if e.IsZero() {
			switch {
			case store.Finished(j.Status):
				c.NotRun++
				continue
			case !js.running[j.ID] && !runningBuild(byJob[j.ID]):
				// It started; its end is not known (its pod went unseen).
				unknownEnd++
				continue
			}
			e = now
		}
		q := j.Started
		if j.Waited > 0 {
			q = j.Started.Add(-j.Waited)
			waits = append(waits, j.Waited)
			if j.Waited > longWait {
				c.WaitedLong++
			}
			if j.Waited > longest {
				longest, c.LongJ = j.Waited, j.Name
			}
		}
		pl := placed{j: j, q: q, s: j.Started, e: e}
		if bs := byJob[j.ID]; len(bs) > 0 {
			// A job's newest build (builds come newest first) says how it
			// ended, as its page shows.
			st := rowStatus(bs[0])
			pl.class, pl.why = "st-"+st.Key, st.Label
			switch {
			case isCanceled(st):
				pl.kind = "canceled"
			case st.Key == "failed", st.Key == "running", st.Key == "cached":
				pl.kind = st.Key
			default:
				pl.kind = "rebuilt"
			}
			// Its image built, the job may still have failed after it, as
			// GitLab says.
			if j.Status == "failed" {
				pl.kind = "failed"
			}
			if pl.kind == "failed" && j.AllowFailure {
				pl.kind = "allowed"
			}
		} else {
			switch {
			case j.Status == "failed" && j.AllowFailure:
				pl.class, pl.why, pl.kind = "jw-allowed", "allowed to fail", "allowed"
			case j.Status == "failed":
				pl.class, pl.why, pl.kind = "jw-failed", "failed", "failed"
			case j.Status == "canceled":
				pl.class, pl.why, pl.kind = "jw-canceled", "canceled", "canceled"
			case j.Finished.IsZero():
				pl.class, pl.why, pl.kind = "jw-running", "running", "running"
			case j.Status == "":
				pl.class, pl.why = "jw-job", "ran"
			default:
				pl.class, pl.why = "jw-job", "passed"
			}
		}
		if origin.IsZero() || q.Before(origin) {
			origin = q
		}
		if e.After(end) {
			end = e
		}
		all = append(all, pl)
	}
	c.Jobs = len(all) + c.NotRun + c.Waiting + c.Manual + unknownEnd
	// While it runs the pipeline's time goes on, whether or not a job runs.
	if c.Running && len(all) > 0 && now.After(end) {
		end = now
	}
	if !p.Created.IsZero() && (origin.IsZero() || p.Created.Before(origin)) && origin.Sub(p.Created) < time.Hour {
		origin = p.Created
	}
	if len(all) == 0 {
		return c
	}
	span := end.Sub(origin)
	if span <= 0 {
		span = time.Second
	}
	c.Dur = duration(end.Sub(origin))
	if len(waits) > 0 {
		c.Waited, c.Longest = duration(median(waits)), duration(longest)
	}
	c.Ticks = ticksFor(span)
	pct := func(d time.Duration) string { return strconv.FormatFloat(pctOf(d, span), 'f', 2, 64) }
	wid := func(d time.Duration) string {
		return strconv.FormatFloat(max(0.35, pctOf(d, span)), 'f', 2, 64)
	}
	// Stages in the order they first started.
	stageAt := map[string]time.Time{}
	for _, pl := range all {
		st := pl.j.Stage
		if t, ok := stageAt[st]; !ok || pl.q.Before(t) {
			stageAt[st] = pl.q
		}
	}
	stages := make([]string, 0, len(stageAt))
	for st := range stageAt {
		stages = append(stages, st)
	}
	sort.Slice(stages, func(a, b int) bool { return stageAt[stages[a]].Before(stageAt[stages[b]]) })
	sort.SliceStable(all, func(a, b int) bool { return all[a].q.Before(all[b].q) })
	for _, st := range stages {
		lane := Lane{Stage: st}
		if lane.Stage == "" {
			lane.Stage = "jobs"
		}
		var mine []placed
		for _, pl := range all {
			if pl.j.Stage == st {
				mine = append(mine, pl)
			}
		}
		// Each job in the first row free when it began to wait (in that
		// order, as few rows as ever ran side by side).
		var rowEnd []time.Time
		rows := make([]int, len(mine))
		for k, pl := range mine {
			row := -1
			for i, t := range rowEnd {
				if !pl.q.Before(t) {
					row = i
					break
				}
			}
			if row < 0 {
				rowEnd = append(rowEnd, time.Time{})
				row = len(rowEnd) - 1
			}
			rowEnd[row], rows[k] = pl.e, row
		}
		if len(rowEnd) > laneRows {
			// More side by side than rows: bars would cover each other.
			agg := make([]aggJob, len(mine))
			for k, pl := range mine {
				agg[k] = aggJob{s: pl.s, e: pl.e, kind: pl.kind}
			}
			lane.Rows = 1
			lane.Agg = laneAgg(lane.Stage, agg, origin, span, c.URL)
			c.Lanes = append(c.Lanes, lane)
			continue
		}
		for k, pl := range mine {
			row := rows[k]
			title := pl.j.Name + " · " + pl.why
			if pl.j.Waited > 0 {
				title += " · waited " + duration(pl.j.Waited) + " for a runner"
			}
			title += " · ran " + duration(max(pl.e.Sub(pl.s), 0))
			lj := LaneJob{L: pct(pl.s.Sub(origin)), W: wid(pl.e.Sub(pl.s)), Row: row, Class: pl.class, Title: title}
			lj.W = fitWidth(lj.L, lj.W)
			if pl.s.After(pl.q) {
				lj.QL, lj.QW = pct(pl.q.Sub(origin)), wid(pl.s.Sub(pl.q))
				lj.QW = fitWidth(lj.QL, lj.QW)
			}
			if bs := byJob[pl.j.ID]; len(bs) > 0 {
				lj.URL = fmt.Sprintf("/builds/%d", bs[0].ID)
			} else {
				lj.URL = c.URL
			}
			lane.Jobs = append(lane.Jobs, lj)
		}
		lane.Rows = len(rowEnd)
		c.Lanes = append(c.Lanes, lane)
	}
	return c
}

// aggJob is a job of a stage drawn as one bar: when it ran and what it
// counts as (laneKinds).
type aggJob struct {
	s, e time.Time
	kind string
}

// laneKinds are what a stage's bar counts its jobs as, in the order it
// says them; "failed" comes last, with a mark. A job of no kind (it
// passed, or GitLab has not said) is counted in the jobs only.
var laneKinds = []struct{ kind, word string }{
	{"rebuilt", "rebuilt"}, {"cached", "from cache"}, {"running", "running"},
	{"canceled", "canceled"}, {"allowed", "allowed to fail"},
}

// laneAgg is one bar for a stage whose jobs ran more than laneRows side
// by side: from the first start to the last end, its jobs counted.
func laneAgg(stage string, jobs []aggJob, origin time.Time, span time.Duration, url string) *LaneAgg {
	a := &LaneAgg{URL: url}
	var from, to time.Time
	n := map[string]int{}
	for i, j := range jobs {
		if i == 0 || j.s.Before(from) {
			from = j.s
		}
		if i == 0 || j.e.After(to) {
			to = j.e
		}
		n[j.kind]++
	}
	if to.Before(from) {
		to = from
	}
	a.Failed = n["failed"]
	l := pctOf(from.Sub(origin), span)
	w := max(0.35, pctOf(to.Sub(from), span))
	a.L = strconv.FormatFloat(l, 'f', 2, 64)
	a.W = fitWidth(a.L, strconv.FormatFloat(w, 'f', 2, 64))
	w, _ = strconv.ParseFloat(a.W, 64)
	// Images first ("4 rebuilt, 8 from cache"), then how jobs stand.
	var imgs, rest []string
	for _, k := range laneKinds {
		if n[k.kind] == 0 {
			continue
		}
		part := fmt.Sprintf("%d %s", n[k.kind], k.word)
		if k.kind == "rebuilt" || k.kind == "cached" {
			imgs = append(imgs, part)
		} else {
			rest = append(rest, part)
		}
	}
	parts := []string{fmt.Sprintf("%d job%s", len(jobs), plural(len(jobs)))}
	if len(imgs) > 0 {
		parts = append(parts, strings.Join(imgs, ", "))
	}
	parts = append(parts, rest...)
	a.Text = strings.Join(parts, " · ")
	said := strings.Join(append(append([]string{fmt.Sprintf("%d job%s", len(jobs), plural(len(jobs)))}, imgs...), rest...), ", ")
	if a.Failed > 0 {
		said += fmt.Sprintf(", %d failed", a.Failed)
	}
	a.Title = fmt.Sprintf("%s: %s, from %s to %s. Open the pipeline for every job.", stage, said,
		clockLabel(max(from.Sub(origin), 0)), clockLabel(max(to.Sub(origin), 0)))
	// The words after the bar, else before it, else on it.
	const room = 30.0 // percent of the axis the words may take
	switch end := l + w; {
	case end <= 100-room:
		a.LabelL = strconv.FormatFloat(end+0.8, 'f', 2, 64)
	case l >= room:
		a.LabelR = strconv.FormatFloat(100-l+0.8, 'f', 2, 64)
	default:
		a.In = true
		a.LabelL = strconv.FormatFloat(l+0.8, 'f', 2, 64)
	}
	return a
}

// runningBuild: one of a job's stored builds has not completed.
func runningBuild(bs []store.BuildRow) bool {
	for _, b := range bs {
		if b.Completed.IsZero() {
			return true
		}
	}
	return false
}
