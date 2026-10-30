package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// The insights, the build list and the images pages: what a project's
// image builds came down to, the exceptions first.

// statsFor is how far back a project's figures (usual times, the images'
// strips) look.
const statsFor = 7 * 24 * time.Hour

// statsFresh is how long they are reused.
const statsFresh = time.Minute

type cachedStats struct {
	at     time.Time
	builds []store.BuildRow     // newest first, over statsFor
	usual  map[string]usualTime // by imageOf
}

// usualTime is an image's usual times: a build that came from the cache
// and one that rebuilt steps take very different times, so each is
// compared with its own kind.
type usualTime struct {
	Cached, Rebuilt, RebuiltP95 time.Duration
	CachedN, RebuiltN           int // builds behind each median
}

// of returns the usual time for builds like b, and the time above which
// b is slow for its kind (0: not known).
func (u usualTime) of(rebuilt bool) (usual, slow time.Duration) {
	if rebuilt {
		return u.Rebuilt, u.RebuiltP95
	}
	return u.Cached, 0
}

// recent returns a project's builds of the last week, newest first, with
// each image's usual time; computed at most once a minute per project.
func (s *Server) recent(ctx context.Context, p store.ProjectKey) (*cachedStats, error) {
	key := p.String()
	s.statsMu.Lock()
	c := s.stats[key]
	s.statsMu.Unlock()
	if c != nil && s.now().Sub(c.at) < statsFresh {
		return c, nil
	}
	bs, err := s.data.Builds(ctx, store.BuildQuery{Project: p, Since: s.now().Add(-statsFor), Light: true})
	if err != nil {
		return nil, err
	}
	c = &cachedStats{at: s.now(), builds: bs, usual: usualTimes(bs)}
	s.statsMu.Lock()
	if s.stats == nil {
		s.stats = map[string]*cachedStats{}
	}
	s.stats[key] = c
	s.statsMu.Unlock()
	return c, nil
}

// minForUsual is how many builds of a kind a "usual" time needs.
const minForUsual = 5

// minForP95 is how many rebuilds a p95 needs: with fewer, the slowest 5%
// is less than one build and the nearest-rank p95 is just the slowest.
const minForP95 = 20

// usualTimes is each image's median times over builds that finished,
// apart for builds from the cache and builds that rebuilt steps, keyed by
// imageOf: two Dockerfiles without a target are two images with their own
// times.
func usualTimes(bs []store.BuildRow) map[string]usualTime {
	return usualTimesBy(bs, imageOf)
}

// imageOf is a build's image as the store tells images apart: its target,
// or without one the repository it pushed to first.
func imageOf(b store.BuildRow) string {
	return store.ImageKey(b.Target, b.Images)
}

// usualTimesBy is usualTimes with the builds grouped by key.
func usualTimesBy(bs []store.BuildRow, key func(store.BuildRow) string) map[string]usualTime {
	cached, rebuilt := map[string][]time.Duration{}, map[string][]time.Duration{}
	for _, b := range bs {
		if b.Completed.IsZero() || b.Error != "" {
			continue
		}
		k := key(b)
		if b.Built > 0 {
			rebuilt[k] = append(rebuilt[k], b.Duration())
		} else {
			cached[k] = append(cached[k], b.Duration())
		}
	}
	out := map[string]usualTime{}
	for t, ds := range cached {
		u := out[t]
		u.Cached, u.CachedN = median(ds), len(ds)
		out[t] = u
	}
	for t, ds := range rebuilt {
		u := out[t]
		u.Rebuilt, u.RebuiltN = median(ds), len(ds)
		if len(ds) >= minForP95 {
			u.RebuiltP95 = percentile(ds, 95)
		}
		out[t] = u
	}
	return out
}

// ---- one build in a list ----

// BuildItem is a build as a row of a list.
type BuildItem struct {
	ID              int64
	URL             string
	Target          string
	Status          Status
	Why             string
	When            Stamp
	Dur, Delta      string
	Slower          bool
	Ref, SHA, Title string
	PipeURL         string
	PipeLabel       string
	Pipeline        uint64
	Quiet           bool // came from the cache: shown, but quietly
	// Picked: the build picked first to compare (picking a pair); Label
	// says what the row does then.
	Picked bool
	Label  string
}

func (s *Server) item(b store.BuildRow, usual map[string]usualTime, loc *time.Location) BuildItem {
	st := rowStatus(b)
	it := BuildItem{ID: b.ID, URL: fmt.Sprintf("/builds/%d", b.ID), Target: buildName(b.Target, b.Images), Status: st, Why: whyShort(b),
		When: clockStamp(b.Started, inZone(s.now(), loc)), Ref: b.Ref, SHA: short(b.SHA, 8), Title: b.Title, Pipeline: b.Pipeline, Quiet: st.Key == "cached"}
	if b.Pipeline != 0 {
		it.PipeURL, it.PipeLabel = "/pipelines/"+store.PipelineKey(b.Instance, b.Pipeline), "#"+strconv.FormatUint(b.Pipeline, 10)
	}
	if d := b.Duration(); d > 0 {
		it.Dur = duration(d)
		// A list marks only what is slow for its kind: slower than 19 in
		// 20 rebuilds of the image.
		if _, slow := usual[imageOf(b)].of(b.Built > 0); slow > 0 && d > slow && b.Error == "" {
			it.Slower = true
		}
	} else if b.Completed.IsZero() {
		it.Dur = "running"
	}
	return it
}

// delta says how a time compares with the usual, when it differs enough
// to matter: "+12 s", "−3.1 s".
func delta(d, usual time.Duration) (string, bool) {
	diff := d - usual
	abs := diff
	if abs < 0 {
		abs = -abs
	}
	if abs < time.Second || float64(abs) < 0.2*float64(usual) {
		return "", false
	}
	if diff > 0 {
		return "+" + duration(abs), true
	}
	return "−" + duration(abs), false
}

// clock is a time the way lists show it, in now's location (the reader's
// zone, see inZone): the time of day today, the day and time otherwise;
// "UTC" follows when that is the zone.
func clock(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	t = t.In(now.Location())
	out := t.Format("2 Jan 15:04")
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		out = t.Format("15:04")
	}
	if now.Location() == time.UTC {
		out += " UTC"
	}
	return out
}

// clockStamp is clock with the moment in UTC for a tooltip.
func clockStamp(t, now time.Time) Stamp {
	if t.IsZero() {
		return Stamp{}
	}
	return Stamp{Text: clock(t, now), UTC: t.UTC().Format("2006-01-02 15:04:05") + " UTC"}
}

// ---- the build list ----

// BuildsView is the list of a project's image builds.
type BuildsView struct {
	Shell
	F        Filters
	Branches []string
	Targets  []Option
	Outcomes []Option
	Items    []BuildItem
	Groups   []PipeGroup // when grouped by pipeline
	Older    string      // the next page's address
	Newest   string      // the first page's address, on a later page
	Mine     bool        // the "Mine" filter is offered (a person is signed in)
	// Picking two builds to compare: the button's address (into or out
	// of picking), and the first build picked.
	PickURL string
	Anchor  *BuildItem
	// Cancel leaves picking; Again picks the first build anew.
	Cancel, Again string
}

// Filters are the list's filters, as its address carries them.
type Filters struct {
	Branch, Outcome, Image string
	Mine, Group            bool
	Before                 int64
	// Pick: picking two builds to compare; A is the first one picked.
	Pick bool
	A    int64
}

// Option is one choice of a filter.
type Option struct {
	Value, Label string
	On           bool
	// Title is the label in full, when the label shortens it.
	Title string
}

// PipeGroup is a pipeline's builds on a page of the list.
type PipeGroup struct {
	URL, Label, Title, Ref, SHA string
	Items                       []BuildItem
	Rebuilt                     int
}

func filtersOf(r *http.Request) Filters {
	q := r.URL.Query()
	f := Filters{Branch: q.Get("branch"), Outcome: q.Get("outcome"), Image: q.Get("image"), Mine: q.Get("mine") == "1", Group: q.Get("group") == "pipeline",
		Pick: q.Get("pick") == "1"}
	f.Before, _ = strconv.ParseInt(q.Get("before"), 10, 64)
	if f.Pick {
		f.A, _ = strconv.ParseInt(q.Get("a"), 10, 64)
	}
	if _, ok := outcomeLabels[f.Outcome]; !ok {
		f.Outcome = ""
	}
	return f
}

// Query is the filters as an address, before excluded.
func (f Filters) Query() url.Values {
	v := url.Values{}
	if f.Branch != "" {
		v.Set("branch", f.Branch)
	}
	if f.Outcome != "" {
		v.Set("outcome", f.Outcome)
	}
	if f.Image != "" {
		v.Set("image", f.Image)
	}
	if f.Mine {
		v.Set("mine", "1")
	}
	if f.Group {
		v.Set("group", "pipeline")
	}
	if f.Pick {
		v.Set("pick", "1")
		if f.A > 0 {
			v.Set("a", strconv.FormatInt(f.A, 10))
		}
	}
	return v
}

// URL is the list's address with these filters.
func (f Filters) URL() string {
	if q := f.Query().Encode(); q != "" {
		return "/builds?" + q
	}
	return "/builds"
}

var outcomeLabels = map[string]string{"": "Any outcome", "rebuilt": "Not from cache", "cached": "From cache", "change": "Your change",
	"farm": "Build farm", "unclear": "Unclear", "failed": "Failed"}

// pageSize is the number of builds on a page of the list.
const pageSize = 50

func (s *Server) builds(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sh, err := s.projectPage(r, "builds")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.settleLater(r, sh.Project.ProjectKey)
	sh.Title = "Builds"
	f := filtersOf(r)
	// Picking the second build of a pair: the list shows the first one's
	// image in its project, so every row can be compared with it.
	var anchor *built
	if f.A > 0 {
		if anchor, err = s.anchor(r, f.A); err != nil {
			s.fail(w, err)
			return
		}
		if anchor == nil {
			f.A = 0
		} else {
			f.Image = imageKey(anchor.Page.Build.Target)
			pinProject(&sh, anchor.Page.Build.Attribution.Instance, projectOf(anchor.Page))
		}
	}
	v := &BuildsView{F: f, Mine: sh.Viewer != nil}
	for _, k := range []string{"", "rebuilt", "cached", "change", "farm", "unclear", "failed"} {
		v.Outcomes = append(v.Outcomes, Option{Value: k, Label: outcomeLabels[k], On: f.Outcome == k})
	}
	month := s.now().Add(-30 * 24 * time.Hour)
	if v.Branches, err = s.data.Branches(ctx, sh.Project.ProjectKey, month); err != nil {
		s.fail(w, err)
		return
	}
	targets, err := s.data.Targets(ctx, sh.Project.ProjectKey, month)
	if err != nil {
		s.fail(w, err)
		return
	}
	names := s.imageNames(ctx, sh.Project.ProjectKey, month)
	for _, t := range targets {
		v.Targets = append(v.Targets, Option{Value: imageKey(t), Label: imageName(t, names[t]), Title: imageTitle(t, names[t]), On: f.Image == imageKey(t)})
	}
	target, none := targetOf(f.Image)
	q := store.BuildQuery{Project: sh.Project.ProjectKey, Ref: f.Branch, Target: target, NoTarget: none, Outcome: f.Outcome, Limit: pageSize + 1}
	if f.Before > 0 {
		q.Before = time.UnixMilli(f.Before)
	}
	if f.Mine && sh.Viewer != nil {
		q.Author = sh.Viewer.Name
		if q.Author == "" {
			q.Author = sh.Viewer.Username
		}
	}
	rows, err := s.data.Builds(ctx, q)
	if err != nil {
		s.fail(w, err)
		return
	}
	st, err := s.recent(ctx, sh.Project.ProjectKey)
	if err != nil {
		s.fail(w, err)
		return
	}
	// cut: where the next page starts (the started time of the last build
	// this page covers); zero when no build is older.
	var cut time.Time
	if anchor != nil && anchor.Page.Build.Target == "" {
		// The first build picked has no target: only builds of its
		// repository are of its image. Read on until a page of them is
		// full or the builds run out (at most maxReads reads), so that a
		// busy other Dockerfile does not leave the page empty.
		const maxReads = 20
		var kept []store.BuildRow
		last := rows
		for reads := 1; ; reads++ {
			for _, b := range last {
				if sameImage(b.Target, b.Images, anchor.Page.Build.Target, anchor.Page.Build.Images) {
					kept = append(kept, b)
				}
			}
			if len(kept) > pageSize || len(last) <= pageSize || reads == maxReads {
				break
			}
			q.Before = last[len(last)-1].Started
			if last, err = s.data.Builds(ctx, q); err != nil {
				s.fail(w, err)
				return
			}
		}
		switch {
		case len(kept) > pageSize:
			cut, kept = kept[pageSize-1].Started, kept[:pageSize]
		case len(last) > pageSize:
			cut = last[len(last)-1].Started
		}
		rows = kept
	} else if len(rows) > pageSize {
		cut, rows = rows[pageSize-1].Started, rows[:pageSize]
	}
	if !cut.IsZero() {
		older := f.Query()
		older.Set("before", strconv.FormatInt(cut.UnixMilli(), 10))
		v.Older = "/builds?" + older.Encode()
	}
	if f.Before > 0 {
		v.Newest = f.URL()
	}
	for _, b := range rows {
		v.Items = append(v.Items, s.item(b, st.usual, zone(r)))
	}
	s.picking(v, anchor, zone(r))
	if f.Group {
		at := map[uint64]int{}
		for _, it := range v.Items {
			i, ok := at[it.Pipeline]
			if !ok {
				i = len(v.Groups)
				at[it.Pipeline] = i
				g := PipeGroup{URL: it.PipeURL, Label: it.PipeLabel, Title: it.Title, Ref: it.Ref, SHA: it.SHA}
				if g.Title == "" {
					g.Title = "Pipeline " + it.PipeLabel
				}
				v.Groups = append(v.Groups, g)
			}
			v.Groups[i].Items = append(v.Groups[i].Items, it)
			if !it.Quiet {
				v.Groups[i].Rebuilt++
			}
		}
	}
	v.Shell = sh
	s.render(w, "builds", http.StatusOK, v)
}

// anchor is the build picked first to compare, nil when it is not there
// or the reader may not see it (picking then starts over).
func (s *Server) anchor(r *http.Request, id int64) (*built, error) {
	p, err := s.data.BuildPage(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !s.mayBuild(r, p)) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.explain(r.Context(), p)
}

// picking turns the list's rows into the choice of a pair: a row picks the
// first build, then a row opens its comparison with the first one.
func (s *Server) picking(v *BuildsView, anchor *built, loc *time.Location) {
	f := v.F
	out := f
	out.Pick, out.A = false, 0
	if !f.Pick {
		in := f
		in.Pick, in.Before = true, 0
		v.PickURL = in.URL()
		return
	}
	v.PickURL, v.Cancel = out.URL(), out.URL()
	again := f
	again.A = 0
	v.Again = again.URL()
	if anchor != nil {
		b := anchor.Page.Build
		it := BuildItem{ID: anchor.Page.ID, URL: fmt.Sprintf("/builds/%d", anchor.Page.ID), Target: buildName(b.Target, b.Images),
			Status: statusOf(anchor.Page.Reason.Kind, b.Error, anchor.Built, b.Completed.IsZero()), When: clockStamp(b.Started, inZone(s.now(), loc))}
		if p := anchor.Page.Pipeline; p != nil {
			it.PipeLabel = "#" + strconv.FormatUint(p.ID, 10)
		}
		v.Anchor = &it
	}
	for i := range v.Items {
		it := &v.Items[i]
		switch {
		case v.Anchor == nil:
			pick := f
			pick.A = it.ID
			it.URL, it.Label = pick.URL(), "pick "+it.Target+" "+it.PipeLabel+" to compare"
		case it.ID == v.Anchor.ID:
			it.Picked, it.URL = true, ""
		default:
			it.URL, it.Label = fmt.Sprintf("/builds/%d/compare?with=%d", v.Anchor.ID, it.ID), "compare with "+it.Target+" "+it.PipeLabel
		}
	}
}

// ---- reasons, grouped ----

// Reason is one cause of rebuilds over a window: builds that were rebuilt
// for the same reason, the time they took.
type Reason struct {
	Status Status
	Text   string
	Images []string
	More   int
	N      int
	Lost   time.Duration
	Last   string
	URL    string
}

// fingerprint groups builds rebuilt for the same reason.
func fingerprint(b store.BuildRow, st Status) string {
	switch st.Key {
	case "unclear":
		// One group per text reasonText gives: never one build's words for
		// the others.
		switch {
		case b.Reason != history.CacheElsewhere:
			return "unclear:nothing to compare"
		case b.Old == "":
			return "unclear:here"
		case replacedHere(b):
			return "unclear:replaced"
		}
		return "unclear:elsewhere"
	case "failed":
		return "failed:" + firstLine(b.Error)
	case "farm":
		switch {
		case b.Reason == history.OtherNode && replacedHere(b) && b.OldPersists:
			return "farm:" + string(history.NotInCache)
		case b.Reason == history.OtherNode && replacedHere(b):
			return "farm:replaced"
		}
		return "farm:" + string(b.Reason)
	}
	switch b.Reason {
	case history.FirstBuild:
		return "first"
	case history.FilesChanged:
		// Unverified paths are candidates, and the label does not name
		// them: one group, or equal-looking groups would multiply.
		if !viaVerified(b.Via) {
			return "files:?"
		}
		return "files:" + strings.Join(b.Paths, ",")
	}
	return string(b.Reason) + ":" + b.ReasonStep
}

func reasonText(b store.BuildRow, st Status) string {
	switch st.Key {
	case "unclear":
		if b.Reason == history.CacheElsewhere {
			if b.Old == "" {
				return "Rebuilt here although nothing seems to have changed; files or an evicted cache, unclear"
			}
			if replacedHere(b) {
				return "The last build of the image ran under this node's previous daemon; whether its files changed is unclear"
			}
			return "The last build of the image ran on another node; whether its files changed is unclear"
		}
		return "Steps ran with nothing on record to compare them with"
	case "failed":
		return "Failed: " + clip(failureText(b.Error), 120)
	case "farm":
		switch {
		case b.Reason == history.NotInCache, b.Reason == history.OtherNode && replacedHere(b) && b.OldPersists:
			return "No longer in the node's cache: cleaned up, or the node was replaced"
		case b.Reason == history.OtherNode && replacedHere(b):
			return "The node's build daemon was replaced and its cache went with it"
		}
		return "Cached on another node: the job ran where the cache was not"
	}
	switch b.Reason {
	case history.FirstBuild:
		return "First build of an image on record"
	case history.FilesChanged:
		if viaVerified(b.Via) && len(b.Paths) > 0 {
			return "Files changed: " + strings.Join(clipList(b.Paths, 3), ", ")
		}
		return "The files a step copies changed"
	case history.StepChanged, history.NewStep:
		return "Dockerfile changed: " + clip(b.ReasonStep, 80)
	case history.ArgChanged:
		return "Build argument " + b.Arg + " changed"
	case history.BaseImageChanged:
		return "Base image changed: " + clip(b.ReasonStep, 80)
	}
	return string(b.Reason)
}

// reasonImage tells a reason's images apart: the image key, and for
// builds without a target the repository they pushed first (two
// Dockerfiles without targets are two images).
func reasonImage(b store.BuildRow) string {
	k := imageKey(b.Target)
	if rs := history.Repositories(b.Images); b.Target == "" && len(rs) > 0 {
		k += "\x00" + rs[0]
	}
	return k
}

// reasons groups the rebuilt builds of bs, the most time first.
func (s *Server) reasons(bs []store.BuildRow, limit int, loc *time.Location) []Reason {
	at := map[string]int{}
	var out []Reason
	images := map[string]map[string]string{} // image (key, repository) -> name
	last := map[string]time.Time{}
	for _, b := range bs {
		st := rowStatus(b)
		if st.Key == "cached" || st.Key == "running" {
			continue
		}
		fp := fingerprint(b, st)
		i, ok := at[fp]
		if !ok {
			i = len(out)
			at[fp] = i
			q := url.Values{"outcome": {st.Key}}
			out = append(out, Reason{Status: st, Text: reasonText(b, st), URL: "/builds?" + q.Encode()})
			images[fp] = map[string]string{}
		}
		out[i].N++
		out[i].Lost += b.BuiltTime
		images[fp][reasonImage(b)] = buildName(b.Target, b.Images)
		if b.Started.After(last[fp]) {
			last[fp] = b.Started
		}
	}
	for fp, i := range at {
		var names []string
		for _, k := range sortedKeys(images[fp]) {
			names = append(names, images[fp][k])
		}
		if len(names) > 5 {
			out[i].More, names = len(names)-5, names[:5]
		}
		out[i].Images, out[i].Last = names, clock(last[fp], inZone(s.now(), loc))
		if len(images[fp]) == 1 {
			key, _, _ := strings.Cut(sortedKeys(images[fp])[0], "\x00")
			q := url.Values{"outcome": {out[i].Status.Key}, "image": {key}}
			out[i].URL = "/builds?" + q.Encode()
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Lost != out[j].Lost {
			return out[i].Lost > out[j].Lost
		}
		return out[i].N > out[j].N
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ---- images ----

// ImageRow is one target over a window.
type ImageRow struct {
	Name, URL  string
	Title      string // every repository, when Name counts them
	Strip      []StripCell
	Usual, P95 string // of the rebuilds; P95 only from minForP95 of them
	Rebuilds   int
	Builds     int
	Cached     int // percent fully from the cache
	Lost       string
}

// StripCell is one build in an image's strip.
type StripCell struct {
	URL, Title string
	Status     Status
}

func (s *Server) imageRows(bs []store.BuildRow, since time.Time, loc *time.Location) []ImageRow {
	by := map[string][]store.BuildRow{}
	for _, b := range bs {
		if b.Started.Before(since) {
			continue
		}
		by[b.Target] = append(by[b.Target], b)
	}
	var out []ImageRow
	for _, t := range sortedKeys(by) {
		list := by[t] // newest first
		var repos []string
		for _, b := range list {
			repos = append(repos, b.Images...)
		}
		rs := history.Repositories(repos)
		row := ImageRow{Name: imageName(t, rs), Title: imageTitle(t, rs), URL: "/images/" + url.PathEscape(imageKey(t)), Builds: len(list)}
		var ds []time.Duration
		cached := 0
		var lost time.Duration
		for _, b := range list {
			st := rowStatus(b)
			if st.Key == "cached" {
				cached++
			}
			lost += b.BuiltTime
			if !b.Completed.IsZero() && b.Error == "" && b.Built > 0 {
				ds = append(ds, b.Duration())
			}
		}
		row.Cached = cached * 100 / max(1, len(list))
		row.Rebuilds = len(ds)
		if len(ds) >= minForUsual {
			row.Usual = duration(median(ds))
		}
		if len(ds) >= minForP95 {
			row.P95 = duration(percentile(ds, 95))
		}
		row.Lost = duration(lost)
		n := min(30, len(list))
		for i := n - 1; i >= 0; i-- {
			b := list[i]
			st := rowStatus(b)
			row.Strip = append(row.Strip, StripCell{URL: fmt.Sprintf("/builds/%d", b.ID), Status: st,
				Title: st.Label + " · " + clock(b.Started, inZone(s.now(), loc)) + " · " + duration(b.Duration())})
		}
		out = append(out, row)
	}
	return out
}

// imageKey is a target in an address: builds without a target are "-".
func imageKey(t string) string {
	if t == "" {
		return "-"
	}
	return t
}

func percentile(ds []time.Duration, p int) time.Duration { return store.Percentile(ds, p) }

// ---- the insights ----

// InsightsView is what needs attention in a project now.
type InsightsView struct {
	Shell
	Window      string // 24h or 7d
	WindowLabel string
	Builds      int
	Cached      int // percent
	Lost        string
	Farm        int // percent of the rebuild time the build farm caused (history.FarmKinds)
	Unclear     int // percent of it where files or node is unclear
	Reasons     []Reason
	MoreReasons string
	Running     int
	Empty       bool
}

func windowOf(r *http.Request) (string, time.Duration, string) {
	if r.URL.Query().Get("window") == "7d" {
		return "7d", 7 * 24 * time.Hour, "the last 7 days"
	}
	return "24h", 24 * time.Hour, "the last 24 hours"
}

func (s *Server) insights(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sh, err := s.projectPage(r, "insights")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.settleLater(r, sh.Project.ProjectKey)
	sh.Title = "Insights"
	v := &InsightsView{}
	var span time.Duration
	v.Window, span, v.WindowLabel = windowOf(r)
	st, err := s.recent(ctx, sh.Project.ProjectKey)
	if err != nil {
		s.fail(w, err)
		return
	}
	since := s.now().Add(-span)
	var in []store.BuildRow
	cached := 0
	var lost, farm, unclear time.Duration
	for _, b := range st.builds {
		if b.Started.Before(since) {
			continue
		}
		in = append(in, b)
		stt := rowStatus(b)
		switch {
		case stt.Key == "running":
			v.Running++
		case stt.Key == "cached":
			cached++
		}
		lost += b.BuiltTime
		switch stt.Key {
		case "farm":
			farm += b.BuiltTime
		case "unclear":
			unclear += b.BuiltTime
		}
	}
	v.Builds, v.Empty = len(in), len(in) == 0
	v.Cached = cached * 100 / max(1, len(in))
	v.Lost = duration(lost)
	if lost > 0 {
		v.Farm, v.Unclear = int(100*farm/lost), int(100*unclear/lost)
	}
	all := s.reasons(in, 0, zone(r))
	v.Reasons = all
	if len(all) > 6 {
		v.Reasons, v.MoreReasons = all[:6], fmt.Sprintf("%d more reasons", len(all)-6)
	}
	if !v.Empty && !sh.NoProjects {
		sh.Desc = fmt.Sprintf("%d image build%s in %s, %d%% fully from the cache.", v.Builds, plural(v.Builds), v.WindowLabel, v.Cached)
		if v.Lost != "" && v.Cached != 100 {
			sh.Desc += " Rebuilds took " + v.Lost + " of build time."
		}
	}
	v.Shell = sh
	s.render(w, "insights", http.StatusOK, v)
}

// ---- images ----

// ImagesView lists the project's images over a window.
type ImagesView struct {
	Shell
	Window, WindowLabel string
	Images              []ImageRow
}

func (s *Server) images(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sh, err := s.projectPage(r, "images")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.settleLater(r, sh.Project.ProjectKey)
	sh.Title = "Images"
	v := &ImagesView{}
	var span time.Duration
	v.Window, span, v.WindowLabel = windowOf(r)
	st, err := s.recent(ctx, sh.Project.ProjectKey)
	if err != nil {
		s.fail(w, err)
		return
	}
	v.Images = s.imageRows(st.builds, s.now().Add(-span), zone(r))
	v.Shell = sh
	s.render(w, "images", http.StatusOK, v)
}

// ImageView is one image's builds over a window.
type ImageView struct {
	Shell
	Name, Key           string
	Title               string // every repository, when Name counts them
	Window, WindowLabel string
	Others              []Option
	Builds              int
	UsualCached         string
	UsualRebuilt, P95   string
	Cached              int
	Chart               *Chart
	Steps               []StepRowView
	Reasons             []Reason
	Recent              []BuildItem
	ListURL             string
	PickURL             string // picks two of its builds to compare
	Year                *YearView
}

// YearView is an image's last year, week by week, from the daily figures
// the console keeps after the builds themselves are gone.
type YearView struct {
	Cached       int // percent
	UsualRebuilt string
	Weeks        []Week
	From, To     string
}

// Week is one column: its height is the week's builds against the
// busiest week's, split by outcome.
type Week struct {
	H, Title string
	Segs     []WeekSeg
}

// WeekSeg is one outcome's share of a week (percent of the column).
type WeekSeg struct {
	Status Status
	H      string
}

// StepRowView is a step that ran instead of coming from the cache.
type StepRowView struct {
	Name    string
	Ran, Of int // ran in Ran of the Of builds of the window
	Time    string
	W       int // percent of the most often run
}

// Chart is an image's builds over time: one dot each, coloured by status,
// or one per status and slice of the window when there are many.
type Chart struct {
	Grouped    bool
	Dots       []Dot
	UsualY     string
	UsualLabel string
	MaxLabel   string
	From, To   string
}

// Dot is one build on the chart (percentages of the plot).
type Dot struct {
	X, Y, URL, Title string
	Status           Status
}

func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	key := r.PathValue("target")
	target, none := targetOf(key)
	sh, err := s.projectPage(r, "images")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.settleLater(r, sh.Project.ProjectKey)
	names := s.imageNames(ctx, sh.Project.ProjectKey, s.now().Add(-30*24*time.Hour))
	v := &ImageView{Name: imageName(target, names[target]), Title: imageTitle(target, names[target]), Key: key}
	sh.Title = v.Name
	var span time.Duration
	v.Window, span, v.WindowLabel = windowOf(r)
	year := r.URL.Query().Get("window") == "1y"
	switch {
	case year:
		v.Window, span, v.WindowLabel = "1y", 365*24*time.Hour, "the last year"
	case r.URL.Query().Get("window") == "30d":
		v.Window, span, v.WindowLabel = "30d", 30*24*time.Hour, "the last 30 days"
	}
	since := s.now().Add(-span)
	var (
		bs   []store.BuildRow
		days []store.ImageDay
	)
	if year {
		days, err = s.data.ImageDays(ctx, sh.Project.ProjectKey, target, since)
	} else {
		bs, err = s.data.Builds(ctx, store.BuildQuery{Project: sh.Project.ProjectKey, Target: target, NoTarget: none, Since: since, Light: true})
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	targets, err := s.data.Targets(ctx, sh.Project.ProjectKey, s.now().Add(-30*24*time.Hour))
	if err != nil {
		s.fail(w, err)
		return
	}
	known := len(days) > 0
	for _, t := range targets {
		known = known || t == target
	}
	if !known {
		// An image's address names no project: one opened from a link, or
		// in a tab after switching projects, may be another project's.
		var elsewhere []Option
		for _, p := range sh.Projects {
			if p.ProjectKey == sh.Project.ProjectKey {
				continue
			}
			ts, err := s.data.Targets(ctx, p.ProjectKey, s.now().Add(-30*24*time.Hour))
			if err != nil {
				s.fail(w, err)
				return
			}
			if slices.Contains(ts, target) {
				label := p.Path
				if label == "" {
					label = fmt.Sprintf("project %d", p.ID)
				}
				elsewhere = append(elsewhere, Option{Value: "/projects/" + p.Instance + "/" + strconv.FormatUint(p.ID, 10) + "?to=" + url.QueryEscape(r.URL.Path), Label: label})
			}
		}
		if len(elsewhere) == 1 {
			http.Redirect(w, r, elsewhere[0].Value, http.StatusFound)
			return
		}
		sh.Title = "Not found"
		s.render(w, "notfound", http.StatusNotFound, NotFound{Shell: sh, What: "image", Elsewhere: elsewhere})
		return
	}
	for _, t := range targets {
		v.Others = append(v.Others, Option{Value: "/images/" + url.PathEscape(imageKey(t)), Label: imageName(t, names[t]), Title: imageTitle(t, names[t]), On: t == target})
	}
	v.Builds = len(bs)
	if year {
		for _, d := range days {
			v.Builds += d.Builds
		}
		v.Year = s.year(days, since)
	}
	if len(bs) == 0 {
		v.ListURL = Filters{Image: key}.URL()
		v.PickURL = Filters{Image: key, Pick: true}.URL()
		v.Shell = sh
		s.render(w, "image", http.StatusOK, v)
		return
	}
	cached := 0
	var rebuilt []time.Duration
	for _, b := range bs {
		if rowStatus(b).Key == "cached" {
			cached++
		}
		if !b.Completed.IsZero() && b.Error == "" && b.Built > 0 {
			rebuilt = append(rebuilt, b.Duration())
		}
	}
	v.Cached = cached * 100 / max(1, len(bs))
	// The page's own figures are over all its builds: without a target,
	// those of every Dockerfile without one, as the page names them all.
	ut := usualTimesBy(bs, func(b store.BuildRow) string { return b.Target })[target]
	if ut.CachedN >= minForUsual {
		v.UsualCached = duration(ut.Cached)
	}
	if ut.RebuiltN >= minForUsual {
		v.UsualRebuilt = duration(ut.Rebuilt)
	}
	if len(rebuilt) >= minForP95 {
		v.P95 = duration(percentile(rebuilt, 95))
	}
	first := bs[len(bs)-1].Started
	if first.After(since) {
		since = first
	}
	usualLine := time.Duration(0) // the chart's dashed line, only from enough rebuilds
	if ut.RebuiltN >= minForUsual {
		usualLine = ut.Rebuilt
	}
	v.Chart = s.chart(bs, since, usualLine, zone(r))
	counts, err := s.data.StepCounts(ctx, sh.Project.ProjectKey, target, since, 8)
	if err != nil {
		s.fail(w, err)
		return
	}
	most := 1
	for _, c := range counts {
		most = max(most, c.Ran)
	}
	for _, c := range counts {
		v.Steps = append(v.Steps, StepRowView{Name: c.Name, Ran: c.Ran, Of: v.Builds, Time: duration(c.RanTime), W: c.Ran * 100 / most})
	}
	v.Reasons = s.reasons(bs, 5, zone(r))
	latest, err := s.data.Builds(ctx, store.BuildQuery{Project: sh.Project.ProjectKey, Target: target, NoTarget: none, Limit: 10})
	if err != nil {
		s.fail(w, err)
		return
	}
	st := usualTimes(bs)
	for _, b := range latest {
		v.Recent = append(v.Recent, s.item(b, st, zone(r)))
	}
	v.ListURL = Filters{Image: key}.URL()
	v.PickURL = Filters{Image: key, Pick: true}.URL()
	v.Shell = sh
	s.render(w, "image", http.StatusOK, v)
}

// year draws the daily figures week by week, weeks starting on Monday.
func (s *Server) year(days []store.ImageDay, since time.Time) *YearView {
	if len(days) == 0 {
		return nil
	}
	monday := func(t time.Time) time.Time {
		t = t.UTC().Truncate(24 * time.Hour)
		return t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7))
	}
	first := monday(since)
	n := int(monday(s.now()).Sub(first)/(7*24*time.Hour)) + 1
	weeks := make([]store.ImageDay, n)
	var p50s []time.Duration
	y := &YearView{}
	builds, cached := 0, 0
	for _, d := range days {
		i := int(monday(d.Day).Sub(first) / (7 * 24 * time.Hour))
		if i < 0 || i >= n {
			continue
		}
		wk := &weeks[i]
		wk.Builds += d.Builds
		wk.Cached += d.Cached
		wk.Change += d.Change
		wk.Farm += d.Farm
		wk.Unclear += d.Unclear
		wk.Fail += d.Fail
		builds, cached = builds+d.Builds, cached+d.Cached
		if d.RebuiltP50 > 0 {
			p50s = append(p50s, d.RebuiltP50)
		}
	}
	y.Cached = cached * 100 / max(1, builds)
	if len(p50s) > 0 {
		y.UsualRebuilt = duration(median(p50s))
	}
	most := 1
	for _, wk := range weeks {
		most = max(most, wk.Builds)
	}
	pct := func(a, b int) string { return strconv.FormatFloat(100*float64(a)/float64(max(1, b)), 'f', 1, 64) }
	for i, wk := range weeks {
		start := first.AddDate(0, 0, 7*i)
		col := Week{H: pct(wk.Builds, most), Title: "Week of " + start.Format("2 Jan 2006") + " · no builds"}
		if wk.Builds > 0 {
			col.Title = fmt.Sprintf("Week of %s · %d build%s, %d%% from cache", start.Format("2 Jan 2006"), wk.Builds, plural(wk.Builds), wk.Cached*100/wk.Builds)
		}
		for _, sc := range []struct {
			key string
			n   int
		}{{"cached", wk.Cached}, {"change", wk.Change}, {"farm", wk.Farm}, {"unclear", wk.Unclear}, {"failed", wk.Fail}} {
			if sc.n > 0 {
				col.Segs = append(col.Segs, WeekSeg{Status: statuses[sc.key], H: pct(sc.n, wk.Builds)})
			}
		}
		y.Weeks = append(y.Weeks, col)
	}
	y.From, y.To = first.Format("Jan 2006"), s.now().UTC().Format("2 Jan 2006")
	return y
}

// maxDots is the most builds the chart draws one by one; more are grouped
// by time, one dot per status and slice of the window.
const maxDots = 300

// chartSlices is how many slices of the window grouped builds fall into.
const chartSlices = 90

func (s *Server) chart(bs []store.BuildRow, since time.Time, usual time.Duration, loc *time.Location) *Chart {
	from, to := since, inZone(s.now(), loc)
	span := to.Sub(from)
	if span <= 0 {
		return nil
	}
	type point struct {
		at    time.Time
		d     time.Duration
		st    Status
		url   string
		title string
	}
	var pts []point
	if len(bs) <= maxDots {
		for _, b := range bs {
			if d := b.Duration(); d > 0 {
				st := rowStatus(b)
				pts = append(pts, point{b.Started, d, st, fmt.Sprintf("/builds/%d", b.ID), st.Label + " · " + duration(d) + " · " + clock(b.Started, to)})
			}
		}
	} else {
		type key struct {
			slice  int
			status string
		}
		groups := map[key][]store.BuildRow{}
		for _, b := range bs {
			if b.Duration() <= 0 {
				continue
			}
			k := key{int(float64(chartSlices) * float64(b.Started.Sub(from)) / float64(span)), rowStatus(b).Key}
			groups[k] = append(groups[k], b)
		}
		for k, g := range groups {
			ds := make([]time.Duration, len(g))
			for i, b := range g {
				ds[i] = b.Duration()
			}
			mid := median(ds)
			st := rowStatus(g[0])
			start := from.Add(time.Duration(float64(span) * float64(k.slice) / chartSlices))
			pts = append(pts, point{start.Add(span / chartSlices / 2), mid, st, Filters{Image: imageKey(g[0].Target), Outcome: st.Key}.URL(),
				fmt.Sprintf("%s · %d build%s from %s · median %s", st.Label, len(g), plural(len(g)), clock(start, to), duration(mid))})
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].at.Before(pts[j].at) })
	}
	var top time.Duration
	for _, p := range pts {
		top = max(top, p.d)
	}
	if top <= 0 {
		return nil
	}
	c := &Chart{MaxLabel: duration(top), From: clock(from, to), To: clock(to, to), UsualLabel: duration(usual), Grouped: len(bs) > maxDots}
	if usual <= 0 {
		c.UsualLabel = ""
	}
	pct := func(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }
	c.UsualY = pct(100 - 100*float64(min(usual, top))/float64(top))
	for _, p := range pts {
		c.Dots = append(c.Dots, Dot{X: pct(100 * float64(p.at.Sub(from)) / float64(span)), Y: pct(100 - 100*float64(p.d)/float64(top)),
			URL: p.url, Status: p.st, Title: p.title})
	}
	return c
}

// sortedKeys returns a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
