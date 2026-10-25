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
	AllowFailure              bool   // its failure does not fail the pipeline
	User                      string // who started it (GitLab username)
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
	seen := s.opts.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO pipelines (instance, pipeline_id, project_id, status, web_url, api_at, seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (instance, pipeline_id) DO UPDATE SET project_id = COALESCE(pipelines.project_id, excluded.project_id),
			status = excluded.status, web_url = excluded.web_url, api_at = excluded.api_at`,
		instance, nullUint(p.ID), project, p.Status, p.WebURL, p.Read.UnixMilli(), seen); err != nil {
		return err
	}
	for _, j := range p.Jobs {
		if j.ID == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO jobs (instance, job_id, pipeline_id, project_id, name, stage, url,
			started_at, status, queued_at, finished_at, duration_ms, queued_ms, runner, seen_at, allow_failure, started_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (instance, job_id) DO UPDATE SET
				pipeline_id = COALESCE(jobs.pipeline_id, excluded.pipeline_id),
				project_id = COALESCE(jobs.project_id, excluded.project_id),
				name = CASE WHEN jobs.name = '' THEN excluded.name ELSE jobs.name END,
				stage = CASE WHEN jobs.stage = '' THEN excluded.stage ELSE jobs.stage END,
				url = CASE WHEN jobs.url = '' THEN excluded.url ELSE jobs.url END,
				started_at = COALESCE(excluded.started_at, jobs.started_at),
				status = excluded.status, queued_at = excluded.queued_at, finished_at = excluded.finished_at,
				duration_ms = excluded.duration_ms, queued_ms = excluded.queued_ms, runner = excluded.runner,
				allow_failure = excluded.allow_failure,
				started_by = CASE WHEN excluded.started_by = '' THEN jobs.started_by ELSE excluded.started_by END`,
			instance, j.ID, nullUint(p.ID), project, j.Name, j.Stage, j.WebURL, nullTime(j.Started), j.Status,
			nullTime(j.Queued), nullTime(j.Finished), nullDuration(j.Duration), nullDuration(j.Waited), j.Runner, seen, j.AllowFailure, j.User); err != nil {
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
	Path string
	Last time.Time
}

// Projects returns the projects with builds, the most recently built first.
func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT instance, project_id, path, last_at FROM projects
		WHERE last_at IS NOT NULL ORDER BY last_at DESC`)
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
		if err := rows.Scan(&instance, &p.ID, &p.Path, &last); err != nil {
			return nil, err
		}
		p.Instance, p.Last = fmt.Sprintf("%06x", instance), msTime(last)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Job returns one job, or nil when it is not stored.
func (s *Store) Job(ctx context.Context, instance string, id uint64) (*JobInfo, error) {
	js, err := s.jobs(ctx, "WHERE instance = ? AND job_id = ?", nullInstance(instance), nullUint(id))
	if err != nil || len(js) == 0 {
		return nil, err
	}
	return &js[0], nil
}

// PipelineJobs returns every job of a pipeline the console knows, from its
// pods or from GitLab's API, in the order they started.
func (s *Store) PipelineJobs(ctx context.Context, instance string, pipeline uint64) ([]JobInfo, error) {
	// GitLab queues every job of a pipeline when it creates it: jobs that
	// never started (skipped, canceled while waiting, manual) come last.
	return s.jobs(ctx, "WHERE instance = ? AND pipeline_id = ? ORDER BY started_at IS NULL, started_at, queued_at, job_id",
		nullInstance(instance), nullUint(pipeline))
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
	Builds int
	// Farm counts rebuilds for want of cache here (history.FarmKinds: it
	// was on another node, or no longer on this one) — the same set the
	// Overview's "build farm" uses, so the two pages always agree.
	Farm    int
	Unclear int // rebuilt here, files or node
	Last    time.Time
}

// DaemonCounts returns, per daemon name, its builds started since t.
func (s *Store) DaemonCounts(ctx context.Context, since time.Time) (map[string]DaemonCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.name, count(b.id),
			COALESCE(sum(b.reason_kind IN `+farmKinds+`), 0), COALESCE(sum(b.reason_kind = 'cache-elsewhere'), 0), max(b.started_at)
		FROM daemons d LEFT JOIN builds b ON b.daemon_id = d.id AND b.started_at >= ? GROUP BY d.name`, since.UnixMilli())
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
		if err := rows.Scan(&name, &c.Builds, &c.Farm, &c.Unclear, &last); err != nil {
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

// PipelineProject returns the project of a pipeline as the store knows it
// (from GitLab, its jobs or its builds), 0 when it does not.
func (s *Store) PipelineProject(ctx context.Context, instance string, pipeline uint64) (uint64, error) {
	var project sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(
		(SELECT project_id FROM pipelines WHERE instance = ?1 AND pipeline_id = ?2),
		(SELECT project_id FROM jobs WHERE instance = ?1 AND pipeline_id = ?2 AND project_id IS NOT NULL LIMIT 1),
		(SELECT project_id FROM builds WHERE instance = ?1 AND pipeline_id = ?2 AND project_id IS NOT NULL LIMIT 1))`,
		nullInstance(instance), pipeline).Scan(&project)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return uint64(project.Int64), err
}
