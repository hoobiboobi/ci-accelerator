package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// BuildRow is a build as the console's lists show it: what it was, how
// it ended and why, without its steps.
type BuildRow struct {
	ID                int64
	Instance          string
	Pipeline, Job     uint64
	Target, Daemon    string
	Started           time.Time
	Completed         time.Time // zero while it runs
	Error             string
	Reason            history.ReasonKind
	ReasonStep        string // the first rebuilt step's instruction
	Paths             []string
	Old, New, Arg     string
	Built             int           // steps executed (resolving images aside)
	BuiltTime         time.Duration // their time
	Ref, SHA          string
	Title, Author     string
	JobName           string
	PipelineCreatedAt time.Time
}

// Duration is the build's time in BuildKit; 0 while it runs.
func (b BuildRow) Duration() time.Duration {
	if b.Completed.IsZero() {
		return 0
	}
	return b.Completed.Sub(b.Started)
}

// BuildQuery selects builds of a project; zero fields select everything.
type BuildQuery struct {
	Project  ProjectKey
	Ref      string
	Target   string
	Author   string
	Pipeline uint64
	// Outcome narrows to one status: cached, change, farm, unclear,
	// failed, or rebuilt (every build that executed steps or failed).
	Outcome string
	// Since and Before bound the start time (Before excluded).
	Since, Before time.Time
	Limit         int
	// Light leaves out the pipeline and the job (branch, commit, job
	// name): for figures over many builds.
	Light bool
}

// changeKinds are the reasons that are the build's own change.
const changeKinds = `('first-build', 'step-changed', 'new-step', 'arg-changed', 'base-image-changed', 'files-changed')`

// outcomes select finished builds by status, as the web pages name them:
// a reason decides before the executed steps do (a build whose rebuilt
// step came from another job executed nothing and was not cached).
var outcomes = map[string]string{
	"cached":  "(b.built = 0 AND b.error = '' AND b.reason_kind = '' AND b.completed_at IS NOT NULL)",
	"change":  "(b.error = '' AND b.reason_kind IN " + changeKinds + " AND b.completed_at IS NOT NULL)",
	"farm":    "(b.error = '' AND b.reason_kind IN ('other-node', 'not-in-cache') AND b.completed_at IS NOT NULL)",
	"unclear": "(b.error = '' AND (b.reason_kind = 'cache-elsewhere' OR (b.reason_kind = '' AND b.built > 0)) AND b.completed_at IS NOT NULL)",
	"failed":  "(b.error != '')",
	"rebuilt": "(b.error != '' OR (b.completed_at IS NOT NULL AND (b.built > 0 OR b.reason_kind != '')))",
}

const feedCols = `b.id, b.instance, b.pipeline_id, b.job_id, b.target, d.name, b.started_at, b.completed_at, b.error,
	b.reason_kind, b.reason_paths, b.reason_old, b.reason_new, b.reason_arg, b.built, b.built_ms,
	COALESCE(p.ref, ''), COALESCE(p.sha, ''), COALESCE(p.commit_title, ''), COALESCE(p.commit_author, ''),
	COALESCE(j.name, ''), p.created_at,
	CASE WHEN b.reason_step IS NULL THEN '' ELSE COALESCE((SELECT COALESCE(s.name, sd.name, '') FROM steps s
		LEFT JOIN step_defs sd ON sd.identity = s.identity WHERE s.build_id = b.id AND s.idx = b.reason_step), '') END`

// Builds returns a project's builds, newest first.
func (s *Store) Builds(ctx context.Context, q BuildQuery) ([]BuildRow, error) {
	where := []string{"b.instance = ?", "b.project_id = ?"}
	args := []any{nullInstance(q.Project.Instance), nullUint(q.Project.ID)}
	if q.Ref != "" {
		where, args = append(where, "p.ref = ?"), append(args, q.Ref)
	}
	if q.Target != "" {
		where, args = append(where, "b.target = ?"), append(args, q.Target)
	}
	if q.Author != "" {
		// CI_COMMIT_AUTHOR is "Name <email>".
		where, args = append(where, "(p.commit_author = ? OR substr(p.commit_author, 1, ?) = ?)"),
			append(args, q.Author, len(q.Author)+2, q.Author+" <")
	}
	if q.Pipeline != 0 {
		where, args = append(where, "b.pipeline_id = ?"), append(args, nullUint(q.Pipeline))
	}
	if c, ok := outcomes[q.Outcome]; ok {
		where = append(where, c)
	}
	if !q.Since.IsZero() {
		where, args = append(where, "b.started_at >= ?"), append(args, q.Since.UnixMilli())
	}
	if !q.Before.IsZero() {
		where, args = append(where, "b.started_at < ?"), append(args, q.Before.UnixMilli())
	}
	limit := q.Limit
	if limit <= 0 || limit > 100000 {
		limit = 100000
	}
	cols, joins := feedCols, `LEFT JOIN pipelines p ON p.instance = b.instance AND p.pipeline_id = b.pipeline_id
		LEFT JOIN jobs j ON j.instance = b.instance AND j.job_id = b.job_id`
	if q.Light && q.Ref == "" && q.Author == "" {
		cols, joins = strings.NewReplacer("COALESCE(p.ref, '')", "''", "COALESCE(p.sha, '')", "''", "COALESCE(p.commit_title, '')", "''",
			"COALESCE(p.commit_author, '')", "''", "COALESCE(j.name, '')", "''", "p.created_at", "NULL").Replace(feedCols), ""
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+`
		FROM builds b JOIN daemons d ON d.id = b.daemon_id `+joins+`
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY b.started_at DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BuildRow
	for rows.Next() {
		var (
			r                  BuildRow
			instance           sql.NullInt64
			pipeline, job      sql.NullInt64
			started            int64
			completed, created sql.NullInt64
			kind, paths        string
			builtMs            int64
		)
		if err := rows.Scan(&r.ID, &instance, &pipeline, &job, &r.Target, &r.Daemon, &started, &completed, &r.Error,
			&kind, &paths, &r.Old, &r.New, &r.Arg, &r.Built, &builtMs, &r.Ref, &r.SHA, &r.Title, &r.Author,
			&r.JobName, &created, &r.ReasonStep); err != nil {
			return nil, err
		}
		if instance.Valid {
			r.Instance = fmt.Sprintf("%06x", instance.Int64)
		}
		r.Pipeline, r.Job = uint64(pipeline.Int64), uint64(job.Int64)
		r.Started, r.Completed, r.PipelineCreatedAt = time.UnixMilli(started).UTC(), msTime(completed), msTime(created)
		r.Reason, r.BuiltTime = history.ReasonKind(kind), time.Duration(builtMs)*time.Millisecond
		_ = json.Unmarshal([]byte(paths), &r.Paths)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Targets returns the targets a project built since t, by name.
func (s *Store) Targets(ctx context.Context, p ProjectKey, since time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT target FROM builds
		WHERE instance = ? AND project_id = ? AND started_at >= ? ORDER BY target`,
		nullInstance(p.Instance), nullUint(p.ID), since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Branches returns the branches a project built on since t, the most recent first.
func (s *Store) Branches(ctx context.Context, p ProjectKey, since time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ref, max(COALESCE(created_at, seen_at)) FROM pipelines
		WHERE instance = ? AND project_id = ? AND COALESCE(created_at, seen_at) >= ? AND ref != ''
		GROUP BY ref ORDER BY 2 DESC LIMIT 50`,
		nullInstance(p.Instance), nullUint(p.ID), since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var (
			ref string
			t   int64
		)
		if err := rows.Scan(&ref, &t); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// PipelineJobCount is how many jobs of a pipeline the console saw.
func (s *Store) PipelineJobCount(ctx context.Context, instance string, pipeline uint64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM jobs WHERE instance = ? AND pipeline_id = ?",
		nullInstance(instance), nullUint(pipeline)).Scan(&n)
	return n, err
}

// StepCount is how often a step of a target ran instead of coming from
// the cache.
type StepCount struct {
	Name    string
	Ran     int
	RanTime time.Duration
}

// StepCounts returns the steps of a target's builds since t that ran,
// the most often run first. Only builds that executed steps are read.
func (s *Store) StepCounts(ctx context.Context, p ProjectKey, target string, since time.Time, limit int) ([]StepCount, error) {
	// Grouped by identity first: names come from the step definitions
	// once per step, not once per run.
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(x.name, sd.name, '') AS n, SUM(x.c), SUM(x.ms)
		FROM (SELECT s.identity, s.name, count(*) AS c, SUM(s.dur_ms) AS ms
			FROM builds b JOIN steps s ON s.build_id = b.id AND s.state = 1
			WHERE b.instance = ? AND b.project_id = ? AND b.target = ? AND b.started_at >= ? AND b.built > 0 AND b.error = ''
			GROUP BY s.identity, s.name) x
		LEFT JOIN step_defs sd ON sd.identity = x.identity
		WHERE COALESCE(x.name, sd.name, '') NOT LIKE 'FROM %'
		GROUP BY n ORDER BY 2 DESC, 3 DESC LIMIT ?`,
		nullInstance(p.Instance), nullUint(p.ID), target, since.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StepCount
	for rows.Next() {
		var (
			c  StepCount
			ms int64
		)
		if err := rows.Scan(&c.Name, &c.Ran, &ms); err != nil {
			return nil, err
		}
		c.RanTime = time.Duration(ms) * time.Millisecond
		out = append(out, c)
	}
	return out, rows.Err()
}
