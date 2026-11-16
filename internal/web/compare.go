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
	DfMissing    []string   // otherwise, why each build without one has none
	Steps        []CmpStep
	GitLabDiff   string // GitLab's comparison of the two commits
	Picks        []CmpPick
	Self         int64
	// This is the build the page belongs to, That the one compared with
	// (A and B are the same two by time).
	This, That CmpSide
	// PickURL picks another build in the builds list.
	PickURL string
	// Message says why nothing is compared.
	Message string
}

// CmpSide is one of the two builds.
type CmpSide struct {
	ID     int64
	URL    string
	Label  string // "#pipeline · when"
	Status Status
	Self   bool // the build the page belongs to
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
	// Delta is the later build's time minus the earlier one's, signed
	// ("+2.1 s", "−1.3 s"), shown from 0.1 s; Pct is it as a share of the
	// earlier build's time ("17 %"; "" when that took 0 s). Big marks a
	// large one: at least 1 s and 20 %, or at least 30 s.
	Delta, Pct  string
	Slower, Big bool
	Changed     bool
}

// CmpPick is another build of the image to compare with.
type CmpPick struct {
	URL, Label string
	Status     Status
	Current    bool // compared with now
	Previous   bool // the image's build before this one
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
		p, err := s.buildPage(ctx, id)
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
	v := &CompareView{Name: buildName(sb.Target, sb.Images), Target: sb.Target, Self: id,
		PickURL: Filters{Image: imageKey(sb.Target), Pick: true, A: id}.URL()}
	sh.Title = "Compare builds of " + v.Name
	v.Shell = sh
	loc := zone(r)

	// The other builds of the image, newest first, to pick from (30 of
	// them; builds without a target are read further back, as they are
	// of every Dockerfile without one).
	const picks = 30
	var rows []store.BuildRow
	if project != 0 {
		limit := picks
		if sb.Target == "" {
			limit = 10 * picks
		}
		rows, err = s.data.Builds(ctx, store.BuildQuery{Project: store.ProjectKey{Instance: sb.Attribution.Instance, ID: project},
			Target: sb.Target, NoTarget: sb.Target == "", Limit: limit})
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	with, prevID := int64(0), int64(0)
	if prev, _, err := s.previous(ctx, sb); err == nil && prev != nil {
		with, prevID = prev.ID, prev.ID
	}
	if q := r.URL.Query().Get("with"); q != "" {
		if with, err = strconv.ParseInt(q, 10, 64); err != nil {
			s.notFound(w, r, "build")
			return
		}
	}
	for _, b := range rows {
		if b.ID == id || !sameImage(b.Target, b.Images, sb.Target, sb.Images) {
			continue
		}
		if len(v.Picks) == picks {
			break
		}
		v.Picks = append(v.Picks, CmpPick{URL: fmt.Sprintf("/builds/%d/compare?with=%d", id, b.ID),
			Label: pipeLabel(b.Pipeline) + clockStamp(b.Started, inZone(s.now(), loc)).Text, Status: rowStatus(b), Current: b.ID == with,
			Previous: b.ID == prevID})
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
	if !sameImage(ob.Target, ob.Images, sb.Target, sb.Images) || projectOf(other.Page) != project || ob.Attribution.Instance != sb.Attribution.Instance {
		v.Message = "Build " + strconv.FormatInt(with, 10) + " is not a build of " + v.Name + " in this project: only two builds of one image are compared."
		for i := range v.Picks {
			v.Picks[i].Current = false
		}
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
			Status: statusOf(d.Page.Reason.Kind, bb.Error, d.Built, bb.Completed.IsZero()), Self: d.Page.ID == id}
	}
	v.A, v.B = side(a), side(b)
	v.This, v.That = v.B, v.A
	if v.A.Self {
		v.This, v.That = v.A, v.B
	}
	v.Facts = compareFacts(a, b)
	v.Args = compareArgs(a.Page.Build.BuildArgs, b.Page.Build.BuildArgs)
	if a.Page.Dockerfile != "" && b.Page.Dockerfile != "" {
		v.DfKnown = true
		v.Dockerfile = lineDiff(a.Page.Dockerfile, b.Page.Dockerfile, 2)
	}
	for _, d := range []struct {
		page *built
		side CmpSide
	}{{a, v.A}, {b, v.B}} {
		if d.page.Page.Dockerfile == "" {
			v.DfMissing = append(v.DfMissing, d.side.Label+": "+noDockerfile(d.page.Page.Build.Error))
		}
	}
	v.Steps = compareSteps(a.Views, b.Views)
	if pa, pb := a.Page.Pipeline, b.Page.Pipeline; pa != nil && pb != nil && pa.SHA != "" && pb.SHA != "" && pa.SHA != pb.SHA && b.Page.Job != nil {
		if base := projectWebURL(b.Page.Job.URL); base != "" {
			v.GitLabDiff = base + "/-/compare/" + url.PathEscape(pa.SHA) + "..." + url.PathEscape(pb.SHA) + "?straight=true"
		}
	}
	s.render(w, "compare", http.StatusOK, v)
}

// projectOf is a build's project: its job's, else its pipeline's as
// GitLab confirmed it; 0 for none. The trace id naming both is the job's
// own choice, and the build's page shows the pipeline's commit and lists
// the job's other builds: a job of another project than the pipeline's
// makes the build no project's (admins only). So does a job not known
// yet in a pipeline only a job's pod named: that job may be of another
// project, which named the pipeline before it existed.
func projectOf(p *store.BuildPage) uint64 {
	project := uint64(0)
	if p.Pipeline != nil {
		project = p.Pipeline.ProjectID
	}
	if p.Job == nil || p.Job.ProjectID == 0 {
		if p.Pipeline == nil || p.Pipeline.APIAt.IsZero() {
			return 0
		}
		return project
	}
	if project != 0 && project != p.Job.ProjectID {
		return 0
	}
	return p.Job.ProjectID
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
	ran := func(d *built) string { return fmt.Sprintf("%d of %d", stepsRan(d.Page.Build), len(d.Page.Build.Steps)) }
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
		if sa != nil && sb != nil {
			c.Delta, c.Pct, c.Slower, c.Big = stepDelta(sb.D, sa.D)
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

// stepDelta is how much longer (or shorter) a step took in the later
// build than in the earlier one, for every difference of at least 0.1 s
// (the feed's delta hides the usual noise; a comparison shows it all).
// The figures are those the page shows: both times and the difference
// rounded the way duration writes them, the share of the earlier time
// cut down to a whole percent, so "20 %" is never below the mark. big:
// at least 1 s and 20 % of the earlier time, or at least 30 s.
func stepDelta(later, earlier time.Duration) (text, pct string, slower, big bool) {
	diff := shownTime(max(later, 0)) - shownTime(max(earlier, 0))
	abs := shownTime(max(diff, -diff))
	if abs < 100*time.Millisecond {
		return "", "", false, false
	}
	e := shownTime(max(earlier, 0))
	big = abs >= 30*time.Second || (abs >= time.Second && abs*5 >= e)
	if e > 0 {
		pct = "<1\u00a0%"
		if p := int64(abs) * 100 / int64(e); p >= 1 {
			pct = strconv.FormatInt(p, 10) + "\u00a0%"
		}
	}
	if diff > 0 {
		return "+" + duration(abs), pct, true, big
	}
	return "−" + duration(abs), pct, false, big
}

// shownTime is a time rounded the way duration writes it.
func shownTime(d time.Duration) time.Duration {
	switch {
	case d.Round(100*time.Millisecond) < 10*time.Second:
		return d.Round(100 * time.Millisecond)
	case d.Round(time.Second) < time.Hour:
		return d.Round(time.Second)
	default:
		return d.Round(time.Minute)
	}
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
