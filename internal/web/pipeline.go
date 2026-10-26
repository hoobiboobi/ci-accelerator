package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// PipelineView is a pipeline's image builds: what was rebuilt and why,
// when the images were ready. Its other jobs are GitLab's to show.
type PipelineView struct {
	Shell
	Key, Title string
	ID         uint64
	Ref, SHA   string
	Started    Stamp
	GitLabURL  string
	Sentence   string
	Ready      string // from the first build's start to the last build's end
	Rebuilt    []BuildItem
	Cached     []BuildItem
	Running    int
	WF         *Waterfall
	Card       PipeCard // its status and what its jobs waited, as the pipelines page says
	SetBy      string   // the job that ended last
	// The critical path: the jobs that set the pipeline's time, stage by stage.
	CritJobs   int
	CritNames  string
	CritWait   string
	CritRun    string
	OtherJobs  int
	OtherDrawn int // of them, on the timeline (GitLab or a pod said when they ran)
	NoBuilds   bool
}

func (s *Server) pipeline(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r, "pipeline")
		return
	}
	instance := r.PathValue("instance")
	ctx := r.Context()
	project, err := s.data.PipelineProject(ctx, instance, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if project == 0 || !s.may(r, instance, project) {
		s.notFound(w, r, "pipeline")
		return
	}
	key := store.PipelineKey(instance, id)
	sh, err := s.shell(r, "pipelines", key)
	if err != nil {
		s.fail(w, err)
		return
	}
	pinProject(&sh, instance, project)
	s.settleLater(r, store.ProjectKey{Instance: instance, ID: project})
	// GitLab says when each job waited and ran: read it, briefly, first.
	info, err := s.data.PipelineInfo(ctx, instance, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if info != nil && s.ensureAPI(r, info) {
		if info, err = s.data.PipelineInfo(ctx, instance, id); err != nil {
			s.fail(w, err)
			return
		}
	}
	rows, err := s.data.Builds(ctx, store.BuildQuery{Project: store.ProjectKey{Instance: instance, ID: project}, Pipeline: id})
	if err != nil {
		s.fail(w, err)
		return
	}
	st, err := s.recent(ctx, store.ProjectKey{Instance: instance, ID: project})
	if err != nil {
		s.fail(w, err)
		return
	}
	v := &PipelineView{Key: key, ID: id, Title: fmt.Sprintf("Pipeline #%d", id), NoBuilds: len(rows) == 0}
	jobs, err := s.data.PipelineJobs(ctx, instance, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	v.OtherJobs = len(jobs) - len(jobsWithBuilds(rows))
	if info != nil {
		v.Card = s.pipeCard(store.PipeRow{Instance: instance, ID: id, Status: info.Status, Created: info.Created, WebURL: info.WebURL}, jobs, rows, zone(r))
	}
	for _, j := range jobs {
		if v.GitLabURL == "" {
			v.GitLabURL = pipelineURL(j.URL, id)
		}
	}
	var t0, t1 time.Time
	for i := len(rows) - 1; i >= 0; i-- { // oldest first
		b := rows[i]
		if i == len(rows)-1 {
			v.Ref, v.SHA = b.Ref, short(b.SHA, 8)
			if b.Title != "" {
				v.Title = b.Title
			}
		}
		it := s.item(b, st.usual, zone(r))
		switch it.Status.Key {
		case "cached":
			v.Cached = append(v.Cached, it)
		case "running":
			v.Running++
			v.Rebuilt = append(v.Rebuilt, it)
		default:
			v.Rebuilt = append(v.Rebuilt, it)
		}
		if t0.IsZero() || b.Started.Before(t0) {
			t0 = b.Started
		}
		end := b.Completed
		if end.IsZero() {
			end = s.now()
		}
		if end.After(t1) {
			t1 = end
		}
	}
	if len(rows) > 0 {
		v.Started = clockStamp(t0, inZone(s.now(), zone(r)))
		v.Ready = duration(t1.Sub(t0))
		v.Sentence = pipelineSentence(v)
	}
	var cp CritPath
	if v.WF, v.SetBy, cp, v.OtherDrawn, err = s.pipelineWaterfall(ctx, v.Title, rows, jobs); err != nil {
		s.fail(w, err)
		return
	}
	if len(cp.Names) > 1 {
		v.CritJobs, v.CritNames = len(cp.Names), strings.Join(cp.Names, " → ")
		v.CritWait, v.CritRun = duration(cp.Waited), duration(cp.Ran)
	}
	sh.Title = v.Title
	v.Shell = sh
	s.render(w, "pipeline", http.StatusOK, v)
}

// pipelineWaterfall lays every job of a pipeline out on one time axis,
// in the order they started: a job with an image build opens into its
// stages and steps; any other job is one bar, as GitLab (or its pod) says.
func (s *Server) pipelineWaterfall(ctx context.Context, title string, rows []store.BuildRow, jobs []store.JobInfo) (*Waterfall, string, CritPath, int, error) {
	byID := map[uint64]store.JobInfo{}
	perJob := map[uint64]int{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	for _, b := range rows {
		perJob[b.Job]++
	}
	var wjobs []wfJob
	for i := len(rows) - 1; i >= 0; i-- {
		page, err := s.data.BuildPage(ctx, rows[i].ID)
		if err != nil {
			return nil, "", CritPath{}, 0, err
		}
		if page == nil {
			continue
		}
		d, err := s.explain(ctx, page)
		if err != nil {
			return nil, "", CritPath{}, 0, err
		}
		b := page.Build
		j := byID[rows[i].Job]
		name := j.Name
		if name == "" {
			name = orDefault(b.Target)
		}
		if perJob[rows[i].Job] > 1 {
			name += " · " + orDefault(b.Target)
		}
		end := b.Completed
		if end.IsZero() {
			end = s.now()
		}
		st := rowStatus(rows[i])
		wjobs = append(wjobs, wfJob{Name: name, Status: &st, Times: timesOf(j, b.Started, end), Build: b, BuildID: rows[i].ID,
			Views: d.Views, Link: fmt.Sprintf("/builds/%d", rows[i].ID), Stage: j.Stage})
	}
	with := jobsWithBuilds(rows)
	drawn := 0
	for _, j := range jobs {
		if with[j.ID] || j.Started.IsZero() {
			continue
		}
		end := j.Finished
		note := j.Status
		switch {
		case note == "success":
			note = "passed"
		case note == "failed" && j.AllowFailure:
			note = "allowed to fail"
		case note == "":
			note = "no image built"
		}
		if end.IsZero() {
			if store.Finished(j.Status) {
				continue
			}
			end, note = s.now(), "running"
		}
		jt := jobTimes{Start: j.Started, End: end, Total: end.Sub(j.Started), Known: true}
		jt.Queued = queuedAt(j, j.Started)
		wjobs = append(wjobs, wfJob{Name: j.Name, Times: jt, Note: note, Link: fmt.Sprintf("/jobs/%s/%d", j.Instance, j.ID), Stage: j.Stage})
		drawn++
	}
	if len(wjobs) == 0 {
		return nil, "", CritPath{}, 0, nil
	}
	sort.SliceStable(wjobs, func(a, b int) bool { return wjobs[a].Times.Start.Before(wjobs[b].Times.Start) })
	// The axis starts when the first job began to wait for a runner.
	start, crit := wjobs[0].Times.Start, 0
	for _, j := range wjobs {
		if q := j.Times.Queued; !q.IsZero() && q.Before(start) {
			start = q
		}
	}
	for i, j := range wjobs {
		if j.Times.End.After(wjobs[crit].Times.End) {
			crit = i
		}
	}
	cp := criticalPath(wjobs)
	return waterfall(title, wjobs, start, false), wjobs[crit].Name, cp, drawn, nil
}

func jobsWithBuilds(rows []store.BuildRow) map[uint64]bool {
	out := map[uint64]bool{}
	for _, b := range rows {
		if b.Job != 0 {
			out[b.Job] = true
		}
	}
	return out
}

// pipelineSentence says what a pipeline's builds came down to.
func pipelineSentence(v *PipelineView) string {
	n := len(v.Rebuilt) + len(v.Cached)
	if len(v.Rebuilt) == 0 {
		return fmt.Sprintf("All %d image%s came from the cache. They were ready %s after the first build started.", n, plural(n), v.Ready)
	}
	by := map[string]int{}
	for _, it := range v.Rebuilt {
		by[it.Status.Key]++
	}
	var parts []string
	for _, k := range []string{"change", "farm", "unclear", "failed", "running"} {
		if by[k] == 0 {
			continue
		}
		switch k {
		case "change":
			parts = append(parts, fmt.Sprintf("%d because of the change", by[k]))
		case "farm":
			parts = append(parts, fmt.Sprintf("%d because of the build farm", by[k]))
		case "unclear":
			parts = append(parts, fmt.Sprintf("%d unclear (files or cache)", by[k]))
		case "failed":
			parts = append(parts, fmt.Sprintf("%d failed", by[k]))
		case "running":
			parts = append(parts, fmt.Sprintf("%d still building", by[k]))
		}
	}
	if len(v.Cached) == 0 {
		return fmt.Sprintf("All %d image%s were rebuilt: %s. Ready %s after the first build started.", n, plural(n), strings.Join(parts, ", "), v.Ready)
	}
	return fmt.Sprintf("%d image%s: %d from the cache, %d rebuilt (%s). Ready %s after the first build started.",
		n, plural(n), len(v.Cached), len(v.Rebuilt), strings.Join(parts, ", "), v.Ready)
}

var jobPath = regexp.MustCompile(`/-/jobs/\d+$`)

// pipelineURL is a pipeline's page in GitLab, from one of its jobs'.
func pipelineURL(jobURL string, id uint64) string {
	u := webURL(jobURL)
	if u == "" || !jobPath.MatchString(u) {
		return ""
	}
	return jobPath.ReplaceAllString(u, fmt.Sprintf("/-/pipelines/%d", id))
}

// JobView is a job that built no image: the console has nothing to
// explain about it, and says so.
type JobView struct {
	Shell
	Name, Stage string
	GitLabURL   string
	PipeURL     string
	PipeLabel   string
	LogURL      string
	// What the console knows: from GitLab when it has read the job, else
	// from the job pod (Approx).
	Result       string
	Waited       string
	Ran          string
	Started      Stamp
	Finished     Stamp
	Runner       string
	Node         string
	StartedBy    string
	AllowFailure bool
	Approx       bool
}

// jobFacts fills the job page's facts from a stored job.
func jobFacts(v *JobView, j *store.JobInfo, now time.Time) {
	switch j.Status {
	case "":
		v.Result = "unknown: the console has not read this job from GitLab"
	case "success":
		v.Result = "passed"
	case "failed":
		v.Result = "failed"
		if j.AllowFailure {
			v.Result = "failed, allowed to fail"
		}
	default:
		v.Result = j.Status
	}
	v.Approx = j.Status == ""
	if j.Waited > 0 {
		v.Waited = duration(j.Waited)
	}
	switch {
	case j.Duration > 0:
		v.Ran = duration(j.Duration)
	case !j.Started.IsZero() && !j.Finished.IsZero():
		v.Ran = duration(j.Finished.Sub(j.Started))
	case !j.Started.IsZero() && !store.Finished(j.Status):
		v.Ran = "running for " + duration(now.Sub(j.Started))
	}
	if !j.Started.IsZero() {
		v.Started = when(j.Started, now.Location())
	}
	if !j.Finished.IsZero() {
		v.Finished = when(j.Finished, now.Location())
	}
	v.Runner, v.Node, v.StartedBy, v.AllowFailure = j.Runner, j.Node, j.StartedBy, j.AllowFailure
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r, "job")
		return
	}
	instance := r.PathValue("instance")
	ctx := r.Context()
	ids, err := s.data.JobBuilds(ctx, instance, id)
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
	j, err := s.data.Job(ctx, instance, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if j == nil || !s.may(r, instance, j.ProjectID) {
		s.notFound(w, r, "job")
		return
	}
	sh, err := s.shell(r, "builds", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	pinProject(&sh, instance, j.ProjectID)
	v := &JobView{Name: j.Name, Stage: j.Stage, GitLabURL: webURL(j.URL)}
	jobFacts(v, j, inZone(s.now(), zone(r)))
	if j.PipelineID != 0 {
		v.PipeURL, v.PipeLabel = "/pipelines/"+store.PipelineKey(instance, j.PipelineID), "#"+strconv.FormatUint(j.PipelineID, 10)
	}
	if api := s.api(r); api != nil && api.Knows(instance) && j.ProjectID != 0 {
		v.LogURL = fmt.Sprintf("/jobs/%s/%d/log", instance, id)
	}
	sh.Title = v.Name
	v.Shell = sh
	s.render(w, "job", http.StatusOK, v)
}

// jobLog answers the log box: a part of the job's log from GitLab (its
// last lines, the lines before a part, or the lines added since one),
// secrets masked, as plain text with the part's place in X-Log-* headers.
// GitLab is read only then.
func (s *Server) jobLog(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	instance := r.PathValue("instance")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	j, err := s.data.Job(r.Context(), instance, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	api := s.api(r)
	if j == nil || api == nil || !s.may(r, instance, j.ProjectID) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "The console has no GitLab token, or does not know this job.")
		return
	}
	select {
	case s.logs <- struct{}{}:
		defer func() { <-s.logs }()
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "The console is reading other job logs; try again in a moment.")
		return
	}
	q, ok := logQuery(r.URL.Query())
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "lines, before and after must be numbers.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	part, err := api.JobLogPart(ctx, instance, j.ProjectID, id, q)
	var rl *gitlab.RateLimited
	switch {
	case errors.As(err, &rl):
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, "GitLab asks the console to wait until %s.", stampOf(rl.Until, zone(r), "15:04:05"))
		return
	case err != nil:
		s.log.Warn("job log", "job", id, "err", err)
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "GitLab did not give the log. It may be gone, or the token may not read this project.")
		return
	}
	// Where the part sits in the log, for the next part the page asks for.
	w.Header().Set("X-Log-Start", strconv.FormatInt(part.Start, 10))
	w.Header().Set("X-Log-End", strconv.FormatInt(part.End, 10))
	w.Header().Set("X-Log-Size", strconv.FormatInt(part.Size, 10))
	if part.Gap {
		w.Header().Set("X-Log-Gap", "1")
	}
	fmt.Fprint(w, history.MaskSecrets(part.Text))
}

// logLines is how many lines of a job's log one part holds at most.
const logLines = 500

// logQuery reads what part of a job's log a page asks for: ?lines=N (at
// most logLines), ?before=<a part's start>, ?after=<a part's end>.
func logQuery(v url.Values) (gitlab.LogQuery, bool) {
	q := gitlab.LogQuery{Lines: logLines}
	num := func(k string) (int64, bool) {
		if v.Get(k) == "" {
			return 0, true
		}
		n, err := strconv.ParseInt(v.Get(k), 10, 64)
		return n, err == nil && n >= 0
	}
	n, ok1 := num("lines")
	b, ok2 := num("before")
	a, ok3 := num("after")
	if n > 0 && n < logLines {
		q.Lines = int(n)
	}
	q.Before = b
	if v.Get("after") != "" {
		q.After, q.Since = a, true
	}
	return q, ok1 && ok2 && ok3
}
