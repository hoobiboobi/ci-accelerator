package store

import (
	"context"
	"database/sql"
	"time"
)

// CacheObject is one archive of the runners' S3 cache.
type CacheObject struct {
	Store     string
	ProjectID uint64
	Key       string
	Bytes     int64
	Modified  time.Time
}

// CacheStore is a bucket folder as the console last saw it.
type CacheStore struct {
	ID      string
	Scanned time.Time
	// Lifecycle: the bucket expires old caches; nil: not known.
	Lifecycle *bool
	Err       string
}

// ReplaceCacheScan stores what a store's folder holds now, in place of what
// it held. With an error the objects are kept and the error recorded.
func (s *Store) ReplaceCacheScan(ctx context.Context, store, instance string, at time.Time, objects []CacheObject, lifecycle *bool, scanErr error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	msg := ""
	if scanErr != nil {
		msg = scanErr.Error()
	} else {
		if _, err := tx.ExecContext(ctx, `DELETE FROM cache_objects WHERE store = ?`, store); err != nil {
			return err
		}
		// One key may come several times: runners with Shared = false each
		// keep their own archive of it. Every one takes room, so the key
		// counts them all and is as new as the newest.
		for _, o := range objects {
			if _, err := tx.ExecContext(ctx, `INSERT INTO cache_objects (store, instance, project_id, key, bytes, modified_at) VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT (store, project_id, key) DO UPDATE SET bytes = cache_objects.bytes + excluded.bytes,
				modified_at = CASE WHEN excluded.modified_at > cache_objects.modified_at THEN excluded.modified_at ELSE cache_objects.modified_at END`,
				store, nullInstance(instance), o.ProjectID, o.Key, o.Bytes, o.Modified.UnixMilli()); err != nil {
				return err
			}
		}
	}
	var lc sql.NullInt64
	if lifecycle != nil {
		lc = sql.NullInt64{Valid: true}
		if *lifecycle {
			lc.Int64 = 1
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO cache_stores (store, scanned_at, lifecycle, error) VALUES (?, ?, ?, ?)
		ON CONFLICT (store) DO UPDATE SET scanned_at = excluded.scanned_at, lifecycle = COALESCE(excluded.lifecycle, cache_stores.lifecycle), error = excluded.error`,
		store, at.UnixMilli(), lc, msg); err != nil {
		return err
	}
	return tx.Commit()
}

// KeepCacheStores forgets every store not in ids, with its archives: a
// folder no longer configured is no longer shown.
func (s *Store) KeepCacheStores(ctx context.Context, ids []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	in := "''"
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
		if i == 0 {
			in = "?"
		} else {
			in += ", ?"
		}
	}
	for _, table := range []string{"cache_objects", "cache_stores"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE store NOT IN ("+in+")", args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ProjectCache returns a project's cache archives, largest first, and the
// state of the stores that hold them (or of every store, when none does).
func (s *Store) ProjectCache(ctx context.Context, p ProjectKey) ([]CacheObject, []CacheStore, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT store, project_id, key, bytes, modified_at FROM cache_objects
		WHERE instance = ? AND project_id = ? ORDER BY bytes DESC, key`, nullInstance(p.Instance), p.ID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var objs []CacheObject
	for rows.Next() {
		var o CacheObject
		var mod int64
		if err := rows.Scan(&o.Store, &o.ProjectID, &o.Key, &o.Bytes, &mod); err != nil {
			return nil, nil, err
		}
		o.Modified = time.UnixMilli(mod).UTC()
		objs = append(objs, o)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	srows, err := s.db.QueryContext(ctx, `SELECT store, scanned_at, lifecycle, error FROM cache_stores ORDER BY store`)
	if err != nil {
		return nil, nil, err
	}
	defer srows.Close()
	var stores []CacheStore
	for srows.Next() {
		var c CacheStore
		var at, lc sql.NullInt64
		if err := srows.Scan(&c.ID, &at, &lc, &c.Err); err != nil {
			return nil, nil, err
		}
		c.Scanned = msTime(at)
		if lc.Valid {
			v := lc.Int64 == 1
			c.Lifecycle = &v
		}
		stores = append(stores, c)
	}
	return objs, stores, srows.Err()
}
