package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// PreviousStarted returns when the build Previous would return for b
// started, without restoring it (Settle needs only the time); ok is false
// when b has no previous build, or its project is not known yet. Every
// build that started then or earlier ran before what b's comparison calls
// the change: a step one of them executed left the cache a witness of b's
// pipeline may legitimately find.
func (s *Store) PreviousStarted(ctx context.Context, b *history.Build) (time.Time, bool, error) {
	instance, job := nullInstance(b.Attribution.Instance), nullUint(b.Attribution.Job)
	if !instance.Valid || !job.Valid {
		return time.Time{}, false, nil
	}
	var project sql.NullInt64
	err := s.db.QueryRowContext(ctx, jobProject, instance, job, nullUint(b.Attribution.Pipeline)).Scan(&project)
	if err == sql.ErrNoRows || (err == nil && !project.Valid) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	id, err := s.sameImage(ctx, instance, project, b, false)
	if err != nil || id == 0 {
		return time.Time{}, false, err
	}
	var started int64
	if err := s.db.QueryRowContext(ctx, `SELECT started_at FROM builds WHERE id = ?`, id).Scan(&started); err != nil {
		return time.Time{}, false, err
	}
	return time.UnixMilli(started).UTC(), true, nil
}
