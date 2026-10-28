package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// CompareView sets two builds of one image side by side: what BuildKit
// recorded for each and where they differ. A is the earlier one.
type CompareView struct {
	Shell
	Name, Target string
	A, B         CmpSide
	Facts        []CmpFact
	Args         []CmpFact
	Dockerfile   []DiffLine // the Dockerfiles' differing lines with context; nil when equal
	DfKnown      bool       // both builds have a Dockerfile on record
	Steps        []CmpStep
	GitLabDiff   string // GitLab's comparison of the two commits
	Picks        []CmpPick
	Self         int64
}

// CmpSide is one of the two builds.
type CmpSide struct {
	ID     int64
	URL    string
	Label  string // "#pipeline · when"
	Status Status
}

// CmpFact is one recorded fact of both builds.
type CmpFact struct {
	K, A, B string
	Differs bool
}

// CmpStep is a step of either build (by its place, "[stage n/m]").
type CmpStep struct {
	Key, Name, NameA string // NameA: the earlier build's text when it differs
	A, B             string // state labels; "" when the build did not report the step
	StateA, StateB   string
	DA, DB           string
	Delta            string
	Slower, Changed  bool
}

// CmpPick is another build of the image to compare with.
type CmpPick struct {
	URL, Label string
	Status     Status
	Current    bool
}

// DiffLine is a line of a unified diff: Op is " ", "-" or "+".
type DiffLine struct {
	Op, Text string
	Gap      bool // lines left out before this one
}

func (s *Server) compare(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r, "build")
		return
	}
	ctx := r.Context()
	load := func(id int64) (*built, error) {
		p, err := s.data.BuildPage(ctx, id)
		if err == nil && !s.mayBuild(r, p) {
			err = store.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		return s.explain(ctx, p)
	}
	self, err := load(id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r, "build")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	sb := self.Page.Build
	project := projectOf(self.Page)
	sh, err := s.shell(r, "builds", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	pinProject(&sh, sb.Attribution.Instance, project)
	v := &CompareView{Name: orDefault(sb.Target), Target: sb.Target, Self: id}
	sh.Title = "Compare builds of " + v.Name
	v.Shell = sh
	loc := zone(r)

	// The other builds of the image, newest first, to pick from.
	var rows []store.BuildRow
	if project != 0 {
		rows, err = s.data.Builds(ctx, store.BuildQuery{Project: store.ProjectKey{Instance: sb.Attribution.Instance, ID: project},
			Target: sb.Target, Limit: 30})
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	with := int64(0)
	if q := r.URL.Query().Get("with"); q != "" {
		if with, err = strconv.ParseInt(q, 10, 64); err != nil {
			s.notFound(w, r, "build")
			return
		}
	} else if prev, _, err := s.data.Previous(ctx, sb); err == nil && prev != nil {
		with = prev.ID
	}
	for _, b := range rows {
		if b.ID == id {
			continue
		}
		v.Picks = append(v.Picks, CmpPick{URL: fmt.Sprintf("/builds/%d/compare?with=%d", id, b.ID),
			Label: pipeLabel(b.Pipeline) + clockStamp(b.Started, inZone(s.now(), loc)).Text, Status: rowStatus(b), Current: b.ID == with})
	}
	if with == 0 || with == id {
		s.render(w, "compare", http.StatusOK, v)
		return
	}
	other, err := load(with)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r, "build")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	ob := other.Page.Build
	if ob.Target != sb.Target || projectOf(other.Page) != project || ob.Attribution.Instance != sb.Attribution.Instance {
		s.render(w, "compare", http.StatusBadRequest, v)
		return
	}
	a, b := other, self
	if sb.Started.Before(ob.Started) {
		a, b = self, other
	}
	side := func(d *built) CmpSide {
		bb := d.Page.Build
		label := clockStamp(bb.Started, inZone(s.now(), loc)).Text
		if p := d.Page.Pipeline; p != nil {
			label = pipeLabel(p.ID) + label
		}
		return CmpSide{ID: d.Page.ID, URL: fmt.Sprintf("/builds/%d", d.Page.ID), Label: label,
			Status: statusOf(d.Page.Reason.Kind, bb.Error, d.Built, bb.Completed.IsZero())}
	}
	v.A, v.B = side(a), side(b)
	v.Facts = compareFacts(a, b)
	v.Args = compareArgs(a.Page.Build.BuildArgs, b.Page.Build.BuildArgs)
	if a.Page.Dockerfile != "" && b.Page.Dockerfile != "" {
		v.DfKnown = true
		v.Dockerfile = lineDiff(a.Page.Dockerfile, b.Page.Dockerfile, 2)
	}
	v.Steps = compareSteps(a.Views, b.Views)
	if pa, pb := a.Page.Pipeline, b.Page.Pipeline; pa != nil && pb != nil && pa.SHA != "" && pb.SHA != "" && pa.SHA != pb.SHA && b.Page.Job != nil {
		if base := projectWebURL(b.Page.Job.URL); base != "" {
			v.GitLabDiff = base + "/-/compare/" + url.PathEscape(pa.SHA) + "..." + url.PathEscape(pb.SHA) + "?straight=true"
		}
	}
	s.render(w, "compare", http.StatusOK, v)
}

func projectOf(p *store.BuildPage) uint64 {
	switch {
	case p.Pipeline != nil && p.Pipeline.ProjectID != 0:
		return p.Pipeline.ProjectID
	case p.Job != nil:
		return p.Job.ProjectID
	}
	return 0
}

func pipeLabel(id uint64) string {
	if id == 0 {
		return ""
	}
	return "#" + strconv.FormatUint(id, 10) + " · "
}

// projectWebURL is a project's address from one of its job's (".../-/jobs/N").
func projectWebURL(jobURL string) string {
	u := webURL(jobURL)
	if i := strings.Index(u, "/-/jobs/"); i > 0 {
		return u[:i]
	}
	return ""
}

func compareFacts(a, b *built) []CmpFact {
	ab, bb := a.Page.Build, b.Page.Build
	dur := func(x *history.Build) string {
		if x.Completed.IsZero() {
			return "building"
		}
		return duration(x.Completed.Sub(x.Started))
	}
	commit := func(d *built) string {
		if p := d.Page.Pipeline; p != nil && p.SHA != "" {
			return short(p.SHA, 8) + paren(clip(p.CommitTitle, 60))
		}
		return ""
	}
	ctxSize := func(x *history.Build) string {
		if x.ContextBytes == 0 {
			return ""
		}
		return bytesText(x.ContextBytes)
	}
	ran := func(d *built) string { return fmt.Sprintf("%d of %d", d.Built, len(d.Page.Build.Steps)) }
	out := []CmpFact{
		{K: "Time", A: dur(ab), B: dur(bb)},
		{K: "Steps run", A: ran(a), B: ran(b)},
		{K: "Commit", A: commit(a), B: commit(b)},
		{K: "Node", A: shortDaemon(ab.Daemon), B: shortDaemon(bb.Daemon)},
		{K: "Build context sent", A: ctxSize(ab), B: ctxSize(bb)},
		{K: "Error", A: clip(firstLine(ab.Error), 120), B: clip(firstLine(bb.Error), 120)},
	}
	kept := out[:0]
	for _, f := range out {
		if f.A == "" && f.B == "" {
			continue
		}
		f.Differs = f.A != f.B
		kept = append(kept, f)
	}
	return kept
}

// compareArgs lists the build arguments either build set, the differing
// ones first; secret-looking values are masked in both.
func compareArgs(a, b map[string]string) []CmpFact {
	names := map[string]bool{}
	for k := range a {
		names[k] = true
	}
	for k := range b {
		names[k] = true
	}
	var out []CmpFact
	for _, k := range sortedKeys(names) {
		va, vb := a[k], b[k]
		f := CmpFact{K: k, A: va, B: vb, Differs: va != vb}
		if history.SecretArg(k) {
			f.A, f.B = maskIf(va), maskIf(vb)
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Differs && !out[j].Differs })
	return out
}

func maskIf(v string) string {
	if v == "" {
		return ""
	}
	return history.Masked
}

// compareSteps pairs the steps of two builds by their place in the
// Dockerfile ("[stage n/m]"), in the later build's order, then the steps
// only the earlier one has.
func compareSteps(a, b []StepView) []CmpStep {
	byKey := map[string]StepView{}
	for _, s := range a {
		byKey[s.Key] = s
	}
	seen := map[string]bool{}
	var out []CmpStep
	add := func(sa, sb *StepView) {
		c := CmpStep{}
		if sb != nil {
			c.Key, c.Name, c.B, c.StateB, c.DB = sb.Key, sb.Name, stateLabels[sb.State], sb.State, duration(sb.D)
		}
		if sa != nil {
			c.A, c.StateA, c.DA = stateLabels[sa.State], sa.State, duration(sa.D)
			if sb == nil {
				c.Key, c.Name = sa.Key, sa.Name
			} else if sa.Name != sb.Name {
				c.NameA, c.Changed = sa.Name, true
			}
		}
		if sa != nil && sb != nil && (sa.D >= 100*time.Millisecond || sb.D >= 100*time.Millisecond) {
			c.Delta, c.Slower = delta(sb.D, sa.D)
		}
		out = append(out, c)
	}
	for i := range b {
		sb := &b[i]
		seen[sb.Key] = true
		if sa, ok := byKey[sb.Key]; ok {
			add(&sa, sb)
		} else {
			add(nil, sb)
		}
	}
	for i := range a {
		if !seen[a[i].Key] {
			add(&a[i], nil)
		}
	}
	return out
}

// lineDiff is a unified diff of two texts by lines (longest common
// subsequence), with ctx lines of context around each change; nil when
// the texts are equal or too long to compare here.
func lineDiff(a, b string, ctx int) []DiffLine {
	if a == b {
		return nil
	}
	x, y := strings.Split(strings.TrimRight(a, "\n"), "\n"), strings.Split(strings.TrimRight(b, "\n"), "\n")
	if len(x)*len(y) > 4_000_000 {
		return nil
	}
	// lcs[i][j]: the longest common subsequence of x[i:] and y[j:].
	lcs := make([][]int32, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var all []DiffLine
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			all = append(all, DiffLine{Op: " ", Text: x[i]})
			i, j = i+1, j+1
		case i < len(x) && (j == len(y) || lcs[i+1][j] >= lcs[i][j+1]):
			// What went comes before what came.
			all = append(all, DiffLine{Op: "-", Text: x[i]})
			i++
		default:
			all = append(all, DiffLine{Op: "+", Text: y[j]})
			j++
		}
	}
	keep := make([]bool, len(all))
	for k, l := range all {
		if l.Op != " " {
			for m := max(0, k-ctx); m <= min(len(all)-1, k+ctx); m++ {
				keep[m] = true
			}
		}
	}
	var out []DiffLine
	gap := false
	for k, l := range all {
		if !keep[k] {
			gap = true
			continue
		}
		l.Gap = gap && len(out) > 0
		gap = false
		out = append(out, l)
	}
	return out
}
