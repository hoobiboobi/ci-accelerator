package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// APIJob is a job as GitLab's API describes it.
type APIJob struct {
	ID                        uint64
	Name, Stage, Status       string
	WebURL, Runner            string
	Queued, Started, Finished time.Time
	Duration, Waited          time.Duration
}

// APIPipeline is a pipeline and its jobs as GitLab's API describes them.
type APIPipeline struct {
	Instance  string
	ID        uint64
	ProjectID uint64
	Status    string
	WebURL    string
	Jobs      []APIJob
	Read      time.Time
}

// Finished says whether a pipeline status is final: its jobs no longer
// change.
func Finished(status string) bool {
	switch status {
	case "success", "failed", "canceled", "skipped":
		return true
	}
	return false
}

// SaveAPIPipeline stores what GitLab's API says about a pipeline and its
// jobs. Jobs that ran on other runners are stored too: the pipeline page
// lists them without builds.
func (s *Store) SaveAPIPipeline(ctx context.Context, p APIPipeline) error {
	instance := nullInstance(p.Instance)
	if !instance.Valid || p.ID == 0 {
		return fmt.Errorf("store: pipeline without instance or id: %+v", p)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	project := nullUint(p.ProjectID)
	if _, err := tx.ExecContext(ctx, `INSERT INTO pipelines (instance, pipeline_id, project_id, status, web_url, api_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (instance, pipeline_id) DO UPDATE SET project_id = COALESCE(pipelines.project_id, excluded.project_id),
			status = excluded.status, web_url = excluded.web_url, api_at = excluded.api_at`,
		instance, nullUint(p.ID), project, p.Status, p.WebURL, p.Read.UnixMilli()); err != nil {
		return err
	}
	for _, j := range p.Jobs {
		if j.ID == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO jobs (instance, job_id, pipeline_id, project_id, name, stage, url,
			started_at, status, queued_at, finished_at, duration_ms, queued_ms, runner)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (instance, job_id) DO UPDATE SET
				pipeline_id = COALESCE(jobs.pipeline_id, excluded.pipeline_id),
				project_id = COALESCE(jobs.project_id, excluded.project_id),
				name = CASE WHEN jobs.name = '' THEN excluded.name ELSE jobs.name END,
				stage = CASE WHEN jobs.stage = '' THEN excluded.stage ELSE jobs.stage END,
				url = CASE WHEN jobs.url = '' THEN excluded.url ELSE jobs.url END,
				started_at = COALESCE(excluded.started_at, jobs.started_at),
				status = excluded.status, queued_at = excluded.queued_at, finished_at = excluded.finished_at,
				duration_ms = excluded.duration_ms, queued_ms = excluded.queued_ms, runner = excluded.runner`,
			instance, j.ID, nullUint(p.ID), project, j.Name, j.Stage, j.WebURL, nullTime(j.Started), j.Status,
			nullTime(j.Queued), nullTime(j.Finished), nullDuration(j.Duration), nullDuration(j.Waited), j.Runner); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.changed(PipelineKey(fmt.Sprintf("%06x", instance.Int64), p.ID))
	return nil
}

func nullDuration(d time.Duration) sql.NullInt64 {
	return sql.NullInt64{Int64: d.Milliseconds(), Valid: d > 0}
}

// ProjectKey names a project: its GitLab instance and id.
type ProjectKey struct {
	Instance string
	ID       uint64
}

// String is the key as the console's addresses carry it ("0a1b2c/42").
func (k ProjectKey) String() string { return fmt.Sprintf("%s/%d", k.Instance, k.ID) }

// ParseProjectKey reads a key written by String.
func ParseProjectKey(s string) (ProjectKey, bool) {
	inst, id, ok := strings.Cut(s, "/")
	n, err := strconv.ParseUint(id, 10, 64)
	if !ok || err != nil || !nullInstance(inst).Valid || n == 0 {
		return ProjectKey{}, false
	}
	return ProjectKey{Instance: inst, ID: n}, true
}

// Project is a project with builds here.
type Project struct {
	ProjectKey
	Path      string
	Builds    int
	Pipelines int
	Last      time.Time
}

// Projects returns the projects with builds, the most recently built first.
func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.instance, b.project_id, COALESCE(pr.path, ''), count(*), count(DISTINCT b.pipeline_id), max(b.started_at)
		FROM builds b LEFT JOIN projects pr ON pr.instance = b.instance AND pr.project_id = b.project_id
		WHERE b.project_id IS NOT NULL
		GROUP BY b.instance, b.project_id ORDER BY max(b.started_at) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var (
			p        Project
			instance int64
			last     sql.NullInt64
		)
		if err := rows.Scan(&instance, &p.ID, &p.Path, &p.Builds, &p.Pipelines, &last); err != nil {
			return nil, err
		}
		p.Instance, p.Last = fmt.Sprintf("%06x", instance), msTime(last)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListJob is one job of a pipeline in the console's pipeline list: its
// last build, with what the list needs to say about it.
type ListJob struct {
	JobInfo
	BuildID int64
	Builds  int
	Target  string
	Daemon  string
	Reason  history.ReasonKind
	// Error is BuildKit's error of a failed build of the job: of the last
	// one that failed, or else of the last canceled one.
	Error     string
	Built     int // steps executed other than resolving an image
	Started   time.Time
	Completed time.Time
}

// ListPipeline is a pipeline with its jobs that built here.
type ListPipeline struct {
	Pipeline
	Jobs []ListJob
}

// List returns the latest pipelines with builds, newest first, with the
// jobs that built in them. A zero project lists every project.
func (s *Store) List(ctx context.Context, project ProjectKey, limit int) ([]ListPipeline, error) {
	where, args := "", []any{}
	if project.ID != 0 {
		where, args = "AND b.instance = ? AND b.project_id = ?", []any{nullInstance(project.Instance), project.ID}
	}
	ps, err := s.pipelines(ctx, where, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	out := make([]ListPipeline, 0, len(ps))
	for _, p := range ps {
		jobs, err := s.ListJobs(ctx, p.Instance, p.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, ListPipeline{Pipeline: p, Jobs: jobs})
	}
	return out, nil
}

// ListJobs returns a pipeline's jobs with their last build each, in the
// order their builds started.
func (s *Store) ListJobs(ctx context.Context, instance string, pipeline uint64) ([]ListJob, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.id, b.job_id, b.target, d.name, b.reason_kind, b.error, b.started_at, b.completed_at,
			(SELECT count(*) FROM steps s LEFT JOIN step_defs sd ON sd.identity = s.identity
				WHERE s.build_id = b.id AND s.state = 1 AND COALESCE(s.name, sd.name) NOT LIKE 'FROM %')
		FROM builds b JOIN daemons d ON d.id = b.daemon_id
		WHERE b.instance = ? AND b.pipeline_id = ? AND b.job_id IS NOT NULL
		ORDER BY b.started_at, b.id`, nullInstance(instance), nullUint(pipeline))
	if err != nil {
		return nil, err
	}
	byJob := map[uint64]*ListJob{}
	var order []uint64
	for rows.Next() {
		var (
			j                  ListJob
			job                uint64
			reason             string
			started, completed sql.NullInt64
		)
		if err := rows.Scan(&j.BuildID, &job, &j.Target, &j.Daemon, &reason, &j.Error, &started, &completed, &j.Built); err != nil {
			rows.Close()
			return nil, err
		}
		j.Reason, j.Started, j.Completed = history.ReasonKind(reason), msTime(started), msTime(completed)
		if prev := byJob[job]; prev != nil {
			// A job with several builds shows its last, and a failure
			// of any of them.
			j.Builds = prev.Builds
			if j.Error == "" || (prev.Error != "" && !strings.Contains(prev.Error, "context canceled")) {
				j.Error = prev.Error
			}
		} else {
			order = append(order, job)
		}
		j.Builds++
		j.ID = job
		byJob[job] = &j
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	infos, err := s.jobs(ctx, "WHERE instance = ? AND pipeline_id = ?", nullInstance(instance), nullUint(pipeline))
	if err != nil {
		return nil, err
	}
	for _, info := range infos {
		if j := byJob[info.ID]; j != nil {
			j.JobInfo = info
		}
	}
	out := make([]ListJob, 0, len(order))
	for _, id := range order {
		j := byJob[id]
		if j.Instance == "" {
			j.Instance, j.PipelineID = instance, pipeline
		}
		out = append(out, *j)
	}
	return out, nil
}

// PipelineJobs returns every job of a pipeline the console knows, from its
// pods or from GitLab's API, in the order they started.
func (s *Store) PipelineJobs(ctx context.Context, instance string, pipeline uint64) ([]JobInfo, error) {
	return s.jobs(ctx, "WHERE instance = ? AND pipeline_id = ? ORDER BY COALESCE(started_at, queued_at), job_id",
		nullInstance(instance), nullUint(pipeline))
}

// ProjectBuilds returns a project's latest builds with their steps,
// oldest first.
func (s *Store) ProjectBuilds(ctx context.Context, project ProjectKey, limit int) ([]*Stored, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+buildColumns+` FROM builds b JOIN daemons d ON d.id = b.daemon_id
		WHERE b.instance = ? AND b.project_id = ? ORDER BY b.started_at DESC, b.id DESC LIMIT ?`,
		nullInstance(project.Instance), project.ID, limit)
	if err != nil {
		return nil, err
	}
	out, err := s.scanBuilds(ctx, rows)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Build.Started.Before(out[j].Build.Started) })
	return out, nil
}

// Usual is the median time BuildKit took for the last builds of the same
// project and target before b (successful ones); 0 without any.
func (s *Store) Usual(ctx context.Context, b *history.Build, project uint64) (time.Duration, error) {
	if project == 0 || b.Target == "" {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT completed_at - started_at FROM builds
		WHERE instance = ? AND project_id = ? AND target = ? AND started_at < ? AND completed_at IS NOT NULL AND error = ''
		ORDER BY started_at DESC LIMIT 10`, nullInstance(b.Attribution.Instance), project, b.Target, b.Started.UnixMilli())
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ds []int64
	for rows.Next() {
		var d int64
		if err := rows.Scan(&d); err != nil {
			return 0, err
		}
		ds = append(ds, d)
	}
	if err := rows.Err(); err != nil || len(ds) == 0 {
		return 0, err
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return time.Duration(ds[len(ds)/2]) * time.Millisecond, nil
}

// DaemonCount is what the store knows about one daemon's builds.
type DaemonCount struct {
	Builds    int
	OtherNode int // rebuilt here only because the cache was on another node
	Unclear   int // rebuilt here, files or node
	Last      time.Time
}

// DaemonCounts returns, per daemon name, its stored builds.
func (s *Store) DaemonCounts(ctx context.Context) (map[string]DaemonCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.name, count(b.id),
			COALESCE(sum(b.reason_kind = 'other-node'), 0), COALESCE(sum(b.reason_kind = 'cache-elsewhere'), 0), max(b.started_at)
		FROM daemons d LEFT JOIN builds b ON b.daemon_id = d.id GROUP BY d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]DaemonCount{}
	for rows.Next() {
		var (
			name string
			c    DaemonCount
			last sql.NullInt64
		)
		if err := rows.Scan(&name, &c.Builds, &c.OtherNode, &c.Unclear, &last); err != nil {
			return nil, err
		}
		c.Last = msTime(last)
		out[name] = c
	}
	return out, rows.Err()
}

// JobEnded records when a job's pod went away, which is when the job
// ended, unless GitLab's API already said. Its duration then counts from
// the job's start.
func (s *Store) JobEnded(ctx context.Context, instance string, job uint64, at time.Time) error {
	inst := nullInstance(instance)
	var pipeline sql.NullInt64
	err := s.db.QueryRowContext(ctx, `UPDATE jobs SET finished_at = ?,
			duration_ms = CASE WHEN started_at IS NOT NULL AND ? > started_at THEN ? - started_at END
		WHERE instance = ? AND job_id = ? AND finished_at IS NULL
		RETURNING pipeline_id`, at.UnixMilli(), at.UnixMilli(), at.UnixMilli(), inst, nullUint(job)).Scan(&pipeline)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err == nil && inst.Valid && pipeline.Valid {
		s.changed(pipelineKey(inst, pipeline))
	}
	return err
}

// Announce tells subscribers about changes the store does not hold, such
// as builds that started; key "" means nothing in particular.
func (s *Store) Announce(keys ...string) { s.changed(keys...) }
