package store

import (
	"context"
	"errors"
)

// Prune deletes what is past retention: builds (with their steps) older
// than Keep once their days are in the images' daily figures (Rollup),
// daily figures older than KeepDays, step definitions and Dockerfiles no build has used since,
// jobs and pipelines of that age, and output files older than KeepOutput
// days. Builds go in small batches so the writer is never held for long.
// It returns the number of builds deleted.
func (s *Store) Prune(ctx context.Context) (int64, error) {
	now := s.opts.Now()
	if err := s.Rollup(ctx); err != nil {
		// Builds stay until their days are summed up.
		return 0, err
	}
	cutoff := now.Add(-s.opts.Keep).UnixMilli()
	var deleted int64
	for {
		res, err := s.db.ExecContext(ctx, `DELETE FROM builds WHERE id IN (
			SELECT id FROM builds WHERE COALESCE(completed_at, started_at) < ? LIMIT 500)`, cutoff)
		if err != nil {
			return deleted, err
		}
		n, _ := res.RowsAffected()
		deleted += n
		if n == 0 {
			break
		}
	}
	var errs []error
	if _, err := s.db.ExecContext(ctx, "DELETE FROM image_days WHERE day < ?", dayOf(now)-int64(s.opts.KeepDays)); err != nil {
		errs = append(errs, err)
	}
	for _, q := range []string{
		"DELETE FROM step_defs WHERE last_seen < ?",
		"DELETE FROM route_decisions WHERE at < ?",
		"DELETE FROM dockerfiles WHERE last_seen < ? AND NOT EXISTS (SELECT 1 FROM builds WHERE dockerfile_id = dockerfiles.id)",
		// A job by the earliest time known of it, else when the console
		// first heard of it (a manual job never started has no other).
		"DELETE FROM jobs WHERE COALESCE(started_at, queued_at, finished_at, seen_at) < ?",
		// A pipeline by its creation, else its jobs' start, else when the
		// console first heard of it; never while builds of it stay.
		`DELETE FROM pipelines WHERE COALESCE(created_at,
				(SELECT MIN(COALESCE(j.started_at, j.queued_at)) FROM jobs j WHERE j.instance = pipelines.instance AND j.pipeline_id = pipelines.pipeline_id),
				seen_at) < ?
			AND NOT EXISTS (SELECT 1 FROM builds b WHERE b.instance = pipelines.instance AND b.pipeline_id = pipelines.pipeline_id)`,
	} {
		if _, err := s.db.ExecContext(ctx, q, cutoff); err != nil {
			errs = append(errs, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA incremental_vacuum"); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, s.pruneOutputs(now))
	return deleted, errors.Join(errs...)
}
