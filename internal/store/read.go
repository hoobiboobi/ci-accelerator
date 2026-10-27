package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

var stateNames = map[int]history.StepState{0: history.Cached, 1: history.Built, 2: history.Received}

var attributionNames = map[int]history.AttributionState{0: history.Unattributed, 1: history.Attributed, 2: history.Ambiguous}

// Stored is a build read back from the store: comparable with FirstRebuild
// (its graph restored from the step definitions), and with its reason.
type Stored struct {
	ID         int64
	Generation int64
	Build      *history.Build
	Reason     history.Reason
	// Saved is the build time its cached and received steps saved.
	Saved time.Duration
	// Via is who decided the reason after the build was stored (see
	// SettleElsewhere); empty: BuildKit's records alone.
	Via string
}

const buildColumns = `b.id, b.generation, d.name, b.ref, b.attribution, b.instance, b.pipeline_id, b.job_id,
	b.target, b.build_args, b.started_at, b.completed_at, b.error, b.context_bytes, b.images,
	b.reason_kind, b.reason_step, b.reason_old, b.reason_new, b.reason_arg, b.reason_paths,
	(SELECT ref FROM builds x WHERE x.id = b.reason_by), b.saved_ms, b.reason_via, b.warnings`

// Previous returns the latest build of the same project and target that
// started before b, restored for FirstRebuild; nil when there is none.
// Builds of b's own pipeline do not count: they build the same commit at
// the same time (parallel jobs, retries), and comparing with one makes the
// commit's own work look like "nothing changed". known is false while b's
// project is not known (its job not seen yet): then there is nothing to
// compare with, which is not a first build.
func (s *Store) Previous(ctx context.Context, b *history.Build) (prev *Stored, known bool, err error) {
	instance, job := nullInstance(b.Attribution.Instance), nullUint(b.Attribution.Job)
	if !instance.Valid || !job.Valid {
		return nil, false, nil
	}
	var project sql.NullInt64
	err = s.db.QueryRowContext(ctx, "SELECT project_id FROM jobs WHERE instance = ? AND job_id = ?", instance, job).Scan(&project)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !project.Valid) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+buildColumns+` FROM builds b JOIN daemons d ON d.id = b.daemon_id
		WHERE b.instance = ? AND b.project_id = ? AND b.target = ? AND b.started_at < ? AND b.ref <> ?
			AND b.pipeline_id IS NOT ?
		ORDER BY b.started_at DESC LIMIT 1`, instance, project, b.Target, b.Started.UnixMilli(), b.Ref,
		nullUint(b.Attribution.Pipeline))
	if err != nil {
		return nil, true, err
	}
	found, err := s.scanBuilds(ctx, rows)
	if err != nil || len(found) == 0 {
		return nil, true, err
	}
	return found[0], true, s.restore(ctx, found[0])
}

// Next returns the first build of the same project and target that
// started after b (its reason compares it with b), restored; nil when there
// is none or b's project is not known.
func (s *Store) Next(ctx context.Context, b *history.Build) (*Stored, error) {
	instance, job := nullInstance(b.Attribution.Instance), nullUint(b.Attribution.Job)
	if !instance.Valid || !job.Valid {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+buildColumns+` FROM builds b JOIN daemons d ON d.id = b.daemon_id
		WHERE b.instance = ? AND b.project_id = (SELECT project_id FROM jobs WHERE instance = ? AND job_id = ?)
			AND b.target = ? AND b.started_at > ? AND b.ref <> ? AND b.pipeline_id IS NOT ?
		ORDER BY b.started_at LIMIT 1`, instance, instance, job, b.Target, b.Started.UnixMilli(), b.Ref,
		nullUint(b.Attribution.Pipeline))
	if err != nil {
		return nil, err
	}
	found, err := s.scanBuilds(ctx, rows)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	return found[0], s.restore(ctx, found[0])
}

// JobBuilds returns the ids of a job's builds.
func (s *Store) JobBuilds(ctx context.Context, instance string, job uint64) ([]int64, error) {
	return s.ids(ctx, "SELECT id FROM builds WHERE instance = ? AND job_id = ? ORDER BY started_at", nullInstance(instance), nullUint(job))
}

// Recent returns the latest builds, newest first, with their steps.
func (s *Store) Recent(ctx context.Context, limit int) ([]*Stored, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+buildColumns+` FROM builds b JOIN daemons d ON d.id = b.daemon_id
		ORDER BY b.started_at DESC, b.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return s.scanBuilds(ctx, rows)
}

func (s *Store) ids(ctx context.Context, q string, args ...any) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Load returns one stored build with its graph restored, so that reasons
// can be computed again from the store alone (for example when its
// project becomes known after it was saved).
func (s *Store) Load(ctx context.Context, id int64) (*Stored, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+buildColumns+` FROM builds b JOIN daemons d ON d.id = b.daemon_id WHERE b.id = ?`, id)
	if err != nil {
		return nil, err
	}
	found, err := s.scanBuilds(ctx, rows)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, sql.ErrNoRows
	}
	return found[0], s.restore(ctx, found[0])
}

// Pipeline returns the stored builds of a pipeline with their steps and
// reasons, enough for Settle; their graphs are not restored.
func (s *Store) Pipeline(ctx context.Context, instance string, pipeline uint64) ([]*Stored, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+buildColumns+` FROM builds b JOIN daemons d ON d.id = b.daemon_id
		WHERE b.instance = ? AND b.pipeline_id = ? ORDER BY b.started_at`, nullInstance(instance), nullUint(pipeline))
	if err != nil {
		return nil, err
	}
	return s.scanBuilds(ctx, rows)
}

func (s *Store) scanBuilds(ctx context.Context, rows *sql.Rows) ([]*Stored, error) {
	var out []*Stored
	var reasonSteps []sql.NullInt64
	for rows.Next() {
		var (
			st                                        Stored
			b                                         history.Build
			attribution                               int
			instance, pipeline, job, completed, rstep sql.NullInt64
			args, images, kind, paths, warnings       string
			started, saved                            int64
			by                                        sql.NullString
		)
		if err := rows.Scan(&st.ID, &st.Generation, &b.Daemon, &b.Ref, &attribution, &instance, &pipeline, &job,
			&b.Target, &args, &started, &completed, &b.Error, &b.ContextBytes, &images,
			&kind, &rstep, &st.Reason.Old, &st.Reason.New, &st.Reason.Arg, &paths, &by, &saved, &st.Via, &warnings); err != nil {
			rows.Close()
			return nil, err
		}
		b.Attribution = history.Attribution{State: attributionNames[attribution], Pipeline: uint64(pipeline.Int64), Job: uint64(job.Int64)}
		if instance.Valid {
			b.Attribution.Instance = fmt.Sprintf("%06x", instance.Int64)
		}
		b.Started = time.UnixMilli(started).UTC()
		if completed.Valid {
			b.Completed = time.UnixMilli(completed.Int64).UTC()
		}
		_ = json.Unmarshal([]byte(args), &b.BuildArgs)
		_ = json.Unmarshal([]byte(images), &b.Images)
		_ = json.Unmarshal([]byte(warnings), &b.Warnings)
		st.Reason.Kind = history.ReasonKind(kind)
		_ = json.Unmarshal([]byte(paths), &st.Reason.Paths)
		if len(st.Reason.Paths) == 0 {
			st.Reason.Paths = nil
		}
		st.Reason.By = by.String
		st.Saved = time.Duration(saved) * time.Millisecond
		st.Build = &b
		out = append(out, &st)
		reasonSteps = append(reasonSteps, rstep)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i, st := range out {
		if err := s.loadSteps(ctx, st); err != nil {
			return nil, err
		}
		if r := reasonSteps[i]; r.Valid && int(r.Int64) < len(st.Build.Steps) {
			st.Reason.Step = st.Build.Steps[r.Int64]
		}
	}
	return out, nil
}

func (s *Store) loadSteps(ctx context.Context, st *Stored) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.key, COALESCE(s.name, d.name, ''), s.identity, s.state, s.start_ms, s.dur_ms, s.lines,
			(SELECT ref FROM builds x WHERE x.id = s.received_from), s.error,
			s.cpu_ms, s.throttled_ms, s.read_bytes, s.write_bytes
		FROM steps s LEFT JOIN step_defs d ON d.identity = s.identity
		WHERE s.build_id = ? ORDER BY s.idx`, st.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	b := st.Build
	for rows.Next() {
		var (
			step       history.Step
			ident      sql.NullInt64
			state      int
			start, dur int64
			lines      string
			from       sql.NullString
			cpu, thr   int64
		)
		if err := rows.Scan(&step.Key, &step.Name, &ident, &state, &start, &dur, &lines, &from, &step.Error,
			&cpu, &thr, &step.Usage.ReadBytes, &step.Usage.WriteBytes); err != nil {
			return err
		}
		if ident.Valid {
			step.Identity = identityString(ident.Int64)
		}
		step.State = stateNames[state]
		step.Started = b.Started.Add(time.Duration(start) * time.Millisecond)
		step.Completed = step.Started.Add(time.Duration(dur) * time.Millisecond)
		step.Lines = parseLines(lines)
		if len(step.Lines) > 0 {
			step.Line = step.Lines[0].Start
		}
		step.ReceivedFrom = from.String
		step.Usage.CPU, step.Usage.Throttled = time.Duration(cpu)*time.Millisecond, time.Duration(thr)*time.Millisecond
		b.Steps = append(b.Steps, step)
	}
	return rows.Err()
}

// restore loads the definitions a build's steps reach and rebuilds its
// graph.
func (s *Store) restore(ctx context.Context, st *Stored) error {
	defs := map[string]history.Def{}
	var queue []int64
	for _, step := range st.Build.Steps {
		if step.Identity != "" {
			id, err := identity(step.Identity)
			if err != nil {
				return err
			}
			queue = append(queue, id)
		}
	}
	for len(queue) > 0 {
		batch := queue
		if len(batch) > 200 {
			batch = batch[:200]
		}
		queue = queue[len(batch):]
		var want []any
		for _, id := range batch {
			if _, ok := defs[identityString(id)]; !ok {
				want = append(want, id)
			}
		}
		if len(want) == 0 {
			continue
		}
		rows, err := s.db.QueryContext(ctx, "SELECT identity, op, inputs FROM step_defs WHERE identity IN (?"+strings.Repeat(", ?", len(want)-1)+")", want...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var op, inputs []byte
			if err := rows.Scan(&id, &op, &inputs); err != nil {
				rows.Close()
				return err
			}
			d := history.Def{Identity: identityString(id), Op: op}
			for i := 0; i+8 <= len(inputs); i += 8 {
				in := int64(binary.BigEndian.Uint64(inputs[i:]))
				d.Inputs = append(d.Inputs, identityString(in))
				if _, ok := defs[identityString(in)]; !ok {
					queue = append(queue, in)
				}
			}
			defs[d.Identity] = d
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	b, err := history.Restore(*st.Build, defs)
	if err != nil {
		return err
	}
	st.Build = b
	return nil
}

// Ping checks that the main database answers.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}
