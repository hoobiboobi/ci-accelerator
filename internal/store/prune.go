package store

import (
	"context"
	"errors"
)

// Prune deletes what is past retention: builds (with their steps) older
// than Keep, step definitions and Dockerfiles no build has used since,
// jobs and pipelines of that age, and output files older than KeepOutput
// days. Builds go in small batches so the writer is never held for long.
// It returns the number of builds deleted.
func (s *Store) Prune(ctx context.Context) (int64, error) {
	now := s.opts.Now()
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
	for _, q := range []string{
		"DELETE FROM step_defs WHERE last_seen < ?",
		"DELETE FROM dockerfiles WHERE last_seen < ? AND NOT EXISTS (SELECT 1 FROM builds WHERE dockerfile_id = dockerfiles.id)",
		"DELETE FROM jobs WHERE started_at < ?",
		"DELETE FROM pipelines WHERE created_at < ?",
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
