package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/route"
)

// SetDaemonNode records the node a daemon runs on (from its worker label).
func (s *Store) SetDaemonNode(ctx context.Context, name, node string) error {
	if node == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO daemons (name, node) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET node = excluded.node WHERE daemons.node != excluded.node`, name, node)
	return err
}

// Decision is a router's decision for one connection of a CI job.
type Decision struct {
	Project route.Project `json:"project"` // ID 0: the job was not known
	JobID   uint64        `json:"job"`
	Node    string        `json:"node"`   // the daemon's node
	Daemon  string        `json:"daemon"` // the daemon's pod
	Reason  route.Reason  `json:"reason"`
	Cands   int           `json:"candidates"`
	JobNode string        `json:"jobNode"`
	At      time.Time     `json:"at"`
}

// SaveDecisions stores routers' decisions.
func (s *Store) SaveDecisions(ctx context.Context, ds []Decision) error {
	if len(ds) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.PrepareContext(ctx, `INSERT INTO route_decisions
		(instance, job_id, project_id, node, daemon, reason, candidates, job_node, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, d := range ds {
		if _, err := st.ExecContext(ctx, nullInstance(d.Project.Instance), nullUint(d.JobID), nullUint(d.Project.ID),
			d.Node, d.Daemon, string(d.Reason), d.Cands, d.JobNode, d.At.UnixMilli()); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.changed(RouteKey)
	return nil
}

// RouteKey is announced when the routers' view of warmth may have changed.
const RouteKey = "route"

// Warmth is where each project's cache is: the nodes holding layers its
// builds made (whatever the daemon is called now, and gone once a restart
// of an emptyDir cache or GC removed them), with when they were last used;
// and the routers' decisions of the last hour (a job the router sent
// somewhere warms that node before its layers are listed).
type Warmth map[route.Project][]route.Activity

// Warm reads where projects are warm. live names the daemons running now:
// decisions for others (replaced since) do not count; nil: all count.
func (s *Store) Warm(ctx context.Context, live []string) (Warmth, error) {
	recent := s.opts.Now().Add(-time.Hour).UnixMilli()
	q := `SELECT instance, project_id, node, MAX(COALESCE(last_used, created_at)) FROM cache_records
		WHERE project_id IS NOT NULL AND instance IS NOT NULL
		GROUP BY instance, project_id, node
		UNION ALL
		SELECT instance, project_id, node, MAX(at) FROM route_decisions
		WHERE at >= ? AND instance IS NOT NULL AND project_id IS NOT NULL`
	args := []any{recent}
	if live != nil {
		q += ` AND daemon IN (SELECT value FROM json_each(?))`
		b, _ := json.Marshal(live)
		args = append(args, string(b))
	}
	q += ` GROUP BY instance, project_id, node`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := Warmth{}
	for rows.Next() {
		var instance int64
		var project uint64
		var node string
		var at int64
		if err := rows.Scan(&instance, &project, &node, &at); err != nil {
			return nil, err
		}
		p := route.Project{Instance: fmt.Sprintf("%06x", instance), ID: project}
		out[p] = append(out[p], route.Activity{Node: node, At: time.UnixMilli(at).UTC()})
	}
	return out, rows.Err()
}

// JobRoutes is the routers' decisions for a job, oldest first.
func (s *Store) JobRoutes(ctx context.Context, instance string, job uint64) ([]Decision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project_id, node, daemon, reason, candidates, job_node, at
		FROM route_decisions WHERE instance = ? AND job_id = ? ORDER BY at, id LIMIT 50`, nullInstance(instance), job)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Decision
	for rows.Next() {
		d := Decision{Project: route.Project{Instance: instance}, JobID: job}
		var project sql.NullInt64
		var at int64
		if err := rows.Scan(&project, &d.Node, &d.Daemon, &d.Reason, &d.Cands, &d.JobNode, &at); err != nil {
			return nil, err
		}
		d.Project.ID, d.At = uint64(project.Int64), time.UnixMilli(at).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

// DaemonNodes maps the daemons' pod names to their nodes.
func (s *Store) DaemonNodes(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, node FROM daemons WHERE node != ''")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, node string
		if err := rows.Scan(&name, &node); err != nil {
			return nil, err
		}
		out[name] = node
	}
	return out, rows.Err()
}
