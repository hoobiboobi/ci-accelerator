package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/hints"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// MaxHintBuilds bounds the builds BuildHints reads at once.
const MaxHintBuilds = 100

// HintWindow is how far back a step's runs are counted for its hint.
const HintWindow = 30 * 24 * time.Hour

// StepHints are the cache-mount hints of one step of a build, with what
// running the step cost the image over HintWindow: measured, never
// estimated.
type StepHints struct {
	Step     int                 `json:"step"`
	Key      string              `json:"key"`
	Name     string              `json:"name"`
	Lines    []history.LineRange `json:"lines,omitempty"`
	Hints    []hints.Hint        `json:"hints"`
	Fragment string              `json:"fragment,omitempty"`
	// Runs is how often builds of the image executed the step (the same
	// instruction) since Since, RunTime their time together, Median the
	// middle one.
	Runs    int           `json:"runs"`
	RunTime time.Duration `json:"runTime"`
	Median  time.Duration `json:"median"`
	Since   time.Time     `json:"since"`
}

// Broken reports whether a step's hints are about a cache mount that is
// there but cannot work.
func (h StepHints) Broken() bool {
	for _, x := range h.Hints {
		if x.Broken() {
			return true
		}
	}
	return false
}

// Hints returns a build's cache-mount hints, the costliest first. A step
// is listed when one of its package managers has no cache mount and the
// image's builds really executed it within HintWindow, or when a cache
// mount it has cannot work (whether it ran or not). The build's graph
// must be restored (Load); without it there are none.
func (s *Store) Hints(ctx context.Context, st *Stored) ([]StepHints, error) {
	return s.hints(ctx, st, nil)
}

// runsMemo keeps step runs already read, by scope and step name: the
// builds of one list are mostly of the same images.
type runsMemo map[string][]time.Duration

func (s *Store) hints(ctx context.Context, st *Stored, memo runsMemo) ([]StepHints, error) {
	b := st.Build
	// The 30 days before now; an older build's own runs count too.
	since := s.opts.Now().Add(-HintWindow)
	if !b.Started.IsZero() && b.Started.Before(since) {
		since = b.Started
	}
	var out []StepHints
	var names []string
	for i, step := range b.Steps {
		op, anc := b.StepOps(i)
		if op == nil {
			continue
		}
		hs := hints.Analyze(op, anc)
		if len(hs) == 0 {
			continue
		}
		out = append(out, StepHints{Step: i, Key: step.Key, Name: step.Name, Lines: step.Lines, Hints: hs, Since: since,
			Fragment: hints.Fragment(step.Name, hs)})
		names = append(names, step.Name)
	}
	if len(out) == 0 {
		return nil, nil
	}
	runs, err := s.stepRuns(ctx, st.ID, names, since, memo)
	if err != nil {
		return nil, err
	}
	kept := out[:0]
	for _, h := range out {
		r := runs[h.Name]
		h.Runs, h.Median = len(r), Percentile(r, 50)
		for _, d := range r {
			h.RunTime += d
		}
		if h.Runs == 0 && !h.Broken() {
			continue
		}
		kept = append(kept, h)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].RunTime != kept[j].RunTime {
			return kept[i].RunTime > kept[j].RunTime
		}
		return kept[i].Step < kept[j].Step
	})
	return kept, nil
}

const (
	stepRunsQuery = `SELECT COALESCE(s.name, sd.name, ''), s.dur_ms
		FROM builds b JOIN steps s ON s.build_id = b.id AND s.state = 1
		LEFT JOIN step_defs sd ON sd.identity = s.identity
		WHERE `
	stepRunsProject = "b.instance IS ? AND b.project_id = ? AND b.target = ? AND b.started_at >= ?"
)

// stepRuns returns, by step name, how long each execution of those steps
// took since t in the builds of the same image: the build's project and
// target (or, without a known project, its pipeline; or only the build
// itself). A step is the same when its instruction is the same.
func (s *Store) stepRuns(ctx context.Context, buildID int64, names []string, since time.Time, memo runsMemo) (map[string][]time.Duration, error) {
	var (
		instance, pipeline, project sql.NullInt64
		target                      string
	)
	err := s.db.QueryRowContext(ctx, `SELECT b.instance, b.pipeline_id, COALESCE(b.project_id, p.project_id), b.target
		FROM builds b LEFT JOIN pipelines p ON p.instance = b.instance AND p.pipeline_id = b.pipeline_id
		WHERE b.id = ?`, buildID).Scan(&instance, &pipeline, &project, &target)
	if err != nil {
		return nil, err
	}
	// One query by the narrowest index that fits: the image's builds of the
	// window (builds_target), the pipeline's, or the build alone.
	var scope string
	var args []any
	switch {
	case project.Valid:
		scope, args = stepRunsProject, []any{instance, project, target, since.UnixMilli()}
	case pipeline.Valid:
		scope, args = "b.instance IS ? AND b.pipeline_id = ? AND b.target = ? AND b.started_at >= ?", []any{instance, pipeline, target, since.UnixMilli()}
	default:
		scope, args = "b.id = ?", []any{buildID}
	}
	out := map[string][]time.Duration{}
	key := fmt.Sprint(scope, args, "|")
	var ask []string
	for _, n := range names {
		if r, ok := memo[key+n]; ok {
			out[n] = r
		} else {
			ask = append(ask, n)
		}
	}
	if len(ask) == 0 {
		return out, nil
	}
	in := strings.Repeat(", ?", len(ask)-1)
	for _, n := range ask {
		args = append(args, n)
	}
	rows, err := s.db.QueryContext(ctx, stepRunsQuery+scope+` AND COALESCE(s.name, sd.name, '') IN (?`+in+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			name string
			ms   int64
		)
		if err := rows.Scan(&name, &ms); err != nil {
			return nil, err
		}
		out[name] = append(out[name], time.Duration(ms)*time.Millisecond)
	}
	if memo != nil {
		for _, n := range ask {
			memo[key+n] = out[n]
		}
	}
	return out, rows.Err()
}

// BuildHints returns the cache-mount hints of builds by id (see Hints),
// for the API; a build without any is left out.
func (s *Store) BuildHints(ctx context.Context, ids []int64) (map[int64][]StepHints, error) {
	out := map[int64][]StepHints{}
	// Each build's graph is restored to read its steps: the newest
	// MaxHintBuilds of a list get hints, the rest none.
	if len(ids) > MaxHintBuilds {
		ids = ids[:MaxHintBuilds]
	}
	memo := runsMemo{}
	for _, id := range ids {
		st, err := s.Load(ctx, id)
		if st == nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		// A graph past retention restores nothing: no hints, no error.
		hs, err := s.hints(ctx, st, memo)
		if err != nil {
			return nil, err
		}
		if len(hs) > 0 {
			out[id] = hs
		}
	}
	return out, nil
}
