package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// PipeQuery selects a page of a project's pipelines, newest first.
type PipeQuery struct {
	Project ProjectKey
	Ref     string
	Author  string // "Name" or "Name <email>"
	// Search: a pipeline number or its first digits ("123", "#123"), the
	// start of a commit SHA, or text in the commit title, the branch, the author or the name
	// of one of the pipeline's jobs (case does not matter).
	Search string
	// Since and Until bound when the pipelines were created: Since
	// included, Until not.
	Since, Until time.Time
	Before       time.Time
	Limit        int
}

// searchWhere is the condition for PipeQuery.Search on table "pipelines".
func searchWhere(q string) (string, []any) {
	q = strings.TrimSpace(q)
	like := "%" + likeEscape(q) + "%"
	conds := []string{`commit_title LIKE ? ESCAPE '\'`, `ref LIKE ? ESCAPE '\'`, `commit_author LIKE ? ESCAPE '\'`,
		`EXISTS (SELECT 1 FROM jobs WHERE jobs.instance = pipelines.instance AND jobs.pipeline_id = pipelines.pipeline_id
			AND jobs.name LIKE ? ESCAPE '\' AND ` + pipelinesOwn + `)`}
	args := []any{like, like, like, like}
	if digits := strings.TrimPrefix(q, "#"); digits != "" {
		if n, err := strconv.ParseUint(digits, 10, 64); err == nil && n > 0 {
			conds, args = append(conds, "pipeline_id = ?"), append(args, nullUint(n))
			// GitLab's numbers are long: the first digits are enough.
			if len(digits) >= 3 && digits[0] != '0' {
				conds, args = append(conds, "CAST(pipeline_id AS TEXT) LIKE ?"), append(args, digits+"%")
			}
		}
	}
	if len(q) >= 4 && len(q) <= 40 && strings.Trim(strings.ToLower(q), "0123456789abcdef") == "" {
		conds, args = append(conds, "sha LIKE ?"), append(args, strings.ToLower(q)+"%")
	}
	return "(" + strings.Join(conds, " OR ") + ")", args
}

// likeEscape makes s match itself in a LIKE pattern with ESCAPE '\'.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(s)
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
			append(args, q.Author, utf8.RuneCountInString(q.Author)+2, q.Author+" <")
	}
	if strings.TrimSpace(q.Search) != "" {
		w, a := searchWhere(q.Search)
		where, args = append(where, w), append(args, a...)
	}
	if !q.Since.IsZero() {
		where, args = append(where, "COALESCE(created_at, seen_at) >= ?"), append(args, q.Since.UnixMilli())
	}
	if !q.Until.IsZero() {
		where, args = append(where, "COALESCE(created_at, seen_at) < ?"), append(args, q.Until.UnixMilli())
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
		") AND "+pipelinesOwn+" ORDER BY started_at IS NULL, started_at, queued_at, job_id", args...)
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
