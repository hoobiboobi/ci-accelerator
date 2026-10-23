package web

import (
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
	Cards    []PipeCard
	Older    string
	Newer    bool
	// NoAPI: the console cannot read GitLab, so how long jobs waited for a
	// runner is not known.
	NoAPI bool
}

// PipeCard is one pipeline of the list.
type PipeCard struct {
	URL, GitLabURL         string
	Title, Ref, SHA, Who   string
	When                   string
	State                  PipeState
	Dur                    string
	Jobs, NotRun           int
	Waited, Longest, LongJ string // how long jobs waited for a runner: the median, the longest and its job
	WaitedLong             int    // jobs that waited longer than longWait
	Images                 string
	Lanes                  []Lane
	Ticks                  []Tick
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

// Lane is one stage of a pipeline: its jobs in up to laneRows rows.
type Lane struct {
	Stage string
	Rows  int
	Jobs  []LaneJob
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

// laneRows bounds the rows of a lane: parallel jobs beyond it share rows.
const laneRows = 3

// pipePage is how many pipelines a page lists.
const pipePage = 20

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
	if sh.Project.ID == 0 {
		v.Shell = sh
		s.render(w, "pipelines", http.StatusOK, v)
		return
	}
	s.settleLater(r, sh.Project.ProjectKey)
	pq := store.PipeQuery{Project: sh.Project.ProjectKey, Ref: v.Branch, Limit: pipePage}
	if b, _ := strconv.ParseInt(q.Get("before"), 10, 64); b > 0 {
		pq.Before, v.Newer = time.UnixMilli(b), true
	}
	if v.Mine && sh.Viewer != nil {
		pq.Author = sh.Viewer.Name
		if pq.Author == "" {
			pq.Author = sh.Viewer.Username
		}
	}
	branches, err := s.store.Branches(ctx, sh.Project.ProjectKey, s.now().Add(-31*24*time.Hour))
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, b := range branches {
		v.Branches = append(v.Branches, Option{Value: b, Label: b, On: b == v.Branch})
	}
	list, err := s.store.PipelineList(ctx, pq)
	if err != nil {
		s.fail(w, err)
		return
	}
	// GitLab says how long each job waited for a runner: read what is
	// missing for the pipelines on the page, together, briefly.
	api, cred := s.api(r)
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
				api.Ensure(cred, p.Instance, sh.Project.ID, p.ID, p.Status, p.APIAt, wait)
			}(p)
		}
		wg.Wait()
		if list, err = s.store.PipelineList(ctx, pq); err != nil {
			s.fail(w, err)
			return
		}
	}
	ids := make([]uint64, len(list))
	for i, p := range list {
		ids[i] = p.ID
	}
	jobs, err := s.store.JobsOf(ctx, sh.Project.Instance, ids)
	if err != nil {
		s.fail(w, err)
		return
	}
	builds, err := s.store.BuildsOf(ctx, sh.Project.ProjectKey, ids)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, p := range list {
		v.Cards = append(v.Cards, s.pipeCard(p, jobs[p.ID], builds[p.ID]))
	}
	if len(list) == pipePage {
		older := url.Values{}
		if v.Branch != "" {
			older.Set("branch", v.Branch)
		}
		if v.Mine {
			older.Set("mine", "1")
		}
		older.Set("before", strconv.FormatInt(list[len(list)-1].Created.UnixMilli(), 10))
		v.Older = "/pipelines?" + older.Encode()
	}
	v.Shell = sh
	s.render(w, "pipelines", http.StatusOK, v)
}

// pipeCard lays a pipeline's jobs out by stage on its time axis.
func (s *Server) pipeCard(p store.PipeRow, jobs []store.JobInfo, builds []store.BuildRow) PipeCard {
	c := PipeCard{URL: "/pipelines/" + store.PipelineKey(p.Instance, p.ID), GitLabURL: webURL(p.WebURL),
		Title: p.Title, Ref: p.Ref, SHA: short(p.SHA, 8), When: clock(p.Created, s.now())}
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
		if j.Started.IsZero() {
			c.NotRun++
			continue
		}
		e := j.Finished
		if e.IsZero() {
			if store.Finished(j.Status) {
				c.NotRun++
				continue
			}
			e = s.now()
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
			st := rowStatus(bs[len(bs)-1])
			pl.class, pl.why = "st-"+st.Key, st.Label
		} else {
			switch {
			case j.Status == "failed" && j.AllowFailure:
				pl.class, pl.why = "jw-allowed", "allowed to fail"
			case j.Status == "failed":
				pl.class, pl.why = "jw-failed", "failed"
			case j.Status == "canceled":
				pl.class, pl.why = "jw-canceled", "canceled"
			case j.Finished.IsZero():
				pl.class, pl.why = "jw-running", "running"
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
	c.Jobs = len(all) + c.NotRun
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
	pct := func(d time.Duration) string { return strconv.FormatFloat(100*float64(d)/float64(span), 'f', 2, 64) }
	wid := func(d time.Duration) string {
		return strconv.FormatFloat(max(0.35, 100*float64(d)/float64(span)), 'f', 2, 64)
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
		var rowEnd []time.Time
		for _, pl := range all {
			if pl.j.Stage != st {
				continue
			}
			row := -1
			for i, t := range rowEnd {
				if !pl.q.Before(t) {
					row = i
					break
				}
			}
			if row < 0 {
				if len(rowEnd) < laneRows {
					rowEnd = append(rowEnd, time.Time{})
					row = len(rowEnd) - 1
				} else {
					// Every row is taken: the one that frees first.
					row = 0
					for i, t := range rowEnd {
						if t.Before(rowEnd[row]) {
							row = i
						}
					}
				}
			}
			rowEnd[row] = pl.e
			title := pl.j.Name + " · " + pl.why
			if pl.j.Waited > 0 {
				title += " · waited " + duration(pl.j.Waited) + " for a runner"
			}
			title += " · ran " + duration(pl.e.Sub(pl.s))
			lj := LaneJob{L: pct(pl.s.Sub(origin)), W: wid(pl.e.Sub(pl.s)), Row: row, Class: pl.class, Title: title}
			if pl.s.After(pl.q) {
				lj.QL, lj.QW = pct(pl.q.Sub(origin)), wid(pl.s.Sub(pl.q))
			}
			if bs := byJob[pl.j.ID]; len(bs) > 0 {
				lj.URL = fmt.Sprintf("/builds/%d", bs[len(bs)-1].ID)
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
