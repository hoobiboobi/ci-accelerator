package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// Pipeline is a pipeline as its job pods described it.
type Pipeline struct {
	Instance     string
	ID           uint64
	ProjectID    uint64
	ProjectPath  string
	Ref, SHA     string
	Source       string
	Created      time.Time
	CommitTitle  string
	CommitAuthor string
	MergeRequest uint64
	// Builds stored here, and the pipeline's time over its builds and
	// jobs (a pipeline whose jobs built no image has no builds).
	Builds   int
	Started  time.Time
	Finished time.Time
	Rebuilt  int // builds that executed a step other than resolving an image
	// From GitLab's API, when the console has read it.
	Status string
	WebURL string
	APIAt  time.Time
	// Activity is the latest a job of it started or ended, as the
	// console saw: after APIAt, GitLab's word is out of date (a manual
	// job was run, a job retried).
	Activity time.Time
}

// JobInfo is a CI job as its pod described it.
type JobInfo struct {
	Instance   string
	ID         uint64
	PipelineID uint64
	ProjectID  uint64
	Name       string
	Stage      string
	URL        string
	Node       string
	Started    time.Time
	// From GitLab's API, when the console has read it.
	Status   string
	Queued   time.Time // created
	Finished time.Time
	Duration time.Duration
	Waited   time.Duration // queued before a runner took it
	Runner   string
	// AllowFailure: its failure does not fail the pipeline.
	AllowFailure bool
	// StartedBy is who started it (GitLab username); for a manual job,
	// who ran it.
	StartedBy string
	// From its pod: when Kubernetes created it and started its build
	// container, in whole seconds.
	PodCreated, PodRunning time.Time
}

// BuildPage is everything the build page shows about one build.
type BuildPage struct {
	*Stored
	Job            *JobInfo
	Pipeline       *Pipeline
	DockerfileName string
	Dockerfile     string
	Saved          time.Duration
	ContextBytes   int64
	// Steps that keep output in a file still within retention.
	OutputKept bool
	// Printed is the steps that printed something, kept or not.
	Printed map[int]bool
	// Route is the build router's decision that sent the build's job to
	// this daemon; nil when no router decided (no router, or a job it did
	// not know).
	Route *Decision
	// Nodes are the build nodes of the build's daemon and of the daemon its
	// reason names (Reason.Old), by daemon name, as the daemons said when
	// the console last read them; a daemon gone since keeps its node.
	Nodes map[string]string
}

// StepRun is one build's run of a step identity, for its history.
type StepRun struct {
	BuildID  int64
	Idx      int // the step's index in that build (builds differ in their steps)
	Pipeline uint64
	SHA      string
	Daemon   string
	State    history.StepState
	Duration time.Duration
	Started  time.Time
}

// PipelineInfo returns a pipeline without its jobs and builds; nil when
// nothing of it is stored. Its times span its builds and jobs.
func (s *Store) PipelineInfo(ctx context.Context, instance string, id uint64) (*Pipeline, error) {
	inst, pid := nullInstance(instance), nullUint(id)
	p := &Pipeline{Instance: instance, ID: id}
	var (
		known                                    bool
		project, created, mr, apiAt              sql.NullInt64
		bProject, bStart, bEnd, jProject, jStart sql.NullInt64
		jEnd, jAct                               sql.NullInt64
		nBuilds, nJobs                           int
	)
	err := s.db.QueryRowContext(ctx, `SELECT project_id, ref, sha, source, created_at, commit_title, commit_author, mr_iid,
			status, web_url, api_at FROM pipelines WHERE instance = ? AND pipeline_id = ?`, inst, pid).
		Scan(&project, &p.Ref, &p.SHA, &p.Source, &created, &p.CommitTitle, &p.CommitAuthor, &mr, &p.Status, &p.WebURL, &apiAt)
	switch {
	case err == nil:
		known = true
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	// The indexes are named: the planner otherwise walks every build or
	// job of the instance for the project id.
	if err := s.db.QueryRowContext(ctx, `SELECT count(*), max(project_id), min(started_at), max(completed_at), COALESCE(SUM(built > 0), 0)
		FROM builds INDEXED BY builds_pipeline WHERE instance = ? AND pipeline_id = ?`, inst, pid).
		Scan(&nBuilds, &bProject, &bStart, &bEnd, &p.Rebuilt); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*), max(project_id), min(COALESCE(started_at, queued_at)), max(finished_at),
			max(max(COALESCE(started_at, 0), COALESCE(finished_at, 0)))
		FROM jobs INDEXED BY jobs_pipeline WHERE instance = ? AND pipeline_id = ?`, inst, pid).
		Scan(&nJobs, &jProject, &jStart, &jEnd, &jAct); err != nil {
		return nil, err
	}
	if !known && nBuilds == 0 && nJobs == 0 {
		return nil, nil
	}
	p.Builds = nBuilds
	switch {
	case project.Valid:
		p.ProjectID = uint64(project.Int64)
	case bProject.Valid:
		p.ProjectID = uint64(bProject.Int64)
	case jProject.Valid:
		p.ProjectID = uint64(jProject.Int64)
	}
	if p.ProjectID != 0 {
		if err := s.db.QueryRowContext(ctx, "SELECT path FROM projects WHERE instance = ? AND project_id = ?", inst, p.ProjectID).
			Scan(&p.ProjectPath); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	p.MergeRequest = uint64(mr.Int64)
	p.Created, p.APIAt = msTime(created), msTime(apiAt)
	first := func(a, b sql.NullInt64, less bool) sql.NullInt64 {
		switch {
		case !a.Valid:
			return b
		case !b.Valid:
			return a
		case (a.Int64 < b.Int64) == less:
			return a
		}
		return b
	}
	p.Started = msTime(first(bStart, jStart, true))
	p.Finished = msTime(first(bEnd, jEnd, false))
	if act := first(bEnd, jAct, false); act.Int64 > 0 {
		p.Activity = msTime(act)
	}
	return p, nil
}

func (s *Store) jobs(ctx context.Context, where string, args ...any) ([]JobInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT instance, job_id, pipeline_id, project_id, name, stage, url, node, started_at,
		status, queued_at, finished_at, duration_ms, queued_ms, runner, allow_failure, started_by, pod_created_at, pod_running_at FROM jobs `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobInfo
	for rows.Next() {
		var (
			j                                  JobInfo
			instance, pipeline, project        sql.NullInt64
			started                            sql.NullInt64
			queued, finished, duration, waited sql.NullInt64
			podCreated, podRunning             sql.NullInt64
		)
		if err := rows.Scan(&instance, &j.ID, &pipeline, &project, &j.Name, &j.Stage, &j.URL, &j.Node, &started,
			&j.Status, &queued, &finished, &duration, &waited, &j.Runner, &j.AllowFailure, &j.StartedBy, &podCreated, &podRunning); err != nil {
			return nil, err
		}
		j.Instance = fmt.Sprintf("%06x", instance.Int64)
		j.PipelineID, j.ProjectID, j.Started = uint64(pipeline.Int64), uint64(project.Int64), msTime(started)
		j.Queued, j.Finished = msTime(queued), msTime(finished)
		j.PodCreated, j.PodRunning = msTime(podCreated), msTime(podRunning)
		j.Duration, j.Waited = time.Duration(duration.Int64)*time.Millisecond, time.Duration(waited.Int64)*time.Millisecond
		// GitLab last said the job was not over (running, or manual or
		// pending before it ran), and its pod has ended since: how it
		// ended is not known until GitLab is read again.
		if j.Status != "" && !Finished(j.Status) && !j.Finished.IsZero() {
			j.Status = ""
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ErrNotFound is returned for a build that is not stored.
var ErrNotFound = errors.New("store: not found")

// BuildPage returns one build with its graph restored, its job, pipeline
// and Dockerfile.
func (s *Store) BuildPage(ctx context.Context, id int64) (*BuildPage, error) {
	st, err := s.Load(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil && st == nil {
		return nil, err
	}
	// A graph past retention leaves the page without comparisons, not
	// without the build.
	p := &BuildPage{Stored: st}
	var (
		dfName, dfText sql.NullString
		saved          int64
		outDay         sql.NullInt64
	)
	err = s.db.QueryRowContext(ctx, `SELECT d.name, d.text, b.saved_ms, b.context_bytes, b.output_day
		FROM builds b LEFT JOIN dockerfiles d ON d.id = b.dockerfile_id WHERE b.id = ?`, id).
		Scan(&dfName, &dfText, &saved, &p.ContextBytes, &outDay)
	if err != nil {
		return nil, err
	}
	p.DockerfileName, p.Dockerfile, p.Saved = dfName.String, dfText.String, time.Duration(saved)*time.Millisecond
	if outDay.Valid {
		_, err := s.output(ctx, int(outDay.Int64), false)
		p.OutputKept = err == nil
	}
	p.Printed = map[int]bool{}
	rows, err := s.db.QueryContext(ctx, "SELECT idx FROM steps WHERE build_id = ? AND output_len > 0", id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var i int
		if err := rows.Scan(&i); err != nil {
			rows.Close()
			return nil, err
		}
		p.Printed[i] = true
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	a := st.Build.Attribution
	if a.State == history.Attributed {
		jobs, err := s.jobs(ctx, "WHERE instance = ? AND job_id = ?", nullInstance(a.Instance), nullUint(a.Job))
		if err != nil {
			return nil, err
		}
		if len(jobs) > 0 {
			p.Job = &jobs[0]
		}
		if p.Pipeline, err = s.PipelineInfo(ctx, a.Instance, a.Pipeline); err != nil {
			return nil, err
		}
		routes, err := s.JobRoutes(ctx, a.Instance, a.Job)
		if err != nil {
			return nil, err
		}
		// The latest decision for this build's daemon made before the build
		// started (a job may connect, and build, several times).
		for i := range routes {
			d := routes[i]
			if d.Daemon == st.Build.Daemon && !d.At.After(st.Build.Started.Add(time.Second)) {
				p.Route = &d
			}
		}
	}
	p.Nodes = map[string]string{}
	for _, name := range []string{st.Build.Daemon, p.Reason.Old} {
		if name == "" {
			continue
		}
		var node string
		if err := s.db.QueryRowContext(ctx, "SELECT node FROM daemons WHERE name = ?", name).Scan(&node); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if node != "" {
			p.Nodes[name] = node
		}
	}
	return p, nil
}

// StepHistory returns the latest runs of a step's identity in the build's
// project (all builds when the project is not known), newest first.
func (s *Store) StepHistory(ctx context.Context, buildID int64, step int, limit int) ([]StepRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH me AS (
			SELECT s.identity, b.instance, b.project_id FROM steps s JOIN builds b ON b.id = s.build_id
			WHERE s.build_id = ? AND s.idx = ? AND s.identity IS NOT NULL)
		SELECT b.id, s.idx, COALESCE(b.pipeline_id, 0), COALESCE(p.sha, ''), d.name, s.state, s.dur_ms, b.started_at + s.start_ms
		FROM me JOIN steps s ON s.identity = me.identity
		JOIN builds b ON b.id = s.build_id JOIN daemons d ON d.id = b.daemon_id
		LEFT JOIN pipelines p ON p.instance = b.instance AND p.pipeline_id = b.pipeline_id
		WHERE me.project_id IS NULL OR (b.instance = me.instance AND b.project_id = me.project_id)
		ORDER BY b.started_at DESC LIMIT ?`, buildID, step, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StepRun
	for rows.Next() {
		var (
			r            StepRun
			state        int
			dur, started int64
		)
		if err := rows.Scan(&r.BuildID, &r.Idx, &r.Pipeline, &r.SHA, &r.Daemon, &state, &dur, &started); err != nil {
			return nil, err
		}
		r.State, r.Duration, r.Started = stateNames[state], time.Duration(dur)*time.Millisecond, time.UnixMilli(started).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

func msTime(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.UnixMilli(v.Int64).UTC()
}

// RefInfo says which build a record ref is, and whose.
type RefInfo struct {
	ID        int64
	Instance  string
	ProjectID uint64
	Pipeline  uint64
	Job       uint64
	JobName   string
	Target    string
}

// Refs looks record refs up (the builds that work was received from, or
// that settled a reason).
func (s *Store) Refs(ctx context.Context, refs []string) (map[string]RefInfo, error) {
	out := map[string]RefInfo{}
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if _, ok := out[ref]; ok {
			continue
		}
		var (
			r                                RefInfo
			instance, project, pipeline, job sql.NullInt64
			name                             sql.NullString
		)
		err := s.db.QueryRowContext(ctx, `SELECT b.id, b.instance, b.project_id, b.pipeline_id, b.job_id, j.name, b.target
			FROM builds b LEFT JOIN jobs j ON j.instance = b.instance AND j.job_id = b.job_id
			WHERE b.ref = ? LIMIT 1`, ref).Scan(&r.ID, &instance, &project, &pipeline, &job, &name, &r.Target)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if instance.Valid {
			r.Instance = fmt.Sprintf("%06x", instance.Int64)
		}
		r.ProjectID, r.Pipeline, r.Job, r.JobName = uint64(project.Int64), uint64(pipeline.Int64), uint64(job.Int64), name.String
		out[ref] = r
	}
	return out, nil
}
