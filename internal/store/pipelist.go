package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// PipeQuery selects a page of a project's pipelines, newest first.
type PipeQuery struct {
	Project ProjectKey
	Ref     string
	Author  string // "Name" or "Name <email>"
	Before  time.Time
	Limit   int
}

// PipeRow is a pipeline as the pipelines page lists it.
type PipeRow struct {
	Instance       string
	ID             uint64
	Ref, SHA       string
	Source         string
	Title, Author  string
	MergeRequest   uint64
	Created        time.Time // GitLab's creation, else when the console first saw it
	Status, WebURL string
	APIAt          time.Time
}

// PipelineList returns a page of a project's pipelines, newest first.
func (s *Store) PipelineList(ctx context.Context, q PipeQuery) ([]PipeRow, error) {
	where := []string{"instance = ?", "project_id = ?"}
	args := []any{nullInstance(q.Project.Instance), nullUint(q.Project.ID)}
	if q.Ref != "" {
		where, args = append(where, "ref = ?"), append(args, q.Ref)
	}
	if q.Author != "" {
		where, args = append(where, "(commit_author = ? OR substr(commit_author, 1, ?) = ?)"),
			append(args, q.Author, len(q.Author)+2, q.Author+" <")
	}
	if !q.Before.IsZero() {
		where, args = append(where, "COALESCE(created_at, seen_at) < ?"), append(args, q.Before.UnixMilli())
	}
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT instance, pipeline_id, ref, sha, source, commit_title, commit_author, mr_iid,
			COALESCE(created_at, seen_at), status, web_url, api_at
		FROM pipelines WHERE `+strings.Join(where, " AND ")+`
		ORDER BY COALESCE(created_at, seen_at) DESC, pipeline_id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PipeRow
	for rows.Next() {
		var (
			p                  PipeRow
			instance           int64
			mr, created, apiAt sql.NullInt64
		)
		if err := rows.Scan(&instance, &p.ID, &p.Ref, &p.SHA, &p.Source, &p.Title, &p.Author, &mr, &created, &p.Status, &p.WebURL, &apiAt); err != nil {
			return nil, err
		}
		p.Instance = fmt.Sprintf("%06x", instance)
		p.MergeRequest, p.Created, p.APIAt = uint64(mr.Int64), msTime(created), msTime(apiAt)
		out = append(out, p)
	}
	return out, rows.Err()
}

// JobsOf returns the jobs of several pipelines of one GitLab, by pipeline.
func (s *Store) JobsOf(ctx context.Context, instance string, pipelines []uint64) (map[uint64][]JobInfo, error) {
	out := map[uint64][]JobInfo{}
	if len(pipelines) == 0 {
		return out, nil
	}
	args := []any{nullInstance(instance)}
	for _, id := range pipelines {
		args = append(args, nullUint(id))
	}
	js, err := s.jobs(ctx, "INDEXED BY jobs_pipeline WHERE instance = ? AND pipeline_id IN (?"+strings.Repeat(", ?", len(pipelines)-1)+
		") ORDER BY started_at IS NULL, started_at, queued_at, job_id", args...)
	if err != nil {
		return nil, err
	}
	for _, j := range js {
		out[j.PipelineID] = append(out[j.PipelineID], j)
	}
	return out, nil
}

// BuildsOf returns the builds of several pipelines of a project, by
// pipeline, without their steps.
func (s *Store) BuildsOf(ctx context.Context, p ProjectKey, pipelines []uint64) (map[uint64][]BuildRow, error) {
	out := map[uint64][]BuildRow{}
	for _, id := range pipelines {
		bs, err := s.Builds(ctx, BuildQuery{Project: p, Pipeline: id, Light: true})
		if err != nil {
			return nil, err
		}
		out[id] = bs
	}
	return out, nil
}
