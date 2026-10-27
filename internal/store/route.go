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

// Warmth is where each project's cache is: the nodes where its builds ran
// and the routers' decisions of the last hour (a job the router sent
// somewhere warms that node before its build is stored).
type Warmth map[route.Project][]route.Activity

// Warm reads where projects are warm: builds since since, and decisions
// of the last hour. live names the daemon pods running now: a decision
// for another (replaced since) does not count, and unless persists (the
// cache survives a daemon's restart: hostPath) neither does a build of
// another, as its cache went with it. live nil: every daemon counts.
func (s *Store) Warm(ctx context.Context, since time.Time, live []string, persists bool) (Warmth, error) {
	recent := s.opts.Now().Add(-time.Hour)
	if recent.Before(since) {
		recent = since
	}
	liveJSON := "null"
	if live != nil {
		b, _ := json.Marshal(live)
		liveJSON = string(b)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.instance, b.project_id, d.node, MAX(b.started_at) FROM builds b JOIN daemons d ON d.id = b.daemon_id
		WHERE b.started_at >= ? AND b.instance IS NOT NULL AND b.project_id IS NOT NULL AND d.node != ''
			AND (? OR json_type(?) = 'null' OR d.name IN (SELECT value FROM json_each(?)))
		GROUP BY b.instance, b.project_id, d.node
		UNION ALL
		SELECT instance, project_id, node, MAX(at) FROM route_decisions
		WHERE at >= ? AND instance IS NOT NULL AND project_id IS NOT NULL
			AND (json_type(?) = 'null' OR daemon IN (SELECT value FROM json_each(?)))
		GROUP BY instance, project_id, node`,
		since.UnixMilli(), persists, liveJSON, liveJSON, recent.UnixMilli(), liveJSON, liveJSON)
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
