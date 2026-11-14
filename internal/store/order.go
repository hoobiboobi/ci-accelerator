package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/hints"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// OrderHint is a step that installed dependencies again only because a
// COPY of the whole build context comes before it: GitLab's comparison of
// the commits proved which files changed, and none of them is a file the
// install reads. Took is the step's measured time in this build.
type OrderHint struct {
	Step      int                 `json:"step"`
	Key       string              `json:"key"`
	Name      string              `json:"name"`
	Lines     []history.LineRange `json:"lines,omitempty"`
	CopyStep  int                 `json:"copyStep"`
	CopyKey   string              `json:"copyKey"`
	CopyName  string              `json:"copyName"`
	CopyLines []history.LineRange `json:"copyLines,omitempty"`
	Managers  []string            `json:"managers"`
	// Files are the files the installs read (pyproject.toml, ...), none
	// of which changed.
	Files []string `json:"files"`
	// Others are the step's other commands, which need the code ("npm run
	// embed"): the step's time is theirs too.
	Others []string      `json:"others,omitempty"`
	Took   time.Duration `json:"took"`
	// Fragment replaces the COPY and the step; "" when the step cannot be
	// split safely (the hint then says what to do in words).
	Fragment string `json:"fragment,omitempty"`
	// Runs is how many builds of the image since Since installed again
	// this way (this one too), RunTime what those steps took.
	Runs    int           `json:"runs"`
	RunTime time.Duration `json:"runTime"`
	Since   time.Time     `json:"since"`
}

// OrderFixed is a rebuild whose dependencies came from the cache although
// an earlier build of the image installed them again after a code change:
// the Dockerfile copies the code after them now.
type OrderFixed struct {
	Managers []string `json:"managers"`
	// Step is the install that came from the cache in this build.
	Step int    `json:"step"`
	Name string `json:"name"`
	Line int    `json:"line,omitempty"`
	// Before is the latest build of the image that installed again,
	// Took its build time and Wasted the installs' time in it.
	Before    int64         `json:"before"`
	BeforeAt  time.Time     `json:"beforeAt"`
	Took      time.Duration `json:"took"`
	Wasted    time.Duration `json:"wasted"`
	ThisBuild time.Duration `json:"thisBuild"`
}

// clipped is the last entry of a changed-files list cut short ("12
// more"): the rest of the list is not known.
var clipped = regexp.MustCompile(`^\d+ more$`)

// orderHints finds the steps of a build that installed dependencies again
// only because of the order of its Dockerfile (see OrderHint). The build's
// graph must be restored. dockerfile is its text, for its frontend.
func orderHints(b *history.Build, r history.Reason, via, dockerfile string) []OrderHint {
	copyIdx, ok := orderCopy(b, r, via)
	if !ok {
		return nil
	}
	have := map[string]bool{}
	for _, m := range installedBelow(b, copyIdx) {
		have[m] = true
	}
	parents := frontendHasParents(dockerfile)
	var out []OrderHint
	for j, s := range b.Steps {
		if j == copyIdx || s.State != history.Built || !b.Ran(j) || !sameStage(b.Steps[copyIdx].Key, s.Key) ||
			s.Duration() < MinOrderTook || !b.DependsOn(j, copyIdx) {
			continue
		}
		op, _ := b.StepOps(j)
		is := hints.Installs(op, s.Name)
		if is == nil {
			continue
		}
		earlier := false
		for _, m := range is.Managers() {
			earlier = earlier || have[m]
		}
		if earlier {
			// The right order is there: a step before the copy installed
			// the dependencies; this one adds the project.
			continue
		}
		changed := false
		for _, p := range r.Paths {
			changed = changed || is.Matters(p)
		}
		if changed {
			continue
		}
		c := b.Steps[copyIdx]
		h := OrderHint{Step: j, Key: s.Key, Name: s.Name, Lines: s.Lines, CopyStep: copyIdx, CopyKey: c.Key, CopyName: c.Name,
			CopyLines: c.Lines, Managers: is.Managers(), Files: installFiles(is), Others: is.Others, Took: s.Duration()}
		if next(c.Key, s.Key) {
			h.Fragment = is.OrderFragment(c.Name, parents)
		}
		out = append(out, h)
	}
	return out
}

// orderCopy is the COPY of the whole context a verified "your change"
// rebuild started from; ok false for any other reason, an unverified
// one, or a changed-files list cut short.
func orderCopy(b *history.Build, r history.Reason, via string) (int, bool) {
	if r.Kind != history.FilesChanged || !strings.HasPrefix(via, "gitlab ") || len(r.Paths) == 0 {
		return -1, false
	}
	for _, p := range r.Paths {
		if clipped.MatchString(p) {
			return -1, false
		}
	}
	for i, s := range b.Steps {
		if s.Key == r.Step.Key && hints.WholeContext(s.Name) {
			return i, true
		}
	}
	return -1, false
}

// installedBelow lists the managers whose installs run in the steps the
// copy builds on.
func installedBelow(b *history.Build, copyIdx int) []string {
	_, anc := b.StepOps(copyIdx)
	var out []string
	for _, a := range anc {
		if is := hints.Installs(a, ""); is != nil {
			out = append(out, is.Managers()...)
		}
	}
	return out
}

// installFiles lists the files a step's installs read, once each, without
// the "*" of the optional ones.
func installFiles(is *hints.InstallStep) []string {
	var out []string
	seen := map[string]bool{}
	for _, in := range is.Installs {
		for _, f := range in.Files {
			f = strings.TrimSuffix(f, "*")
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	return out
}

// MinOrderTook is the least time an install must have taken for its
// order to be worth a hint.
const MinOrderTook = time.Second

// sameStage reports whether two step keys are of one stage ("[builder
// 4/5]", "[builder 5/5]"): an install in a later stage that copies from
// this one is moved in its own stage, not above this copy.
func sameStage(a, b string) bool {
	ma, mb := stepKey.FindStringSubmatch(a), stepKey.FindStringSubmatch(b)
	return ma != nil && mb != nil && ma[1] == mb[1]
}

var stepKey = regexp.MustCompile(`^\[(?:(.*\S)\s+)?(\d+)/(\d+)\]$`)

// next reports whether step key b comes right after a in the same stage
// ("[builder 4/5]", "[builder 5/5]").
func next(a, b string) bool {
	ma, mb := stepKey.FindStringSubmatch(a), stepKey.FindStringSubmatch(b)
	if ma == nil || mb == nil || ma[1] != mb[1] {
		return false
	}
	na, _ := strconv.Atoi(ma[2])
	nb, _ := strconv.Atoi(mb[2])
	return nb == na+1
}

var syntaxLine = regexp.MustCompile(`(?i)^#\s*syntax\s*=\s*(\S+)`)

// frontendHasParents reports whether the Dockerfile's frontend has COPY
// --parents (stable since docker/dockerfile 1.20, in labs since 1.7): the
// daemon's own frontend (no syntax line; BuildKit v0.34) and
// docker/dockerfile:1, :latest or 1.20 and later have it; another
// frontend, or an older pinned one, is not assumed to.
func frontendHasParents(dockerfile string) bool {
	for _, line := range strings.Split(dockerfile, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			return true
		}
		m := syntaxLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ref, _, _ := strings.Cut(m[1], "@")
		name, tag, _ := strings.Cut(ref, ":")
		name = strings.TrimPrefix(strings.TrimPrefix(name, "docker.io/"), "library/")
		if name != "docker/dockerfile" && name != "docker/dockerfile-upstream" {
			return false
		}
		labs := strings.HasSuffix(tag, "-labs") || tag == "labs"
		tag = strings.TrimSuffix(tag, "-labs")
		parts := strings.Split(tag, ".")
		switch {
		case tag == "" || tag == "latest" || tag == "labs" || len(parts) == 1:
			return true
		default:
			minor, err := strconv.Atoi(parts[1])
			return err == nil && parts[0] == "1" && (minor >= 20 || labs && minor >= 7)
		}
	}
	return true
}

// orderRuns adds to each hint how often the image's builds installed
// again this way since HintWindow (builds.order_ms), this build included.
func (s *Store) orderRuns(ctx context.Context, id int64, started time.Time, hs []OrderHint) error {
	if len(hs) == 0 {
		return nil
	}
	since := s.opts.Now().Add(-HintWindow)
	if !started.IsZero() && started.Before(since) {
		since = started
	}
	var (
		n     int
		total int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT count(*), COALESCE(sum(o.order_ms), 0) FROM builds b JOIN builds o
			ON o.instance IS b.instance AND o.project_id = b.project_id AND o.target = b.target
		WHERE b.id = ? AND o.order_ms > 0 AND o.started_at >= ? AND o.id <> b.id`, id, since.UnixMilli()).Scan(&n, &total)
	if err != nil {
		return err
	}
	var mine time.Duration
	for _, h := range hs {
		mine += h.Took
	}
	for i := range hs {
		hs[i].Since = since
		hs[i].Runs = n + 1
		// The image's other builds by their stored total, shared out by
		// this build's steps (one step in all but rare cases).
		hs[i].RunTime = hs[i].Took
		if mine > 0 {
			hs[i].RunTime += time.Duration(float64(time.Duration(total)*time.Millisecond) * float64(hs[i].Took) / float64(mine))
		}
	}
	return nil
}

// orderFixed finds whether a verified "your change" rebuild took its
// dependencies from the cache where an earlier build of the image
// installed them again (OrderFixed); nil otherwise.
func (s *Store) orderFixed(ctx context.Context, st *Stored, found []OrderHint) (*OrderFixed, error) {
	b := st.Build
	if len(found) > 0 {
		return nil, nil
	}
	copyIdx, ok := orderCopy(b, st.Reason, st.Via)
	if !ok {
		return nil, nil
	}
	fix := &OrderFixed{Step: -1}
	for k, step := range b.Steps {
		if step.State != history.Cached || !b.DependsOn(copyIdx, k) {
			continue
		}
		op, _ := b.StepOps(k)
		if is := hints.Installs(op, step.Name); is != nil {
			fix.Step, fix.Name, fix.Managers = k, step.Name, is.Managers()
			if len(step.Lines) > 0 {
				fix.Line = step.Lines[0].Start
			}
		}
	}
	if fix.Step < 0 {
		return nil, nil
	}
	var (
		before          int64
		started, wasted int64
		completed       sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `SELECT o.id, o.started_at, o.completed_at, o.order_ms FROM builds b JOIN builds o
			ON o.instance IS b.instance AND o.project_id = b.project_id AND o.target = b.target
		WHERE b.id = ? AND o.order_ms > 0 AND o.started_at < b.started_at AND o.started_at >= ?
		ORDER BY o.started_at DESC LIMIT 1`, st.ID, b.Started.Add(-HintWindow).UnixMilli()).Scan(&before, &started, &completed, &wasted)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fix.Before, fix.BeforeAt, fix.Wasted = before, time.UnixMilli(started), time.Duration(wasted)*time.Millisecond
	if completed.Valid {
		fix.Took = time.Duration(completed.Int64-started) * time.Millisecond
	}
	if !b.Completed.IsZero() {
		fix.ThisBuild = b.Completed.Sub(b.Started)
	}
	return fix, nil
}

// storeOrder computes a settled build's order hints and keeps their time
// (builds.order_ms) for the image's figures and the Insights page.
func (s *Store) storeOrder(ctx context.Context, id int64) error {
	st, err := s.Load(ctx, id)
	if st == nil {
		return err
	}
	var text sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT d.text FROM builds b LEFT JOIN dockerfiles d ON d.id = b.dockerfile_id WHERE b.id = ?`, id).Scan(&text); err != nil {
		return err
	}
	var ms int64
	for _, h := range orderHints(st.Build, st.Reason, st.Via, text.String) {
		ms += h.Took.Milliseconds()
	}
	_, err = s.db.ExecContext(ctx, "UPDATE builds SET order_ms = ? WHERE id = ?", ms, id)
	return err
}

// BuildOrder returns the order hints of builds by id (see OrderHint), for
// the API; a build without any is left out. Like BuildHints, only the
// newest MaxHintBuilds of a list are read.
func (s *Store) BuildOrder(ctx context.Context, ids []int64) (map[int64][]OrderHint, error) {
	out := map[int64][]OrderHint{}
	if len(ids) > MaxHintBuilds {
		ids = ids[:MaxHintBuilds]
	}
	for _, id := range ids {
		var (
			ms   int64
			text sql.NullString
		)
		err := s.db.QueryRowContext(ctx, `SELECT b.order_ms, d.text FROM builds b LEFT JOIN dockerfiles d ON d.id = b.dockerfile_id WHERE b.id = ?`, id).Scan(&ms, &text)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if ms <= 0 {
			// Settled builds keep what their installs took: none, none here.
			continue
		}
		st, err := s.Load(ctx, id)
		if st == nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		hs := orderHints(st.Build, st.Reason, st.Via, text.String)
		if err := s.orderRuns(ctx, id, st.Build.Started, hs); err != nil {
			return nil, err
		}
		if len(hs) > 0 {
			out[id] = hs
		}
	}
	return out, nil
}
