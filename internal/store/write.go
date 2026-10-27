package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// Build is what the collector saves about one build record.
type Build struct {
	*history.Build
	// Generation is the record's generation: a record is saved again when
	// the daemon updates it (for example when the trace is attached), and
	// an older generation never replaces a newer one.
	Generation int64
	// Reason is FirstRebuild's (and Settle's) answer; Kind empty for none.
	Reason history.Reason
}

var states = map[history.StepState]int{history.Cached: 0, history.Built: 1, history.Received: 2}

var attributions = map[history.AttributionState]int{history.Unattributed: 0, history.Attributed: 1, history.Ambiguous: 2}

// SaveBuild stores a build, its steps, its reason and its graph, and then
// its steps' output. It returns the build's id.
func (s *Store) SaveBuild(ctx context.Context, b Build) (int64, error) {
	now := s.opts.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	daemon, err := s.daemon(ctx, tx, b.Daemon)
	if err != nil {
		return 0, err
	}
	var dockerfile any
	if name, text := b.Dockerfile(); text != "" {
		id := hash64(text)
		if _, err := tx.ExecContext(ctx, `INSERT INTO dockerfiles (id, name, text, last_seen) VALUES (?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET last_seen = excluded.last_seen`, id, name, text, now); err != nil {
			return 0, err
		}
		dockerfile = id
	}
	defNames := map[string]string{}
	for _, st := range b.Steps {
		if st.Identity != "" && defNames[st.Identity] == "" {
			defNames[st.Identity] = st.Name
		}
	}
	for _, d := range b.Defs() {
		id, err := identity(d.Identity)
		if err != nil {
			return 0, err
		}
		inputs := make([]byte, 0, 8*len(d.Inputs))
		for _, in := range d.Inputs {
			v, err := identity(in)
			if err != nil {
				return 0, err
			}
			inputs = binary.BigEndian.AppendUint64(inputs, uint64(v))
		}
		// A step keeps its own name only where it differs from the one
		// stored with its identity, which may come from an earlier build.
		var stored string
		if err := tx.QueryRowContext(ctx, `INSERT INTO step_defs (identity, op, inputs, name, last_seen) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (identity) DO UPDATE SET last_seen = excluded.last_seen,
				name = CASE WHEN step_defs.name = '' THEN excluded.name ELSE step_defs.name END
			RETURNING name`,
			id, []byte(d.Op), inputs, defNames[d.Identity], now).Scan(&stored); err != nil {
			return 0, err
		}
		defNames[d.Identity] = stored
	}

	a := b.Attribution
	instance, pipeline, job := nullInstance(a.Instance), nullUint(a.Pipeline), nullUint(a.Job)
	var project sql.NullInt64
	if instance.Valid && job.Valid {
		err := tx.QueryRowContext(ctx, "SELECT project_id FROM jobs WHERE instance = ? AND job_id = ?", instance, job).Scan(&project)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	args := map[string]string{}
	for k, v := range b.BuildArgs {
		if history.SecretArg(k) {
			v = history.Masked
		}
		args[k] = v
	}
	argsJSON, _ := json.Marshal(args)
	images, _ := json.Marshal(nonNil(b.Images))
	warnings, _ := json.Marshal(nonNil(b.Warnings))
	outputs := map[int]string{}
	for i, st := range b.Steps {
		if st.Output != "" && i < MaxSteps {
			outputs[i] = st.Output
		}
	}
	var outDay any
	if len(outputs) > 0 {
		end := b.Completed
		if end.IsZero() {
			end = s.opts.Now()
		}
		outDay = day(end)
	}
	r := b.Reason
	reasonStep, reasonBy := sql.NullInt64{}, sql.NullInt64{}
	for i, st := range b.Steps {
		if r.Kind != "" && st.Key == r.Step.Key {
			reasonStep = sql.NullInt64{Int64: int64(i), Valid: true}
			break
		}
	}
	if r.By != "" {
		if err := tx.QueryRowContext(ctx, "SELECT id FROM builds WHERE ref = ? LIMIT 1", r.By).Scan(&reasonBy); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	paths, _ := json.Marshal(nonNil(r.Paths))
	var built, builtMs int64
	for _, st := range b.Steps {
		if st.State == history.Built && !strings.HasPrefix(st.Name, "FROM ") {
			built++
			builtMs += st.Duration().Milliseconds()
		}
	}
	saved, err := s.timeSaved(ctx, tx, b.Build, instance, project)
	if err != nil {
		return 0, err
	}

	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO builds (daemon_id, ref, generation, attribution, instance, pipeline_id, job_id, project_id,
			target, dockerfile_id, build_args, started_at, completed_at, error, context_bytes, images,
			reason_kind, reason_step, reason_old, reason_new, reason_arg, reason_paths, reason_by, saved_ms, output_day, built, built_ms,
			warnings)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (ref, daemon_id) DO UPDATE SET generation = excluded.generation,
			attribution = excluded.attribution, instance = excluded.instance, pipeline_id = excluded.pipeline_id,
			job_id = excluded.job_id, project_id = COALESCE(excluded.project_id, builds.project_id),
			target = excluded.target, dockerfile_id = excluded.dockerfile_id, build_args = excluded.build_args,
			started_at = excluded.started_at, completed_at = excluded.completed_at, error = excluded.error,
			context_bytes = excluded.context_bytes, images = excluded.images,
			reason_kind = excluded.reason_kind, reason_step = excluded.reason_step, reason_old = excluded.reason_old,
			reason_new = excluded.reason_new, reason_arg = excluded.reason_arg, reason_paths = excluded.reason_paths,
			reason_by = excluded.reason_by, reason_via = '', saved_ms = excluded.saved_ms,
			output_day = COALESCE(excluded.output_day, builds.output_day),
			built = excluded.built, built_ms = excluded.built_ms, warnings = excluded.warnings
		WHERE excluded.generation >= builds.generation
		RETURNING id`,
		daemon, b.Ref, b.Generation, attributions[a.State], instance, pipeline, job, project,
		b.Target, dockerfile, string(argsJSON), b.Started.UnixMilli(), nullTime(b.Completed), b.Error, b.ContextBytes, string(images),
		string(r.Kind), reasonStep, r.Old, r.New, r.Arg, string(paths), reasonBy, saved.Milliseconds(), outDay, built, builtMs,
		string(warnings),
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// A newer generation is stored already.
		err = tx.QueryRowContext(ctx, "SELECT id FROM builds WHERE ref = ? AND daemon_id = ?", b.Ref, daemon).Scan(&id)
		if err != nil {
			return 0, err
		}
		return id, tx.Commit()
	}
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM steps WHERE build_id = ?", id); err != nil {
		return 0, err
	}
	for i, st := range b.Steps {
		var ident, name any
		if st.Identity != "" {
			v, err := identity(st.Identity)
			if err != nil {
				return 0, err
			}
			ident = v
		}
		if st.Identity == "" || st.Name != defNames[st.Identity] {
			name = st.Name
		}
		var from sql.NullInt64
		if st.ReceivedFrom != "" {
			err := tx.QueryRowContext(ctx, "SELECT id FROM builds WHERE ref = ? AND daemon_id = ?", st.ReceivedFrom, daemon).Scan(&from)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, err
			}
		}
		start, dur := int64(0), st.Duration().Milliseconds()
		if !st.Started.IsZero() && !b.Started.IsZero() {
			start = st.Started.Sub(b.Started).Milliseconds()
		}
		u := st.Usage
		if _, err := tx.ExecContext(ctx, `INSERT INTO steps (build_id, idx, key, name, identity, state, start_ms, dur_ms, lines,
			received_from, error, output_len, cpu_ms, throttled_ms, read_bytes, write_bytes)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, i, st.Key, name, ident, states[st.State], start, dur, formatLines(st.Lines), from, st.Error, len(st.Output),
			u.CPU.Milliseconds(), u.Throttled.Milliseconds(), u.ReadBytes, u.WriteBytes); err != nil {
			return 0, err
		}
	}
	if project.Valid {
		if _, err := tx.ExecContext(ctx, "UPDATE projects SET last_at = max(COALESCE(last_at, 0), ?) WHERE instance = ? AND project_id = ?",
			b.Started.UnixMilli(), instance, project); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if instance.Valid && pipeline.Valid {
		defer s.changed(pipelineKey(instance, pipeline))
	}
	if outDay != nil {
		if err := s.writeOutputs(ctx, outDay.(int), id, projectKey(instance, project), outputs); err != nil {
			return id, fmt.Errorf("store: output of build %d: %w", id, err)
		}
	}
	return id, nil
}

// timeSaved estimates what the cache and other builds saved this build: for
// each step it did not execute, how long that step took the last time it
// was built in the project (in any project when the project is unknown).
func (s *Store) timeSaved(ctx context.Context, tx *sql.Tx, b *history.Build, instance, project sql.NullInt64) (time.Duration, error) {
	var total time.Duration
	for _, st := range b.Steps {
		if st.Identity == "" || st.State == history.Built {
			continue
		}
		id, err := identity(st.Identity)
		if err != nil {
			return 0, err
		}
		var ms int64
		err = tx.QueryRowContext(ctx, `
			SELECT s.dur_ms FROM steps s JOIN builds b ON b.id = s.build_id
			WHERE s.identity = ? AND s.state = 1 AND (? IS NULL OR (b.instance = ? AND b.project_id = ?))
			ORDER BY s.build_id DESC LIMIT 1`, id, project, instance, project).Scan(&ms)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		total += time.Duration(ms) * time.Millisecond
	}
	return total, nil
}

func (s *Store) daemon(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	// Inside the build's transaction, uncached: an id cached before a
	// rolled-back commit would name a row that does not exist.
	var id int64
	if _, err := tx.ExecContext(ctx, "INSERT INTO daemons (name) VALUES (?) ON CONFLICT (name) DO NOTHING", name); err != nil {
		return 0, err
	}
	err := tx.QueryRowContext(ctx, "SELECT id FROM daemons WHERE name = ?", name).Scan(&id)
	return id, err
}

// SetReason replaces a stored build's reason (Settle may decide it after
// other builds of the pipeline arrive).
func (s *Store) SetReason(ctx context.Context, ref, daemon string, r history.Reason) error {
	paths, _ := json.Marshal(nonNil(r.Paths))
	var instance, pipeline sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		UPDATE builds SET reason_kind = ?, reason_old = ?, reason_new = ?, reason_arg = ?, reason_paths = ?, reason_via = '',
			reason_step = (SELECT idx FROM steps WHERE build_id = builds.id AND key = ? ORDER BY idx LIMIT 1),
			reason_by = (SELECT id FROM builds b WHERE b.ref = ? LIMIT 1)
		WHERE ref = ? AND daemon_id = (SELECT id FROM daemons WHERE name = ?)
		RETURNING instance, pipeline_id`,
		string(r.Kind), r.Old, r.New, r.Arg, string(paths), r.Step.Key, nullString(r.By), ref, daemon).Scan(&instance, &pipeline)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err == nil && instance.Valid && pipeline.Valid {
		s.changed(pipelineKey(instance, pipeline))
	}
	return err
}

// Job is a CI job as its pod's annotations describe it.
type Job struct {
	Instance                      string // trace-id prefix, 6 hex characters
	JobID, PipelineID, ProjectID  uint64
	ProjectPath, Name, Stage, URL string
	Node                          string
	StartedAt                     time.Time
	Ref, SHA, Source              string
	PipelineCreatedAt             time.Time
	CommitTitle, CommitAuthor     string
	MergeRequestIID               uint64
}

// SaveJob stores a job, its pipeline and project, and attaches the project
// to the job's builds saved before the job was known.
func (s *Store) SaveJob(ctx context.Context, j Job) error {
	instance := nullInstance(j.Instance)
	if !instance.Valid || j.JobID == 0 {
		return fmt.Errorf("store: job without instance or id: %+v", j)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	project, pipeline := nullUint(j.ProjectID), nullUint(j.PipelineID)
	if project.Valid {
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects (instance, project_id, path) VALUES (?, ?, ?)
			ON CONFLICT (instance, project_id) DO UPDATE SET path = CASE WHEN excluded.path = '' THEN projects.path ELSE excluded.path END`,
			instance, project, j.ProjectPath); err != nil {
			return err
		}
	}
	if pipeline.Valid {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pipelines (instance, pipeline_id, project_id, ref, sha, source, created_at,
			commit_title, commit_author, mr_iid, seen_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (instance, pipeline_id) DO UPDATE SET project_id = COALESCE(excluded.project_id, pipelines.project_id),
				ref = excluded.ref, sha = excluded.sha, source = excluded.source,
				created_at = COALESCE(excluded.created_at, pipelines.created_at), commit_title = excluded.commit_title,
				commit_author = excluded.commit_author, mr_iid = COALESCE(excluded.mr_iid, pipelines.mr_iid)`,
			instance, pipeline, project, j.Ref, j.SHA, j.Source, nullTime(j.PipelineCreatedAt),
			j.CommitTitle, j.CommitAuthor, nullUint(j.MergeRequestIID), s.opts.Now().UnixMilli()); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs (instance, job_id, pipeline_id, project_id, name, stage, url, started_at, node, seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (instance, job_id) DO UPDATE SET pipeline_id = COALESCE(excluded.pipeline_id, jobs.pipeline_id),
			project_id = COALESCE(excluded.project_id, jobs.project_id), name = excluded.name, stage = excluded.stage,
			url = excluded.url, started_at = COALESCE(excluded.started_at, jobs.started_at), node = excluded.node`,
		instance, j.JobID, pipeline, project, j.Name, j.Stage, j.URL, nullTime(j.StartedAt), j.Node, s.opts.Now().UnixMilli()); err != nil {
		return err
	}
	if project.Valid {
		if _, err := tx.ExecContext(ctx, "UPDATE builds SET project_id = ? WHERE instance = ? AND job_id = ? AND project_id IS NULL",
			project, instance, j.JobID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET last_at = max(COALESCE(last_at, 0),
			COALESCE((SELECT max(started_at) FROM builds WHERE instance = ?1 AND job_id = ?2), 0))
			WHERE instance = ?1 AND project_id = ?3 AND EXISTS (SELECT 1 FROM builds WHERE instance = ?1 AND job_id = ?2)`,
			instance, j.JobID, project); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if pipeline.Valid {
		s.changed(pipelineKey(instance, pipeline))
	}
	return nil
}

// identity converts a step identity (16 hex characters) to its column value.
func identity(h string) (int64, error) {
	v, err := strconv.ParseUint(h, 16, 64)
	if err != nil || len(h) != 16 {
		return 0, fmt.Errorf("store: step identity %q", h)
	}
	return int64(v), nil
}

func identityString(v int64) string { return fmt.Sprintf("%016x", uint64(v)) }

func hash64(s string) int64 {
	sum := sha256.Sum256([]byte(s))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func pipelineKey(instance, pipeline sql.NullInt64) string {
	return PipelineKey(fmt.Sprintf("%06x", instance.Int64), uint64(pipeline.Int64))
}

func nullInstance(h string) sql.NullInt64 {
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil || len(h) != 6 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(v), Valid: true}
}

func nullUint(v uint64) sql.NullInt64 {
	return sql.NullInt64{Int64: int64(v), Valid: v != 0}
}

func nullTime(t time.Time) sql.NullInt64 {
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: !t.IsZero()}
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

func formatLines(rs []history.LineRange) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = fmt.Sprintf("%d-%d", r.Start, r.End)
	}
	return strings.Join(parts, ",")
}

func parseLines(s string) []history.LineRange {
	var out []history.LineRange
	for _, p := range strings.Split(s, ",") {
		a, b, ok := strings.Cut(p, "-")
		x, err1 := strconv.Atoi(a)
		y, err2 := strconv.Atoi(b)
		if ok && err1 == nil && err2 == nil {
			out = append(out, history.LineRange{Start: x, End: y})
		}
	}
	return out
}
