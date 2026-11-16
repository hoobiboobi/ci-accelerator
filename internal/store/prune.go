package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"modernc.org/sqlite"
)

// pruneBatch is how many builds one delete of Prune takes; pruneGap is the
// pause between two, in which the other writers (the collector) get the
// lock: SQLite's busy handler is not fair, and a loop without a pause
// takes it again first.
const (
	pruneBatch = 500
	pruneGap   = 20 * time.Millisecond
)

// Prune deletes what is past retention: output files older than KeepOutput
// days, builds (with their steps) older than Keep once their days are in
// the images' daily figures (Rollup), daily figures older than KeepDays,
// step definitions and Dockerfiles no build has used since, jobs and
// pipelines of that age without builds, and projects nothing is kept of.
// Builds go in small batches so the writer is never held for long. It
// returns the number of builds deleted. It also says when the store's
// volume is nearly full (lowSpace).
//
// With Options.MaxBytes it then deletes the oldest builds until the
// database fits (pruneToSize), and says so.
//
// On a full volume it still frees what it can: the output files go first
// (deleting a file needs no room), a failing rollup keeps only the builds
// of days it has not summed, and a delete that finds no room is retried
// with fewer builds after the write-ahead log is emptied.
func (s *Store) Prune(ctx context.Context) (int64, error) {
	now := s.opts.Now()
	errs := []error{s.pruneOutputs(now)}
	cutoff := now.Add(-s.opts.Keep).UnixMilli()
	// Builds stay until their days are summed up.
	summed := int64(math.MaxInt64)
	if err := s.Rollup(ctx); err != nil {
		errs = append(errs, err)
		last, ok, err := s.rolledUp(ctx)
		switch {
		case err != nil:
			errs = append(errs, err)
			summed = math.MinInt64
		case !ok:
			summed = math.MinInt64
		default:
			summed = (last + 1) * dayMs
		}
	}
	deleted, err := s.pruneBuilds(ctx, cutoff, summed)
	if err != nil {
		errs = append(errs, err)
	}
	if err == nil {
		n, err := s.pruneToSize(ctx, min(summed, dayOf(now)*dayMs))
		deleted += n
		errs = append(errs, err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM image_days WHERE day < ?", dayOf(now)-int64(s.opts.KeepDays)); err != nil {
		errs = append(errs, err)
	}
	for _, q := range []string{
		"DELETE FROM step_defs WHERE last_seen < ?",
		"DELETE FROM route_decisions WHERE at < ?",
		"DELETE FROM compares WHERE at < ?",
		"DELETE FROM dockerfiles WHERE last_seen < ? AND NOT EXISTS (SELECT 1 FROM builds WHERE dockerfile_id = dockerfiles.id)",
		// A job by the earliest time known of it, else when the console
		// first heard of it (a manual job never started has no other);
		// never while builds of it stay.
		`DELETE FROM jobs WHERE COALESCE(started_at, queued_at, finished_at, seen_at) < ?
			AND NOT EXISTS (SELECT 1 FROM builds b WHERE b.instance = jobs.instance AND b.job_id = jobs.job_id)`,
		// A pipeline by its creation, else its jobs' start, else when the
		// console first heard of it; never while builds of it stay.
		`DELETE FROM pipelines WHERE COALESCE(created_at,
				(SELECT MIN(COALESCE(j.started_at, j.queued_at)) FROM jobs j WHERE j.instance = pipelines.instance AND j.pipeline_id = pipelines.pipeline_id),
				seen_at) < ?
			AND NOT EXISTS (SELECT 1 FROM builds b WHERE b.instance = pipelines.instance AND b.pipeline_id = pipelines.pipeline_id)`,
		// A project leaves the project menu once nothing of it is kept: no
		// build, pipeline, daily figure or cache archive.
		`DELETE FROM projects WHERE COALESCE(last_at, 0) < ?
			AND NOT EXISTS (SELECT 1 FROM builds b WHERE b.instance = projects.instance AND b.project_id = projects.project_id)
			AND NOT EXISTS (SELECT 1 FROM pipelines p WHERE p.instance = projects.instance AND p.project_id = projects.project_id)
			AND NOT EXISTS (SELECT 1 FROM image_days d WHERE d.instance = projects.instance AND d.project_id = projects.project_id)
			AND NOT EXISTS (SELECT 1 FROM cache_objects c WHERE c.instance = projects.instance AND c.project_id = projects.project_id)`,
	} {
		if _, err := s.db.ExecContext(ctx, q, cutoff); err != nil {
			errs = append(errs, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA incremental_vacuum"); err != nil {
		errs = append(errs, err)
	}
	// Statistics for the planner, now that the tables changed (see Open).
	if _, err := s.db.ExecContext(ctx, "PRAGMA optimize"); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, s.lowSpace())
	return deleted, errors.Join(errs...)
}

// pruneBuildsQuery deletes a batch of builds (?3) that ended before ?1 and
// started before ?2. started_at first: a build ends after it starts, and
// the index on it spares a scan of every build.
const pruneBuildsQuery = `DELETE FROM builds WHERE id IN (
	SELECT id FROM builds WHERE started_at < ?1 AND started_at < ?2 AND COALESCE(completed_at, started_at) < ?1 LIMIT ?3)`

// pruneBuilds deletes the builds that ended before cutoff and started
// before summed, in batches.
func (s *Store) pruneBuilds(ctx context.Context, cutoff, summed int64) (int64, error) {
	var deleted int64
	batch, retried, full := pruneBatch, false, false
	for {
		res, err := s.db.ExecContext(ctx, pruneBuildsQuery, cutoff, summed, batch)
		if isFull(err) && !retried {
			// A delete needs room in the write-ahead log for every page it
			// touches: empty the log and take fewer builds at once. Only
			// when that does not help either is the volume too full.
			_, _ = s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
			batch, retried, full = max(1, batch/10), true, true
			continue
		}
		if err != nil {
			return deleted, err
		}
		retried = false
		if full {
			// Keep the log empty: its room is all there is.
			_, _ = s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
		}
		n, _ := res.RowsAffected()
		deleted += n
		if n < int64(batch) {
			return deleted, nil
		}
		select {
		case <-ctx.Done():
			return deleted, ctx.Err()
		case <-time.After(pruneGap):
		}
	}
}

// pruneToSize deletes the oldest builds that started before summed while
// the database holds more than Options.MaxBytes, down to nine tenths of it
// (a store at its bound is not pruned again every hour), with the
// Dockerfiles and step definitions only they used. The days it cuts into
// are not summed up again (rollup_floor): their daily figures stay as
// summed with all their builds. The error says what was deleted, or that
// the bound is not kept.
func (s *Store) pruneToSize(ctx context.Context, summed int64) (int64, error) {
	bound := s.opts.MaxBytes
	if bound <= 0 {
		return 0, nil
	}
	used, err := s.dbUsed(ctx)
	if err != nil || used <= bound {
		return 0, err
	}
	start, target := used, bound/10*9
	var deleted, newest int64
	for used > target {
		n, last, err := s.deleteOldest(ctx, summed)
		if err != nil {
			return deleted, err
		}
		if n == 0 {
			break
		}
		deleted, newest = deleted+n, max(newest, last)
		if _, err := s.db.ExecContext(ctx, "DELETE FROM dockerfiles WHERE NOT EXISTS (SELECT 1 FROM builds WHERE dockerfile_id = dockerfiles.id)"); err != nil {
			return deleted, err
		}
		if used, err = s.dbUsed(ctx); err != nil {
			return deleted, err
		}
		select {
		case <-ctx.Done():
			return deleted, ctx.Err()
		case <-time.After(pruneGap):
		}
	}
	if deleted > 0 {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM step_defs WHERE NOT EXISTS (SELECT 1 FROM steps WHERE steps.identity = step_defs.identity)"); err != nil {
			return deleted, err
		}
		if used, err = s.dbUsed(ctx); err != nil {
			return deleted, err
		}
	}
	if used > bound {
		return deleted, fmt.Errorf("store: the database holds %d MiB, over its bound of %d MiB, after deleting %d builds early: the rest started on days not summed up yet (raise the bound, or keep builds for fewer days)",
			used>>20, bound>>20, deleted)
	}
	return deleted, fmt.Errorf("store: the database held %d MiB, over its bound of %d MiB: %d builds deleted before their retention ended, the oldest first, up to %s (their days' figures stay as summed up)",
		start>>20, bound>>20, deleted, time.UnixMilli(newest).UTC().Format(time.DateTime))
}

// deleteOldest deletes a batch of the oldest builds that started before
// summed and returns how many and when the newest of them started. Its
// day becomes rollup_floor in the same transaction.
func (s *Store) deleteOldest(ctx context.Context, summed int64) (n, newest int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `DELETE FROM builds WHERE id IN (
		SELECT id FROM builds INDEXED BY builds_started WHERE started_at < ? ORDER BY started_at, id LIMIT ?) RETURNING started_at`, summed, pruneBatch)
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var at int64
		if err := rows.Scan(&at); err != nil {
			rows.Close()
			return 0, 0, err
		}
		n, newest = n+1, max(newest, at)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, 0, err
	}
	if n == 0 {
		return 0, 0, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta (name, value) VALUES ('rollup_floor', ?1)
		ON CONFLICT (name) DO UPDATE SET value = max(CAST(value AS INTEGER), ?1)`, newest/dayMs); err != nil {
		return 0, 0, err
	}
	return n, newest, tx.Commit()
}

// dbUsed is the bytes of the database's pages in use.
func (s *Store) dbUsed(ctx context.Context) (int64, error) {
	var used int64
	err := s.db.QueryRowContext(ctx, `SELECT (p.page_count - f.freelist_count) * z.page_size
		FROM pragma_page_count() p, pragma_freelist_count() f, pragma_page_size() z`).Scan(&used)
	return used, err
}

// sqliteFull is SQLite's result code for a full disk (SQLITE_FULL).
const sqliteFull = 13

// isFull reports whether err is SQLite's "database or disk is full".
func isFull(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code()&0xff == sqliteFull
}

// Below lowSpaceShare of its volume, or lowSpaceBytes, free, the store
// warns: once the volume is full no build is stored any more.
const (
	lowSpaceShare = 0.05
	lowSpaceBytes = 256 << 20
)

// lowSpace returns an error naming the store's free space when its volume
// is nearly full; nil otherwise, or when the space is not known.
func (s *Store) lowSpace() error {
	free, total, err := volumeSpace(s.dir)
	if err != nil || total == 0 {
		return nil
	}
	if free >= lowSpaceBytes && float64(free) >= lowSpaceShare*float64(total) {
		return nil
	}
	return fmt.Errorf("store: the volume of %s is nearly full: %d MiB free of %d MiB; builds are no longer stored once it is full (enlarge it, or keep builds for fewer days)",
		s.dir, free>>20, total>>20)
}
