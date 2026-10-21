package web

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// PipelineView is the pipeline page.
type PipelineView struct {
	Shell
	P         *store.Pipeline
	Key       string
	Title     string
	NewerURL  string
	OlderURL  string
	GitLabURL string
	Time      time.Duration
	SetBy     string
	Rebuilt   int
	FromCache int
	Saved     time.Duration
	NodeN     int
	UnclearN  int
	Builds    int
	Gantt     []WFRow
	Ticks     []Tick
	Jobs      []PJob
	Other     []PJob // jobs without a build here
	WF        *Waterfall
	Lines     []PipelineLine
	Columns   []string
	Mixed     bool
	DfJobs    int
	Running   int
	// TimeElapsed: while jobs run, the seconds so far; the page counts on.
	TimeElapsed int
}

// PJob is a job in the pipeline's jobs table.
type PJob struct {
	Name, URL, Node, Time, Status string
	Cat                           Cat
	Why                           template.HTML
	Crit                          bool
	Retried                       bool // a later attempt of the job followed
	Elapsed                       int  // seconds a running job has run
}

// PipelineLine is a Dockerfile line with every job's result on it.
type PipelineLine struct {
	Line
	Chips   []Chip // one per column; State "" where the job did not run this line
	Started []string
}

// Chip is one job's result on a line.
type Chip struct {
	Col, State, Title, URL string
}

func (s *Server) pipeline(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r, "pipeline")
		return
	}
	instance := r.PathValue("instance")
	ctx := r.Context()
	p, _, builds, err := s.store.PipelinePage(ctx, instance, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if p == nil {
		s.notFound(w, r, "pipeline")
		return
	}
	if s.ensureAPI(p) {
		if p, _, builds, err = s.store.PipelinePage(ctx, instance, id); err != nil {
			s.fail(w, err)
			return
		}
	}
	key := store.PipelineKey(p.Instance, p.ID)
	sh, err := s.shell(r, "pipes", key)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, pr := range sh.Projects {
		if pr.Instance == p.Instance && pr.ID == p.ProjectID {
			sh.Project = pr
		}
	}
	lv, err := s.listFor(ctx, r, &sh, "p:"+key, r.URL.Path)
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.List, sh.Split, sh.Has = lv, true, "pipe"
	v := &PipelineView{P: p, Key: key, Title: p.CommitTitle, GitLabURL: p.WebURL}
	if v.Title == "" {
		v.Title = "Pipeline #" + strconv.FormatUint(p.ID, 10)
	}
	sh.Title = v.Title
	v.NewerURL, v.OlderURL = lv.pipeNeighbours("p:" + key)
	jobs, err := s.store.PipelineJobs(ctx, instance, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	retried := retriedOf(jobs, func(j store.JobInfo) (string, uint64) { return j.Name, j.ID })
	jobByID := map[uint64]store.JobInfo{}
	for _, j := range jobs {
		jobByID[j.ID] = j
	}
	nodeHost := map[string]string{}
	if s.opt.Live != nil {
		for _, dm := range s.opt.Live.Daemons() {
			nodeHost[dm.Name] = dm.Inspection.Node
		}
	}
	var wjobs []wfJob
	var start time.Time
	withBuild := map[uint64]bool{}
	cols := map[string]bool{}
	var dfBuilds []colBuild
	// A job that ran several builds gets one row per build, named by target.
	perJob := map[uint64]int{}
	for _, st := range builds {
		perJob[st.Build.Attribution.Job]++
	}
	for _, st := range builds {
		page, err := s.store.BuildPage(ctx, st.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		d, err := s.explain(ctx, page)
		if err != nil {
			s.fail(w, err)
			return
		}
		b := page.Build
		j := jobByID[b.Attribution.Job]
		withBuild[b.Attribution.Job] = true
		name := j.Name
		if name == "" {
			name = "job " + strconv.FormatUint(b.Attribution.Job, 10)
		}
		if perJob[b.Attribution.Job] > 1 {
			t := b.Target
			if t == "" {
				t = "default target"
			}
			name += " · " + t
		}
		jt := timesOf(j, b.Started, b.Completed)
		if start.IsZero() || jt.Start.Before(start) {
			start = jt.Start
		}
		wjobs = append(wjobs, wfJob{Name: name, Cat: d.Cat, Times: jt, Build: b, BuildID: st.ID, Views: d.Views,
			Link: fmt.Sprintf("/builds/%d", st.ID)})
		v.Builds++
		v.Saved += page.Saved
		for _, vw := range d.Views {
			switch vw.State {
			case "built":
				v.Rebuilt++
			case "cached":
				v.FromCache++
			}
		}
		switch d.Cat.Key {
		case "node":
			v.NodeN++
		case "unknown":
			v.UnclearN++
		}
		v.Jobs = append(v.Jobs, PJob{Name: name, URL: fmt.Sprintf("/builds/%d", st.ID), Node: shortDaemon(b.Daemon) + paren(nodeHost[b.Daemon]),
			Time: duration(jt.Total), Cat: d.Cat, Why: d.Verdict.Title, Retried: retried[b.Attribution.Job]})
		col := b.Target
		if col == "" || cols[col] {
			col = name
		}
		cols[col] = true
		dfBuilds = append(dfBuilds, colBuild{col: col, d: d})
	}
	running := map[uint64]bool{}
	if s.opt.Running != nil {
		for _, j := range s.opt.Running.Jobs() {
			// A pod outlives its job a little; GitLab's status wins.
			if j.Instance == p.Instance && j.PipelineID == p.ID && !store.Finished(jobByID[j.JobID].Status) {
				running[j.JobID] = true
				v.Running++
			}
		}
	}
	// GitLab may say a job runs before its pod shows up.
	for _, j := range jobs {
		if j.Status == "running" && !withBuild[j.ID] && !running[j.ID] {
			running[j.ID] = true
			v.Running++
		}
	}
	// The job that ended last sets the pipeline's time; while jobs still
	// run, none does yet.
	crit := -1
	var end time.Time
	for i, j := range wjobs {
		if j.Times.End.After(end) {
			end, crit = j.Times.End, i
		}
	}
	if v.Running > 0 {
		// Still running: the time so far, from the first job's start.
		first := start
		for _, j := range jobs {
			if running[j.ID] && !j.Started.IsZero() && (first.IsZero() || j.Started.Before(first)) {
				first = j.Started
			}
		}
		if !first.IsZero() {
			v.TimeElapsed = int(time.Since(first).Seconds())
			v.Time = time.Duration(v.TimeElapsed) * time.Second
		}
	} else if crit >= 0 {
		v.Time = end.Sub(start)
		wjobs[crit].Crit = true
		v.Jobs[crit].Crit = true
		v.SetBy = wjobs[crit].Name
	}
	for _, j := range jobs {
		if withBuild[j.ID] {
			continue
		}
		pj := PJob{Name: j.Name, URL: fmt.Sprintf("/jobs/%s/%d", j.Instance, j.ID), Status: j.Status, Cat: cats["nobuild"], Retried: retried[j.ID]}
		switch {
		case running[j.ID]:
			pj.Cat, pj.Why = cats["running"], "Running — its builds appear when they complete."
			if !j.Started.IsZero() {
				pj.Elapsed = int(time.Since(j.Started).Seconds())
				pj.Time = duration(time.Duration(pj.Elapsed) * time.Second)
			}
		case j.Status == "manual":
			pj.Why = "A manual job: it has not been started."
		case j.Status != "" && !store.Finished(j.Status):
			pj.Why = template.HTML("Waiting to start (" + template.HTMLEscapeString(strings.ReplaceAll(j.Status, "_", " ")) + ").")
		default:
			st := ""
			if j.Status != "" {
				st = " · " + template.HTMLEscapeString(j.Status)
			}
			pj.Why = template.HTML("No build on the daemons" + st + ": the job built nothing, or ran on another runner.")
		}
		if j.Duration > 0 && pj.Elapsed == 0 {
			pj.Time = duration(j.Duration)
		}
		v.Other = append(v.Other, pj)
	}
	if len(wjobs) > 0 {
		v.WF = waterfall(v.Title, wjobs, start, false)
		for _, row := range v.WF.Rows {
			if row.Kind == "job" {
				v.Gantt = append(v.Gantt, row)
			}
		}
		v.Ticks = v.WF.Ticks
	}
	v.addDockerfiles(dfBuilds)
	v.Shell = sh
	s.render(w, "pipeline", http.StatusOK, v)
}

// addDockerfiles lays out the Dockerfile with one column per job that used
// it: the first job's Dockerfile, and a note when the jobs used several.
// colBuild is a build with the name of its Dockerfile column.
type colBuild struct {
	col string
	d   *built
}

func (v *PipelineView) addDockerfiles(bs []colBuild) {
	text := ""
	for _, cb := range bs {
		if cb.d.Page.Dockerfile == "" {
			continue
		}
		if text == "" {
			text = cb.d.Page.Dockerfile
			for _, l := range dockerfile(text, cb.d.Views, cb.d.Page.Build) {
				l.Step, l.State, l.Opens, l.First, l.Dim, l.Showed = -1, "", false, false, false, ""
				v.Lines = append(v.Lines, PipelineLine{Line: l})
			}
		}
		if cb.d.Page.Dockerfile != text {
			v.Mixed = true
			continue
		}
		v.Columns = append(v.Columns, cb.col)
		v.DfJobs++
		for i := range v.Lines {
			v.Lines[i].Chips = append(v.Lines[i].Chips, Chip{})
		}
		c := len(v.Columns) - 1
		for _, l := range dockerfile(text, cb.d.Views, cb.d.Page.Build) {
			if l.Step < 0 || l.N-1 >= len(v.Lines) {
				continue
			}
			sv := cb.d.Views[l.Step]
			title := fmt.Sprintf("%s: %s, %s", cb.col, stateLabels[sv.State], duration(sv.D))
			if sv.From != "" {
				title = fmt.Sprintf("%s: from %s, waited %s", cb.col, sv.From, duration(sv.D))
			}
			chip := Chip{Col: cb.col, State: sv.State, Title: title, URL: fmt.Sprintf("/builds/%d/steps/%d", cb.d.Page.ID, l.Step)}
			if !l.Opens {
				chip.State = "cont"
			}
			v.Lines[l.N-1].Chips[c] = chip
			if l.First {
				v.Lines[l.N-1].Started = append(v.Lines[l.N-1].Started, cb.col)
			}
		}
	}
}

// JobView is the page of a job without a build here yet: running, or one
// that never built.
type JobView struct {
	Shell
	Name, Status, Node, Started, Commit, URL, PipeURL, PipeLabel string
	Running                                                      bool
	Active                                                       []string
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r, "job")
		return
	}
	instance := r.PathValue("instance")
	ctx := r.Context()
	ids, err := s.store.JobBuilds(ctx, instance, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(ids) > 0 {
		// A job with several builds opens its last; the page lists the others.
		// The link in a job's log comes here: the page says so.
		http.Redirect(w, r, fmt.Sprintf("/builds/%d?via=log", ids[len(ids)-1]), http.StatusFound)
		return
	}
	var job *store.Job
	if s.opt.Running != nil {
		for _, j := range s.opt.Running.Jobs() {
			if j.Instance == instance && j.JobID == id {
				j := j
				job = &j
			}
		}
	}
	v := &JobView{}
	if job != nil {
		v.Name, v.Running, v.Node, v.URL, v.Commit = job.Name, true, job.Node, job.URL, job.CommitTitle
		if !job.StartedAt.IsZero() {
			v.Started = ago(job.StartedAt) + " · running for " + duration(s.now().Sub(job.StartedAt).Truncate(time.Second))
		}
		if job.PipelineID != 0 {
			v.PipeURL, v.PipeLabel = "/pipelines/"+store.PipelineKey(instance, job.PipelineID), "#"+strconv.FormatUint(job.PipelineID, 10)
		}
	} else {
		found := false
		ps, err := s.store.Pipelines(ctx, 200)
		if err != nil {
			s.fail(w, err)
			return
		}
		for _, p := range ps {
			if p.Instance != instance {
				continue
			}
			jobs, err := s.store.PipelineJobs(ctx, instance, p.ID)
			if err != nil {
				s.fail(w, err)
				return
			}
			for _, j := range jobs {
				if j.ID == id {
					found = true
					v.Name, v.Status, v.Node, v.URL, v.Commit = j.Name, j.Status, j.Node, j.URL, p.CommitTitle
					v.PipeURL, v.PipeLabel = "/pipelines/"+store.PipelineKey(instance, p.ID), "#"+strconv.FormatUint(p.ID, 10)
					if !j.Started.IsZero() {
						v.Started = ago(j.Started)
					}
				}
			}
		}
		if !found {
			s.notFound(w, r, "job (its builds appear when they complete)")
			return
		}
	}
	live := ""
	if v.PipeURL != "" {
		live = strings.TrimPrefix(v.PipeURL, "/pipelines/")
	}
	sh, err := s.shell(r, "pipes", live)
	if err != nil {
		s.fail(w, err)
		return
	}
	// A running job shows its running builds.
	sh.LiveNodes = true
	if s.opt.Live != nil && v.Node != "" {
		for _, d := range s.opt.Live.Daemons() {
			if d.Inspection.Node != v.Node {
				continue
			}
			for _, a := range d.Active {
				v.Active = append(v.Active, fmt.Sprintf("%s: building %s for %s", shortDaemon(d.Name), orDefault(a.Target),
					duration(s.now().Sub(a.Started).Truncate(time.Second))))
			}
		}
	}
	lv, err := s.listFor(ctx, r, &sh, fmt.Sprintf("j:%s/%d", instance, id), r.URL.Path)
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.List, sh.Split, sh.Has, sh.Title = lv, true, "job", v.Name
	v.Shell = sh
	s.render(w, "job", http.StatusOK, v)
}

// sortedKeys returns a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var _ = history.Built
