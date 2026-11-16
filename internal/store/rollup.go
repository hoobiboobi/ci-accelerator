package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
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
	WHEN b.reason_kind IN ` + unclearKinds + ` OR b.built > 0 THEN 'unclear'
	ELSE 'cached' END`

// Rollup sums every finished UTC day's builds per image into the daily
// figures, from the last few days summed up before (or the oldest build)
// to yesterday.
func (s *Store) Rollup(ctx context.Context) error {
	today := dayOf(s.opts.Now())
	last, ok, err := s.rolledUp(ctx)
	if err != nil {
		return err
	}
	// Days summed up again must still have all their builds: retention
	// cuts into the day of its cutoff. New days are summed up anyway.
	from := max(last-rollupRedo+1, min(last+1, dayOf(s.opts.Now().Add(-s.opts.Keep))+1))
	if !ok {
		var first sql.NullInt64
		if err := s.db.QueryRowContext(ctx, "SELECT min(started_at) FROM builds WHERE project_id IS NOT NULL").Scan(&first); err != nil {
			return err
		}
		if !first.Valid {
			return nil
		}
		from = first.Int64 / dayMs
	}
	if err := s.rollupDays(ctx, from, today); err != nil {
		return err
	}
	if today-1 <= last && ok {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO meta (name, value) VALUES ('rollup_last', ?)
		ON CONFLICT (name) DO UPDATE SET value = excluded.value`, strconv.FormatInt(today-1, 10))
	return err
}

// rolledUp returns the last day Rollup summed up (ok false before the
// first). A single day summed up again elsewhere (SettleElsewhere) does
// not count: the days before it may not be summed yet. A store that never
// kept it takes the latest day summed up. A release rolled back to sums
// days up without keeping it: the latest day summed up that is past
// retention, which only Rollup writes, catches up with it.
func (s *Store) rolledUp(ctx context.Context) (last int64, ok bool, err error) {
	var kept sql.NullString
	err = s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE name = 'rollup_last'").Scan(&kept)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	var day sql.NullInt64
	floor := dayOf(s.opts.Now().Add(-s.opts.Keep))
	if !kept.Valid {
		floor = math.MaxInt64
	}
	if err := s.db.QueryRowContext(ctx, "SELECT max(day) FROM image_days WHERE day <= ?", floor).Scan(&day); err != nil {
		return 0, false, err
	}
	if kept.Valid {
		v, err := strconv.ParseInt(kept.String, 10, 64)
		if err != nil {
			return 0, false, fmt.Errorf("store: rollup_last %q: %w", kept.String, err)
		}
		return max(v, day.Int64), true, nil
	}
	return day.Int64, day.Valid, nil
}

// rollupDays sums up the days from (included) to to (excluded) again,
// never a day up to rollup_floor: the size bound (pruneToSize) deleted
// builds of those days after they were summed up.
func (s *Store) rollupDays(ctx context.Context, from, to int64) error {
	var floor sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT CAST(value AS INTEGER) FROM meta WHERE name = 'rollup_floor'").Scan(&floor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if floor.Valid {
		from = max(from, floor.Int64+1)
	}
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
			Percentile(a.cached, 50).Milliseconds(), Percentile(a.rebuilds, 50).Milliseconds(), Percentile(a.rebuilds, 95).Milliseconds()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Percentile is the p-th percentile of ds by nearest rank: always one of
// the durations itself (a build a person can find), the lower middle one
// for p=50 of an even count; 0 for none. Every median and p95 the console
// shows comes from here, so two pages never disagree about one set.
func Percentile(ds []time.Duration, p int) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	x := append([]time.Duration(nil), ds...)
	sort.Slice(x, func(i, j int) bool { return x[i] < x[j] })
	i := (len(x)*p + 99) / 100
	return x[max(0, min(len(x)-1, i-1))]
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
