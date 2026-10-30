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
	// The pipeline is not over: jobs without a stored build may still
	// build. RunningJobs started and have not ended (GitLab or the pod
	// says; a job with a stored build too), WaitingJobs have not started;
	// neither counts in OtherJobs.
	InProgress  bool
	RunningJobs int
	WaitingJobs int
	// StatusUnknown: the console has not read the pipeline from GitLab;
	// with no job running or waiting it may be between two stages.
	StatusUnknown bool
}

// jobDone: GitLab or the job's pod says the job ended.
func jobDone(j store.JobInfo) bool { return !j.Finished.IsZero() || store.Finished(j.Status) }

// podTrust bounds how long the console takes a job pod's word that its
// job runs or waits: a pod deleted while the controller was down never
// says that its job ended.
const podTrust = 24 * time.Hour

// podStart is when the job's pod started its build container. A pod seen
// before the console kept that (no creation time either) has only the
// job's start its annotations carry.
func podStart(j store.JobInfo) time.Time {
	if j.PodRunning.IsZero() && j.PodCreated.IsZero() {
		return j.Started
	}
	return j.PodRunning
}

// jobRunning: GitLab says the job runs; without GitLab's word, or when
// its word is from before the job started (it waited when GitLab was read),
// its pod started its build container less than podTrust ago and has not
// ended. A pod that is there but has not started is a job still starting
// (jobWaiting).
func jobRunning(j store.JobInfo, now time.Time) bool {
	switch {
	case jobDone(j):
		return false
	case j.Status == "running":
		return true
	case j.Status != "" && !beforeStart(j.Status):
		return false
	}
	t := podStart(j)
	return !t.IsZero() && now.Sub(t) < podTrust
}

// beforeStart: GitLab's word for a job that has not started yet.
func beforeStart(status string) bool {
	switch status {
	case "created", "pending", "preparing", "waiting_for_resource", "scheduled":
		return true
	}
	return false
}

// jobWaiting: GitLab says the job waits to start (not a manual job: that
// one waits for a person and may never run); without GitLab's word, its
// pod was created less than podTrust ago and has not started.
func jobWaiting(j store.JobInfo, now time.Time) bool {
	if jobDone(j) || jobRunning(j, now) {
		return false
	}
	switch {
	case beforeStart(j.Status):
		return true
	case j.Status == "":
		return !j.PodCreated.IsZero() && j.PodRunning.IsZero() && now.Sub(j.PodCreated) < podTrust
	}
	return false
}

// gitlabRunning: GitLab says the pipeline is not over ("manual" is a
// pipeline that waits for a person and may stay so).
func gitlabRunning(status string) bool {
	return status != "" && !store.Finished(status) && status != "manual"
}

// pipeJobs is how a pipeline's jobs stand: those running and those
// waiting to start, and whether the pipeline is in progress (GitLab says
// so, or a job of it runs or waits). In a pipeline in progress, a job
// that has not started and is not over (nor manual) waits too. When
// GitLab says the pipeline is over, nothing runs or waits. built are the
// jobs with a stored build: they started, whatever else is known.
type pipeJobs struct {
	running, waiting map[uint64]bool
	inProgress       bool
}

func jobStates(status string, jobs []store.JobInfo, built map[uint64]bool, now time.Time) pipeJobs {
	p := pipeJobs{running: map[uint64]bool{}, waiting: map[uint64]bool{}}
	if store.Finished(status) {
		return p
	}
	for _, j := range jobs {
		switch {
		case jobRunning(j, now):
			p.running[j.ID] = true
		case built[j.ID]:
			// It started: a build of it is stored, whatever GitLab said
			// when it was last read.
		case status == "manual" && j.Status == "created":
			// Behind a manual job a person has not run: it may never start.
		case jobWaiting(j, now):
			p.waiting[j.ID] = true
		}
	}
	p.inProgress = gitlabRunning(status) || len(p.running) > 0 || len(p.waiting) > 0
	if p.inProgress {
		for _, j := range jobs {
			if j.Started.IsZero() && !built[j.ID] && !p.running[j.ID] && !jobDone(j) && j.Status != "manual" {
				p.waiting[j.ID] = true
			}
		}
	}
	return p
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
	with := jobsWithBuilds(rows)
	status := ""
	if info != nil {
		status = info.Status
	}
	v.StatusUnknown = status == ""
	js := jobStates(status, jobs, with, s.now())
	for _, j := range jobs {
		switch {
		case js.running[j.ID]:
			v.RunningJobs++
		case js.waiting[j.ID]:
			v.WaitingJobs++
		case !with[j.ID]:
			v.OtherJobs++
		}
	}
	for _, b := range rows {
		if b.Completed.IsZero() {
			v.Running++
		}
	}
	v.InProgress = js.inProgress
	v.NoBuilds = len(rows) == 0 && !v.InProgress
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
	var created time.Time
	if info != nil {
		created = info.Created
	}
	if v.WF, v.SetBy, cp, v.OtherDrawn, err = s.pipelineWaterfall(ctx, v.Title, created, rows, jobs, v.InProgress); err != nil {
		s.fail(w, err)
		return
	}
	if len(cp.Names) > 1 {
		v.CritJobs, v.CritNames = len(cp.Names), strings.Join(cp.Names, " → ")
		v.CritWait, v.CritRun = duration(cp.Waited), duration(cp.Ran)
	}
	sh.Title = v.Title
	sh.Preview = fmt.Sprintf("Pipeline #%d", v.ID)
	if v.Title != "" && v.Title != sh.Preview {
		sh.Preview += ": " + v.Title
	}
	sh.Desc = v.Sentence
	v.Shell = sh
	s.render(w, "pipeline", http.StatusOK, v)
}

// maxLiveJobs bounds the running builds a pipeline's timeline follows.
const maxLiveJobs = 8

// pipelineWaterfall lays every job of a pipeline out on one time axis,
// in the order they started: a job with an image build opens into its
// stages and steps; any other job is one bar, as GitLab (or its pod) says.
// While the pipeline runs no job "ended last" and there is no critical
// path yet (inProgress).
func (s *Server) pipelineWaterfall(ctx context.Context, title string, created time.Time, rows []store.BuildRow, jobs []store.JobInfo, inProgress bool) (*Waterfall, string, CritPath, int, error) {
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
			name = buildName(b.Target, b.Images)
		}
		if perJob[rows[i].Job] > 1 {
			name += " · " + buildName(b.Target, b.Images)
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
	drawn, live := 0, 0
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
		var chip *Status
		if end.IsZero() {
			// Not over and not running: waiting (counted as such), or
			// its end is not known (a pod gone unseen): no bar.
			if !jobRunning(j, s.now()) {
				continue
			}
			end, note = s.now(), "running"
			st := statuses["running"]
			chip = &st
		}
		jt := jobTimes{Start: j.Started, End: end, Total: end.Sub(j.Started), Known: true}
		jt.Queued = queuedAt(j, j.Started)
		// A running job whose build runs: its stages and steps so far.
		if chip != nil && live < maxLiveJobs {
			if lbs := s.liveBuildsOf(ctx, j.Instance, j.PipelineID, j.ID); len(lbs) > 0 && len(lbs[0].Steps) > 0 && !lbs[0].Started.IsZero() {
				live++
				b, views := liveBuild(lbs[0], s.now())
				wjobs = append(wjobs, wfJob{Name: j.Name, Status: chip, Times: jt, Build: b, Views: views, Live: true,
					Link: fmt.Sprintf("/jobs/%s/%d", j.Instance, j.ID), Stage: j.Stage})
				continue
			}
		}
		wjobs = append(wjobs, wfJob{Name: j.Name, Status: chip, Times: jt, Note: note, Link: fmt.Sprintf("/jobs/%s/%d", j.Instance, j.ID), Stage: j.Stage})
		if chip == nil { // the grey bars of the jobs that ended
			drawn++
		}
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
	// From when GitLab created the pipeline, as its card counts.
	if !created.IsZero() && created.Before(start) && start.Sub(created) < time.Hour {
		start = created
	}
	for i, j := range wjobs {
		if j.Times.End.After(wjobs[crit].Times.End) {
			crit = i
		}
	}
	if inProgress {
		wf := waterfall(title, wjobs, start)
		wf.Unfinished = true
		return wf, "", CritPath{}, drawn, nil
	}
	cp := criticalPath(wjobs)
	return waterfall(title, wjobs, start), wjobs[crit].Name, cp, drawn, nil
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

// oneImageSentence is pipelineSentence for a pipeline that built one image
// ("All 1 image were rebuilt" said 0.3.1).
func oneImageSentence(v *PipelineView) string {
	if len(v.Rebuilt) == 0 {
		return fmt.Sprintf("The image came from the cache. It was ready %s after its build started.", v.Ready)
	}
	switch v.Rebuilt[0].Status.Key {
	case "change":
		return fmt.Sprintf("The image was rebuilt because of the change. It was ready %s after its build started.", v.Ready)
	case "farm":
		return fmt.Sprintf("The image was rebuilt because of the build farm. It was ready %s after its build started.", v.Ready)
	case "unclear":
		return fmt.Sprintf("The image was rebuilt; files or cache, unclear. It was ready %s after its build started.", v.Ready)
	case "failed":
		return "The image's build failed."
	}
	return fmt.Sprintf("The image was rebuilt. It was ready %s after its build started.", v.Ready)
}

// pipelineSentence says what a pipeline's builds came down to.
func pipelineSentence(v *PipelineView) string {
	n := len(v.Rebuilt) + len(v.Cached)
	if len(v.Rebuilt) == 0 && v.InProgress {
		return fmt.Sprintf("%d image%s so far, all from the cache. The pipeline is still running; builds show up here as BuildKit completes them.", n, plural(n))
	}
	if n == 1 && !v.InProgress {
		return oneImageSentence(v)
	}
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
	if v.InProgress {
		return fmt.Sprintf("%d image%s so far: %d from the cache, %d rebuilt (%s). The pipeline is still running; builds show up here as BuildKit completes them.",
			n, plural(n), len(v.Cached), len(v.Rebuilt), strings.Join(parts, ", "))
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
	// Running: the job runs, Waiting: it waits to start; either way its
	// build may still come, and the page asks for itself again.
	Running, Waiting bool
	// LiveBuilds are the job's builds that are running now (Live is
	// the shell's: what the page hears of).
	LiveBuilds []*LiveView
}

// jobFacts fills the job page's facts from a stored job; pipeStatus is
// GitLab's word for its pipeline ("" unknown).
func jobFacts(v *JobView, j *store.JobInfo, pipeStatus string, now time.Time) {
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
	case !j.Started.IsZero() && jobRunning(*j, now):
		v.Ran = "running for " + duration(now.Sub(j.Started))
	}
	if !j.Started.IsZero() {
		v.Started = when(j.Started, now.Location())
	}
	if !j.Finished.IsZero() {
		v.Finished = when(j.Finished, now.Location())
	}
	v.Runner, v.Node, v.StartedBy, v.AllowFailure = j.Runner, j.Node, j.StartedBy, j.AllowFailure
	v.Running, v.Waiting = jobRunning(*j, now), jobWaiting(*j, now)
	if store.Finished(pipeStatus) || (pipeStatus == "manual" && j.Status == "created") {
		// Its pipeline is over, or the job is behind a manual job a person
		// may never run: it waits for nothing the page could show.
		v.Running, v.Waiting = v.Running && !store.Finished(pipeStatus), false
	}
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r, "job")
		return
	}
	instance := r.PathValue("instance")
	ctx := r.Context()
	j, err := s.data.Job(ctx, instance, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	// Someone who may not see the job learns nothing of it, not even
	// whether it built something.
	if j != nil && !s.may(r, instance, j.ProjectID) {
		s.notFound(w, r, "job")
		return
	}
	ids, err := s.data.JobBuilds(ctx, instance, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if j == nil && len(ids) > 1 {
		// Not known yet: any trace can name the job, with any pipeline.
		// Builds naming it with different pipelines leave open which are
		// its own: none is opened as the job's.
		var pipeline uint64
		for i, bid := range ids {
			p, err := s.data.BuildPage(ctx, bid)
			if err != nil {
				s.fail(w, err)
				return
			}
			if i > 0 && p.Build.Attribution.Pipeline != pipeline {
				ids = nil
				break
			}
			pipeline = p.Build.Attribution.Pipeline
		}
	}
	if len(ids) > 0 {
		// A job with several builds opens its last; the page lists the others.
		// The link in a job's log comes here: the page says so. (The build
		// page checks its own project.)
		http.Redirect(w, r, fmt.Sprintf("/builds/%d?via=log", ids[len(ids)-1]), http.StatusFound)
		return
	}
	if j == nil {
		s.notFound(w, r, "job")
		return
	}
	// Live on its pipeline: the job's build, once stored, turns this page
	// into the build's (the redirect above), as the job log's link
	// promises.
	live := ""
	if j.PipelineID != 0 {
		live = store.PipelineKey(instance, j.PipelineID)
	}
	sh, err := s.shell(r, "builds", live)
	if err != nil {
		s.fail(w, err)
		return
	}
	pinProject(&sh, instance, j.ProjectID)
	v := &JobView{Name: j.Name, Stage: j.Stage, GitLabURL: webURL(j.URL)}
	pipeStatus := ""
	if j.PipelineID != 0 {
		info, err := s.data.PipelineInfo(ctx, instance, j.PipelineID)
		if err != nil {
			s.fail(w, err)
			return
		}
		if info != nil {
			pipeStatus = info.Status
		}
	}
	jobFacts(v, j, pipeStatus, inZone(s.now(), zone(r)))
	if j.PipelineID != 0 {
		v.PipeURL, v.PipeLabel = "/pipelines/"+store.PipelineKey(instance, j.PipelineID), "#"+strconv.FormatUint(j.PipelineID, 10)
	}
	if api := s.api(r); api != nil && api.Knows(instance) && j.ProjectID != 0 {
		v.LogURL = fmt.Sprintf("/jobs/%s/%d/log", instance, id)
	}
	if v.Running && j.PipelineID != 0 {
		nodes := map[string]string{}
		for _, d := range s.data.Daemons() {
			nodes[d.Name] = d.Inspection.Node
		}
		for _, lb := range s.liveBuildsOf(ctx, instance, j.PipelineID, id) {
			v.LiveBuilds = append(v.LiveBuilds, s.liveView(ctx, lb, instance, j.ProjectID, j.Name, nodes[lb.Daemon], zone(r)))
		}
	}
	sh.Title = v.Name
	if v.PipeLabel != "" {
		sh.Preview = v.Name + " · pipeline " + v.PipeLabel
	}
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
		fmt.Fprint(w, "before and after must be numbers.")
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

// logLines is how many lines of a job's log one part holds at most. It is
// not the reader's to choose: secrets are masked per part, and a part of a
// few lines could sit inside a printed key, away from its BEGIN and END.
const logLines = 500

// logQuery reads what part of a job's log a page asks for: ?before=<a
// part's start>, ?after=<a part's end>.
func logQuery(v url.Values) (gitlab.LogQuery, bool) {
	q := gitlab.LogQuery{Lines: logLines}
	num := func(k string) (int64, bool) {
		if v.Get(k) == "" {
			return 0, true
		}
		n, err := strconv.ParseInt(v.Get(k), 10, 64)
		return n, err == nil && n >= 0
	}
	b, ok1 := num("before")
	a, ok2 := num("after")
	q.Before = b
	if v.Get("after") != "" {
		q.After, q.Since = a, true
	}
	return q, ok1 && ok2
}
