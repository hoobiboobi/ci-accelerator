package store

import (
	"context"
	"database/sql"
	"sort"
	"time"
)

// ImageDay is one image's builds of one UTC day.
type ImageDay struct {
	Day                                 time.Time
	Builds                              int
	Cached, Change, Farm, Unclear, Fail int
	Built                               time.Duration // executed steps' time
	CachedP50                           time.Duration
	RebuiltP50, RebuiltP95              time.Duration
}

// Rebuilt is how many builds executed steps or failed.
func (d ImageDay) Rebuilt() int { return d.Builds - d.Cached }

const dayMs = 24 * 60 * 60 * 1000

func dayOf(t time.Time) int64 { return t.UnixMilli() / dayMs }

// rollupRedo is how many finished days are summed up again on every
// rollup: a daemon's history can bring older builds in late.
const rollupRedo = 3

// statusCase names a build's outcome as the pages do (running builds are
// left out by the query).
var statusCase = `CASE
	WHEN b.error != '' THEN 'failed'
	WHEN b.reason_kind IN ` + changeKinds + ` THEN 'change'
	WHEN b.reason_kind IN ` + farmKinds + ` THEN 'farm'
	WHEN b.reason_kind = 'cache-elsewhere' OR b.built > 0 THEN 'unclear'
	ELSE 'cached' END`

// Rollup sums every finished UTC day's builds per image into the daily
// figures, from the last few days summed up before (or the oldest build)
// to yesterday.
func (s *Store) Rollup(ctx context.Context) error {
	today := dayOf(s.opts.Now())
	var last, first sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT max(day) FROM image_days").Scan(&last); err != nil {
		return err
	}
	// Days summed up again must still have all their builds: retention
	// cuts into the day of its cutoff. New days are summed up anyway.
	from := max(last.Int64-rollupRedo+1, min(last.Int64+1, dayOf(s.opts.Now().Add(-s.opts.Keep))+1))
	if !last.Valid {
		if err := s.db.QueryRowContext(ctx, "SELECT min(started_at) FROM builds WHERE project_id IS NOT NULL").Scan(&first); err != nil {
			return err
		}
		if !first.Valid {
			return nil
		}
		from = first.Int64 / dayMs
	}
	return s.rollupDays(ctx, from, today)
}

// rollupDays sums up the days from (included) to to (excluded) again.
func (s *Store) rollupDays(ctx context.Context, from, to int64) error {
	if from >= to {
		return nil
	}
	today := to
	rows, err := s.db.QueryContext(ctx, `SELECT b.instance, b.project_id, b.target, b.started_at / 86400000,
			`+statusCase+`, b.completed_at - b.started_at, b.built_ms
		FROM builds b
		WHERE b.completed_at IS NOT NULL AND b.project_id IS NOT NULL AND b.instance IS NOT NULL
			AND b.started_at >= ? AND b.started_at < ?`, from*dayMs, today*dayMs)
	if err != nil {
		return err
	}
	type key struct {
		instance, project int64
		target            string
		day               int64
	}
	type acc struct {
		counts           map[string]int
		builtMs          int64
		cached, rebuilds []time.Duration
	}
	days := map[key]*acc{}
	for rows.Next() {
		var (
			k       key
			status  string
			dur, ms int64
		)
		if err := rows.Scan(&k.instance, &k.project, &k.target, &k.day, &status, &dur, &ms); err != nil {
			rows.Close()
			return err
		}
		a := days[k]
		if a == nil {
			a = &acc{counts: map[string]int{}}
			days[k] = a
		}
		a.counts[status]++
		a.builtMs += ms
		d := time.Duration(dur) * time.Millisecond
		switch status {
		case "cached":
			a.cached = append(a.cached, d)
		case "failed":
		default:
			a.rebuilds = append(a.rebuilds, d)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM image_days WHERE day >= ? AND day < ?", from, today); err != nil {
		return err
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO image_days (instance, project_id, target, day, builds,
		cached, change, farm, unclear, failed, built_ms, cached_p50_ms, rebuilt_p50_ms, rebuilt_p95_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for k, a := range days {
		c := a.counts
		n := c["cached"] + c["change"] + c["farm"] + c["unclear"] + c["failed"]
		if _, err := ins.ExecContext(ctx, k.instance, k.project, k.target, k.day, n,
			c["cached"], c["change"], c["farm"], c["unclear"], c["failed"], a.builtMs,
			pct(a.cached, 50).Milliseconds(), pct(a.rebuilds, 50).Milliseconds(), pct(a.rebuilds, 95).Milliseconds()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// pct is the p-th percentile of ds (nearest rank); 0 for none.
func pct(ds []time.Duration, p int) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	i := (len(ds)*p + 99) / 100
	return ds[max(0, min(len(ds)-1, i-1))]
}

// ImageDays returns an image's daily figures since t, oldest first.
func (s *Store) ImageDays(ctx context.Context, p ProjectKey, target string, since time.Time) ([]ImageDay, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT day, builds, cached, change, farm, unclear, failed, built_ms,
			cached_p50_ms, rebuilt_p50_ms, rebuilt_p95_ms
		FROM image_days WHERE instance = ? AND project_id = ? AND target = ? AND day >= ? ORDER BY day`,
		nullInstance(p.Instance), nullUint(p.ID), target, dayOf(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImageDay
	for rows.Next() {
		var (
			d                         ImageDay
			day, built, c50, r50, r95 int64
		)
		if err := rows.Scan(&day, &d.Builds, &d.Cached, &d.Change, &d.Farm, &d.Unclear, &d.Fail, &built, &c50, &r50, &r95); err != nil {
			return nil, err
		}
		ms := func(v int64) time.Duration { return time.Duration(v) * time.Millisecond }
		d.Day = time.UnixMilli(day * dayMs).UTC()
		d.Built, d.CachedP50, d.RebuiltP50, d.RebuiltP95 = ms(built), ms(c50), ms(r50), ms(r95)
		out = append(out, d)
	}
	return out, rows.Err()
}
