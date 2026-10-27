package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/cacheuse"
)

// retryMatch is how long a layer nobody owns is tried again: its build is
// stored when it completes, after the record shows up in a listing.
const retryMatch = 2 * time.Hour

// CacheSnapshot takes a node's cache records as its daemon lists them now:
// new layers are attributed to the builds that made them, sizes and use
// are kept up to date, records gone are forgotten.
func (s *Store) CacheSnapshot(ctx context.Context, node string, recs []cacheuse.Record) error {
	if node == "" {
		return nil
	}
	now := s.opts.Now()
	type known struct {
		size, lastUsed int64
		inUse, owned   bool
		created        int64
		matched        sql.NullInt64
	}
	have := map[string]known{}
	rows, err := s.db.QueryContext(ctx, `SELECT id, size, COALESCE(last_used, 0), in_use, owners != '[]', created_at, matched_at
		FROM cache_records WHERE node = ?`, node)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var k known
		if err := rows.Scan(&id, &k.size, &k.lastUsed, &k.inUse, &k.owned, &k.created, &k.matched); err != nil {
			rows.Close()
			return err
		}
		have[id] = k
	}
	if err := rows.Close(); err != nil {
		return err
	}
	// The layers to attribute: new ones, and young ones nobody owns yet.
	var todo []cacheuse.Record
	var from, to time.Time
	for _, r := range recs {
		k, ok := have[r.ID]
		if cacheuse.KindOf(r) != cacheuse.Layer || (ok && (k.owned || now.Sub(r.Created) > retryMatch)) {
			continue
		}
		if ok && k.matched.Valid && now.Sub(time.UnixMilli(k.matched.Int64)) < time.Minute {
			continue
		}
		todo = append(todo, r)
		if from.IsZero() || r.Created.Before(from) {
			from = r.Created
		}
		if r.Created.After(to) {
			to = r.Created
		}
	}
	var steps []cacheuse.Step
	if len(todo) > 0 {
		if steps, err = s.executedSteps(ctx, node, from.Add(-cacheuse.Slack), to.Add(cacheuse.Slack)); err != nil {
			return err
		}
	}
	owners := map[string][]cacheuse.Owner{}
	for _, r := range todo {
		owners[r.ID] = cacheuse.Match(r, steps)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ins, err := tx.PrepareContext(ctx, `INSERT INTO cache_records (node, id, kind, description, size, created_at, last_used,
		in_use, parents, instance, project_id, target, owners, matched_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (node, id) DO UPDATE SET size = excluded.size, last_used = excluded.last_used, in_use = excluded.in_use,
			instance = COALESCE(excluded.instance, cache_records.instance),
			project_id = COALESCE(excluded.project_id, cache_records.project_id),
			target = CASE WHEN excluded.matched_at IS NULL THEN cache_records.target ELSE excluded.target END,
			owners = CASE WHEN excluded.matched_at IS NULL THEN cache_records.owners ELSE excluded.owners END,
			matched_at = COALESCE(excluded.matched_at, cache_records.matched_at)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	seen := make(map[string]bool, len(recs))
	for _, r := range recs {
		seen[r.ID] = true
		k, ok := have[r.ID]
		o, tried := owners[r.ID]
		lastUsed := sql.NullInt64{}
		if !r.LastUsed.IsZero() {
			lastUsed = sql.NullInt64{Int64: r.LastUsed.UnixMilli(), Valid: true}
		}
		if ok && !tried && k.size == r.Size && k.lastUsed == lastUsed.Int64 && k.inUse == r.InUse {
			continue
		}
		var instance, project sql.NullInt64
		target, list, matched := "", "[]", sql.NullInt64{}
		if tried {
			matched = sql.NullInt64{Int64: now.UnixMilli(), Valid: true}
			if len(o) == 1 {
				instance, project, target = nullInstance(o[0].Instance), nullUint(o[0].Project), o[0].Target
			}
			if len(o) > 0 {
				b, _ := json.Marshal(o)
				list = string(b)
			}
		}
		parents, _ := json.Marshal(r.Parents)
		if r.Parents == nil {
			parents = []byte("[]")
		}
		if _, err := ins.ExecContext(ctx, node, r.ID, string(cacheuse.KindOf(r)), r.Description, r.Size, r.Created.UnixMilli(),
			lastUsed, r.InUse, string(parents), instance, project, target, list, matched); err != nil {
			return err
		}
	}
	for id := range have {
		if !seen[id] {
			if _, err := tx.ExecContext(ctx, "DELETE FROM cache_records WHERE node = ? AND id = ?", node, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// executedSteps lists the steps builds on node executed between from and
// to, with the builds' projects.
func (s *Store) executedSteps(ctx context.Context, node string, from, to time.Time) ([]cacheuse.Step, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.id, b.instance, b.project_id, b.target, s.key, COALESCE(s.name, sd.name, ''), b.started_at + s.start_ms, s.dur_ms
		FROM builds b JOIN daemons d ON d.id = b.daemon_id
			JOIN steps s ON s.build_id = b.id LEFT JOIN step_defs sd ON sd.identity = s.identity
		WHERE d.node = ? AND b.project_id IS NOT NULL AND b.instance IS NOT NULL AND s.state = 1
			AND b.started_at <= ? AND COALESCE(b.completed_at, ?) >= ?`,
		node, to.UnixMilli(), to.UnixMilli(), from.UnixMilli()-int64(time.Hour/time.Millisecond))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cacheuse.Step
	for rows.Next() {
		var st cacheuse.Step
		var instance int64
		var start, dur int64
		if err := rows.Scan(&st.Owner.Build, &instance, &st.Owner.Project, &st.Owner.Target, &st.Key, &st.Name, &start, &dur); err != nil {
			return nil, err
		}
		st.Owner.Instance = fmt.Sprintf("%06x", instance)
		st.Started = time.UnixMilli(start).UTC()
		st.Completed = st.Started.Add(time.Duration(dur) * time.Millisecond)
		out = append(out, st)
	}
	return out, rows.Err()
}

// CacheUsage is each node's cache by owner, by node.
func (s *Store) CacheUsage(ctx context.Context) (map[string]cacheuse.Usage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node, kind, owners != '[]', project_id IS NOT NULL, COALESCE(instance, 0),
		COALESCE(project_id, 0), SUM(size), COUNT(*)
		FROM cache_records GROUP BY node, kind, owners != '[]', project_id IS NOT NULL, instance, project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]cacheuse.Usage{}
	for rows.Next() {
		var node, kind string
		var owned, alone bool
		var instance int64
		var project uint64
		var bytes int64
		var n int
		if err := rows.Scan(&node, &kind, &owned, &alone, &instance, &project, &bytes, &n); err != nil {
			return nil, err
		}
		u := out[node]
		u.Total += bytes
		switch cacheuse.Kind(kind) {
		case cacheuse.Base:
			u.Base += bytes
		case cacheuse.Context:
			u.Contexts += bytes
		case cacheuse.Mount:
			u.Mounts += bytes
		case cacheuse.Layer:
			switch {
			case alone:
				u.Projects = append(u.Projects, cacheuse.ProjectBytes{Instance: fmt.Sprintf("%06x", instance), Project: project, Bytes: bytes, Records: n})
			case owned:
				u.Shared += bytes
			default:
				u.Unknown += bytes
			}
		default:
			u.Other += bytes
		}
		out[node] = u
	}
	for node, u := range out {
		sort.Slice(u.Projects, func(i, j int) bool { return u.Projects[i].Bytes > u.Projects[j].Bytes })
		out[node] = u
	}
	return out, rows.Err()
}

// ProjectRecords is the records a project's builds made alone on a node,
// for removing them: the ids and their bytes. Records in use are left out.
func (s *Store) ProjectRecords(ctx context.Context, node string, p ProjectKey) ([]string, int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, size FROM cache_records
		WHERE node = ? AND instance = ? AND project_id = ? AND in_use = 0`, node, nullInstance(p.Instance), p.ID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var ids []string
	var bytes int64
	for rows.Next() {
		var id string
		var size int64
		if err := rows.Scan(&id, &size); err != nil {
			return nil, 0, err
		}
		ids, bytes = append(ids, id), bytes+size
	}
	return ids, bytes, rows.Err()
}

// CacheAction is a removal from a node's cache someone asked for.
type CacheAction struct {
	At      time.Time
	Who     string
	Node    string
	Project ProjectKey
	Records int
	Bytes   int64
	Error   string
}

// SaveCacheAction records a removal.
func (s *Store) SaveCacheAction(ctx context.Context, a CacheAction) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO cache_actions (at, who, node, instance, project_id, records, bytes, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, a.At.UnixMilli(), a.Who, a.Node, nullInstance(a.Project.Instance), nullUint(a.Project.ID),
		a.Records, a.Bytes, a.Error)
	return err
}

// CacheActions lists the latest removals, newest first.
func (s *Store) CacheActions(ctx context.Context, limit int) ([]CacheAction, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, who, node, COALESCE(instance, 0), COALESCE(project_id, 0), records, bytes, error
		FROM cache_actions ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CacheAction
	for rows.Next() {
		var a CacheAction
		var at, instance int64
		if err := rows.Scan(&at, &a.Who, &a.Node, &instance, &a.Project.ID, &a.Records, &a.Bytes, &a.Error); err != nil {
			return nil, err
		}
		a.At, a.Project.Instance = time.UnixMilli(at).UTC(), fmt.Sprintf("%06x", instance)
		out = append(out, a)
	}
	return out, rows.Err()
}
