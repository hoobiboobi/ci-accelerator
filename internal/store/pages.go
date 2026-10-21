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
	// Over the pipeline's builds stored here.
	Builds   int
	Started  time.Time
	Finished time.Time
	Rebuilt  int // builds that executed a step other than resolving an image
	// From GitLab's API, when the console has read it.
	Status string
	WebURL string
	APIAt  time.Time
}

// JobInfo is a CI job as its pod described it.
type JobInfo struct {
	Instance   string
	ID         uint64
	PipelineID uint64
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
}

// StepRun is one build's run of a step identity, for its history.
type StepRun struct {
	BuildID  int64
	Pipeline uint64
	SHA      string
	Daemon   string
	State    history.StepState
	Duration time.Duration
	Started  time.Time
}

// rebuilt counts a pipeline's builds that executed a step other than
// resolving an image.
const rebuilt = `SUM(EXISTS (SELECT 1 FROM steps s LEFT JOIN step_defs d ON d.identity = s.identity
	WHERE s.build_id = b.id AND s.state = 1 AND COALESCE(s.name, d.name) NOT LIKE 'FROM %'))`

// Pipelines returns the latest pipelines with builds here, newest first.
func (s *Store) Pipelines(ctx context.Context, limit int) ([]Pipeline, error) {
	return s.pipelines(ctx, "", limit)
}

// pipelines lists pipelines whose builds match where (conditions on b,
// starting with AND), newest first; the last argument is the limit.
func (s *Store) pipelines(ctx context.Context, where string, args ...any) ([]Pipeline, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.instance, b.pipeline_id, COALESCE(p.project_id, MAX(b.project_id)), COALESCE(pr.path, ''),
			COALESCE(p.ref, ''), COALESCE(p.sha, ''), COALESCE(p.source, ''), p.created_at,
			COALESCE(p.commit_title, ''), COALESCE(p.commit_author, ''), p.mr_iid,
			COALESCE(p.status, ''), COALESCE(p.web_url, ''), p.api_at,
			count(*), MIN(b.started_at), MAX(b.completed_at),
			`+rebuilt+`
		FROM builds b
		LEFT JOIN pipelines p ON p.instance = b.instance AND p.pipeline_id = b.pipeline_id
		LEFT JOIN projects pr ON pr.instance = b.instance AND pr.project_id = COALESCE(p.project_id, b.project_id)
		WHERE b.pipeline_id IS NOT NULL `+where+`
		GROUP BY b.instance, b.pipeline_id
		ORDER BY MIN(b.started_at) DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pipeline
	for rows.Next() {
		p, err := scanPipeline(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanPipeline(rows *sql.Rows) (Pipeline, error) {
	var (
		p                              Pipeline
		instance, project, created, mr sql.NullInt64
		started, finished, apiAt       sql.NullInt64
	)
	if err := rows.Scan(&instance, &p.ID, &project, &p.ProjectPath, &p.Ref, &p.SHA, &p.Source, &created,
		&p.CommitTitle, &p.CommitAuthor, &mr, &p.Status, &p.WebURL, &apiAt, &p.Builds, &started, &finished, &p.Rebuilt); err != nil {
		return p, err
	}
	p.Instance = fmt.Sprintf("%06x", instance.Int64)
	p.ProjectID, p.MergeRequest = uint64(project.Int64), uint64(mr.Int64)
	p.Created, p.Started, p.Finished, p.APIAt = msTime(created), msTime(started), msTime(finished), msTime(apiAt)
	return p, nil
}

// PipelinePage returns a pipeline, its jobs and its builds (with steps and
// reasons, graphs not restored); nil when nothing of it is stored.
func (s *Store) PipelinePage(ctx context.Context, instance string, id uint64) (*Pipeline, []JobInfo, []*Stored, error) {
	inst := nullInstance(instance)
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.instance, b.pipeline_id, COALESCE(p.project_id, MAX(b.project_id)), COALESCE(pr.path, ''),
			COALESCE(p.ref, ''), COALESCE(p.sha, ''), COALESCE(p.source, ''), p.created_at,
			COALESCE(p.commit_title, ''), COALESCE(p.commit_author, ''), p.mr_iid,
			COALESCE(p.status, ''), COALESCE(p.web_url, ''), p.api_at,
			count(*), MIN(b.started_at), MAX(b.completed_at),
			`+rebuilt+`
		FROM builds b
		LEFT JOIN pipelines p ON p.instance = b.instance AND p.pipeline_id = b.pipeline_id
		LEFT JOIN projects pr ON pr.instance = b.instance AND pr.project_id = COALESCE(p.project_id, b.project_id)
		WHERE b.instance = ? AND b.pipeline_id = ?
		GROUP BY b.instance, b.pipeline_id`, inst, nullUint(id))
	if err != nil {
		return nil, nil, nil, err
	}
	var pipeline *Pipeline
	for rows.Next() {
		p, err := scanPipeline(rows)
		if err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		pipeline = &p
	}
	rows.Close()
	if pipeline == nil {
		// No build completed yet: the pipeline is known from its running
		// jobs' pods or from GitLab's API.
		rows, err := s.db.QueryContext(ctx, `
			SELECT p.instance, p.pipeline_id, p.project_id, COALESCE(pr.path, ''),
				COALESCE(p.ref, ''), COALESCE(p.sha, ''), COALESCE(p.source, ''), p.created_at,
				COALESCE(p.commit_title, ''), COALESCE(p.commit_author, ''), p.mr_iid,
				COALESCE(p.status, ''), COALESCE(p.web_url, ''), p.api_at, 0, NULL, NULL, 0
			FROM pipelines p
			LEFT JOIN projects pr ON pr.instance = p.instance AND pr.project_id = p.project_id
			WHERE p.instance = ? AND p.pipeline_id = ?`, inst, nullUint(id))
		if err != nil {
			return nil, nil, nil, err
		}
		for rows.Next() {
			p, err := scanPipeline(rows)
			if err != nil {
				rows.Close()
				return nil, nil, nil, err
			}
			pipeline = &p
		}
		rows.Close()
	}
	if pipeline == nil {
		return nil, nil, nil, nil
	}
	jobs, err := s.jobs(ctx, "WHERE instance = ? AND pipeline_id = ? ORDER BY started_at, job_id", inst, nullUint(id))
	if err != nil {
		return nil, nil, nil, err
	}
	builds, err := s.Pipeline(ctx, instance, id)
	return pipeline, jobs, builds, err
}

func (s *Store) jobs(ctx context.Context, where string, args ...any) ([]JobInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT instance, job_id, pipeline_id, name, stage, url, node, started_at,
		status, queued_at, finished_at, duration_ms, queued_ms, runner FROM jobs `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobInfo
	for rows.Next() {
		var (
			j                                  JobInfo
			instance, pipeline, started        sql.NullInt64
			queued, finished, duration, waited sql.NullInt64
		)
		if err := rows.Scan(&instance, &j.ID, &pipeline, &j.Name, &j.Stage, &j.URL, &j.Node, &started,
			&j.Status, &queued, &finished, &duration, &waited, &j.Runner); err != nil {
			return nil, err
		}
		j.Instance = fmt.Sprintf("%06x", instance.Int64)
		j.PipelineID, j.Started = uint64(pipeline.Int64), msTime(started)
		j.Queued, j.Finished = msTime(queued), msTime(finished)
		j.Duration, j.Waited = time.Duration(duration.Int64)*time.Millisecond, time.Duration(waited.Int64)*time.Millisecond
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
		pipeline, _, _, err := s.PipelinePage(ctx, a.Instance, a.Pipeline)
		if err != nil {
			return nil, err
		}
		p.Pipeline = pipeline
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
		SELECT b.id, COALESCE(b.pipeline_id, 0), COALESCE(p.sha, ''), d.name, s.state, s.dur_ms, b.started_at + s.start_ms
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
		if err := rows.Scan(&r.BuildID, &r.Pipeline, &r.SHA, &r.Daemon, &state, &dur, &started); err != nil {
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
