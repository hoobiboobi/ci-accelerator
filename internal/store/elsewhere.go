package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// Elsewhere is a build whose first rebuilt step's cache was on another
// daemon, where it is not known whether its files changed: the commit it
// built and the commit of the image's previous build.
type Elsewhere struct {
	ID      int64
	Started time.Time
	SHA     string // this build's commit; empty when not known
	PrevSHA string // the previous build's; empty when not known
	Paths   []string
	// Old is the daemon of the image's previous build when it was another
	// one; empty when the previous build ran on the same daemon. It decides
	// whether a no-change outcome is "other node" or an evicted cache.
	Old string
}

// Compared returns GitLab's comparison of two commits as stored by
// SaveCompare: the changed files and whether the list is complete; ok is
// false when it was not stored.
func (s *Store) Compared(ctx context.Context, p ProjectKey, from, to string) (files []string, complete, ok bool, err error) {
	var raw string
	err = s.db.QueryRowContext(ctx, `SELECT files, complete FROM compares WHERE instance = ? AND project_id = ? AND from_sha = ? AND to_sha = ?`,
		nullInstance(p.Instance), nullUint(p.ID), from, to).Scan(&raw, &complete)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	err = json.Unmarshal([]byte(raw), &files)
	return files, complete, err == nil, err
}

// SaveCompare stores GitLab's comparison of two commits of a project.
func (s *Store) SaveCompare(ctx context.Context, p ProjectKey, from, to string, files []string, complete bool) error {
	raw, _ := json.Marshal(nonNil(files))
	_, err := s.db.ExecContext(ctx, `INSERT INTO compares (instance, project_id, from_sha, to_sha, files, complete, at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (instance, project_id, from_sha, to_sha) DO UPDATE SET files = excluded.files, complete = excluded.complete, at = excluded.at`,
		nullInstance(p.Instance), nullUint(p.ID), from, to, string(raw), complete, s.opts.Now().UnixMilli())
	return err
}

// ElsewhereProjects returns the projects with builds since t whose reason
// is CacheElsewhere and that nothing has settled yet.
func (s *Store) ElsewhereProjects(ctx context.Context, since time.Time) ([]ProjectKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT instance, project_id FROM builds INDEXED BY builds_elsewhere
		WHERE reason_kind = 'cache-elsewhere' AND reason_via = '' AND started_at >= ? AND instance IS NOT NULL AND project_id IS NOT NULL`,
		since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectKey
	for rows.Next() {
		var instance int64
		var project uint64
		if err := rows.Scan(&instance, &project); err != nil {
			return nil, err
		}
		out = append(out, ProjectKey{Instance: fmt.Sprintf("%06x", instance), ID: project})
	}
	return out, rows.Err()
}

// Elsewhere returns a project's builds since t whose reason is
// CacheElsewhere and that nothing has settled yet, newest first. PrevSHA is
// the commit of the build the reason compared it with (Previous): for an
// image without a target, its own repository's previous build, not the
// latest of the project's other Dockerfiles.
func (s *Store) Elsewhere(ctx context.Context, p ProjectKey, since time.Time, limit int) ([]Elsewhere, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT b.id, b.started_at, COALESCE(p.sha, ''), b.reason_paths, b.reason_old,
			b.ref, b.target, b.images, b.pipeline_id
		FROM builds b INDEXED BY builds_elsewhere
		LEFT JOIN pipelines p ON p.instance = b.instance AND p.pipeline_id = b.pipeline_id
		WHERE b.instance = ? AND b.project_id = ? AND b.started_at >= ?
			AND b.reason_kind = 'cache-elsewhere' AND b.reason_via = ''
		ORDER BY b.started_at DESC LIMIT ?`,
		nullInstance(p.Instance), nullUint(p.ID), since.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	var (
		out    []Elsewhere
		builds []history.Build
	)
	for rows.Next() {
		var (
			e             Elsewhere
			b             history.Build
			started       int64
			paths, images string
			pipeline      sql.NullInt64
		)
		if err := rows.Scan(&e.ID, &started, &e.SHA, &paths, &e.Old, &b.Ref, &b.Target, &images, &pipeline); err != nil {
			rows.Close()
			return nil, err
		}
		e.Started = time.UnixMilli(started).UTC()
		_ = json.Unmarshal([]byte(paths), &e.Paths)
		b.Started, b.Attribution.Pipeline = e.Started, uint64(pipeline.Int64)
		_ = json.Unmarshal([]byte(images), &b.Images)
		out = append(out, e)
		builds = append(builds, b)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		prev, err := s.sameImage(ctx, nullInstance(p.Instance), nullUint(p.ID), &builds[i], false)
		if err != nil {
			return nil, err
		}
		if prev == 0 {
			continue
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(p.sha, '') FROM builds b
			LEFT JOIN pipelines p ON p.instance = b.instance AND p.pipeline_id = b.pipeline_id
			WHERE b.id = ?`, prev).Scan(&out[i].PrevSHA); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Settled is what became of an Elsewhere build: FilesChanged (Paths are
// the changed files it copies), OtherNode, or still CacheElsewhere when
// nothing could tell; Via says who decided it.
type Settled struct {
	ID    int64
	Kind  history.ReasonKind
	Paths []string
	Via   string
	// Asked: the answer is to the question Elsewhere listed (its previous
	// daemon Old and paths Open); a build saved again since with another
	// question keeps it open.
	Asked bool
	Old   string
	Open  []string
}

// SettleElsewhere stores what became of Elsewhere builds and sums their
// finished days up again. A build stored again since keeps its new reason.
func (s *Store) SettleElsewhere(ctx context.Context, settled []Settled) error {
	if len(settled) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	days := map[int64]bool{}
	var keys []string
	for _, st := range settled {
		var (
			kind  = string(st.Kind)
			paths sql.NullString
		)
		if st.Kind == history.FilesChanged {
			b, _ := json.Marshal(nonNil(st.Paths))
			paths = sql.NullString{String: string(b), Valid: true}
		}
		var started int64
		var instance, pipeline sql.NullInt64
		open, _ := json.Marshal(nonNil(st.Open))
		err := tx.QueryRowContext(ctx, `UPDATE builds SET reason_kind = ?, reason_via = ?,
				reason_paths = COALESCE(?, reason_paths)
			WHERE id = ? AND reason_kind = 'cache-elsewhere' AND reason_via = ''
				AND (NOT ? OR (reason_old = ? AND reason_paths = ?))
			RETURNING started_at, instance, pipeline_id`, kind, st.Via, paths, st.ID, st.Asked, st.Old, string(open)).Scan(&started, &instance, &pipeline)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return err
		}
		days[started/dayMs] = true
		if instance.Valid && pipeline.Valid {
			keys = append(keys, pipelineKey(instance, pipeline))
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, k := range keys {
		s.changed(k)
	}
	today := dayOf(s.opts.Now())
	floor := dayOf(s.opts.Now().Add(-s.opts.Keep)) + 1
	for d := range days {
		if d < today && d >= floor {
			if err := s.rollupDays(ctx, d, d+1); err != nil {
				return err
			}
		}
	}
	return nil
}
