package web

import (
	"fmt"
	"html/template"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// Cat is what a job's build comes down to, as the list and the chips say
// it: never by colour alone, always with an icon and words.
type Cat struct {
	Key   string // cached, first, step, files, node, unknown, failed, canceled, running
	Label string
	Short string
	Tone  string // the verdict tone: cached, first, built, node, unknown, failed
	Icon  string
}

var cats = map[string]Cat{
	"cached":   {"cached", "All from cache", "cached", "cached", "v_cached"},
	"first":    {"first", "First build", "first", "first", "v_first"},
	"compare":  {"first", "Not compared yet", "not compared", "first", "v_first"},
	"step":     {"step", "Dockerfile changed", "dockerfile", "built", "v_step"},
	"arg":      {"step", "Build argument changed", "build arg", "built", "v_step"},
	"base":     {"step", "Base image changed", "base image", "built", "v_step"},
	"files":    {"files", "Files changed", "files", "built", "v_files"},
	"node":     {"node", "Other node — not your change", "other node", "node", "v_node"},
	"unknown":  {"unknown", "Files or node — unclear", "unclear", "unknown", "v_unknown"},
	"evicted":  {"unknown", "Not in the cache", "evicted", "unknown", "v_unknown"},
	"failed":   {"failed", "Failed", "failed", "failed", "warn"},
	"canceled": {"canceled", "Canceled", "canceled", "first", "x"},
	"running":  {"running", "Running", "running", "first", "clock"},
	"nobuild":  {"none", "No build on this box", "no build", "first", "job"},
}

// retriedOf finds the earlier attempts of retried jobs: a retry has the
// job's name and a higher job id.
func retriedOf[T any](items []T, key func(T) (name string, id uint64)) map[uint64]bool {
	latest := map[string]uint64{}
	for _, it := range items {
		if n, id := key(it); id > latest[n] {
			latest[n] = id
		}
	}
	out := map[uint64]bool{}
	for _, it := range items {
		if n, id := key(it); id < latest[n] {
			out[id] = true
		}
	}
	return out
}

// canceled says whether a build or step error is a cancellation (the job
// was canceled or its client went away), not a failure of the Dockerfile.
func canceled(err string) bool {
	return strings.HasSuffix(err, "context canceled") || strings.Contains(err, "code = Canceled")
}

// catOf classifies a build by its reason, error and executed steps.
func catOf(kind history.ReasonKind, buildErr string, built int) Cat {
	switch {
	case canceled(buildErr):
		return cats["canceled"]
	case buildErr != "":
		return cats["failed"]
	}
	switch kind {
	case history.FirstBuild:
		return cats["first"]
	case history.StepChanged, history.NewStep:
		return cats["step"]
	case history.ArgChanged:
		return cats["arg"]
	case history.BaseImageChanged:
		return cats["base"]
	case history.FilesChanged:
		return cats["files"]
	case history.OtherNode:
		return cats["node"]
	case history.CacheElsewhere:
		return cats["unknown"]
	case history.NotInCache:
		return cats["evicted"]
	}
	if built > 0 {
		return cats["compare"]
	}
	return cats["cached"]
}

// Filters of the pipeline list.
var filters = []struct{ Key, Label string }{{"", "All"}, {"rebuilt", "Rebuilt"}, {"node", "Node-related"}}

func passes(filter string, c Cat) bool {
	switch filter {
	case "":
		return true
	case "rebuilt":
		return c.Key != "cached" && c.Key != "running"
	case "node":
		return c.Key == "node" || c.Key == "unknown"
	}
	return c.Key == filter
}

// filterLabel names a filter that is not one of the list's buttons.
func filterLabel(f string) string {
	for _, c := range cats {
		if c.Key == f {
			if f == "step" {
				return "Dockerfile changed"
			}
			if f == "unknown" {
				return "Files or node — unclear"
			}
			return c.Label
		}
	}
	return f
}

// ---- icons ----

var icons = map[string]string{
	"pipes":      `<path d="M2.5 4h11M2.5 8h11M2.5 12h7"/>`,
	"insights":   `<path d="M2.5 13.5h11M4.5 11V8M8 11V4M11.5 11V6.5"/>`,
	"nodes":      `<rect x="2.5" y="2.5" width="11" height="4.5" rx="1.2"/><rect x="2.5" y="9" width="11" height="4.5" rx="1.2"/><path d="M5 4.75h.01M5 11.25h.01"/>`,
	"search":     `<circle cx="7" cy="7" r="4.3"/><path d="m10.3 10.3 3.2 3.2"/>`,
	"sun":        `<circle cx="8" cy="8" r="2.8"/><path d="M8 1.8v1.4M8 12.8v1.4M1.8 8h1.4M12.8 8h1.4M3.6 3.6l1 1M11.4 11.4l1 1M3.6 12.4l1-1M11.4 4.6l1-1"/>`,
	"moon":       `<path d="M12.8 9.6A5.2 5.2 0 0 1 6.4 3.2a5.2 5.2 0 1 0 6.4 6.4Z"/>`,
	"kbd":        `<rect x="1.8" y="4" width="12.4" height="8" rx="1.5"/><path d="M4.5 6.6h.01M7 6.6h.01M9.5 6.6h.01M12 6.6h-.5M5.5 9.4h5"/>`,
	"side":       `<rect x="2" y="2.5" width="12" height="11" rx="1.8"/><path d="M6 2.5v11"/>`,
	"chev":       `<path d="m6 4 4 4-4 4"/>`,
	"chevD":      `<path d="m4 6 4 4 4-4"/>`,
	"chevU":      `<path d="m4 10 4-4 4 4"/>`,
	"back":       `<path d="M9.5 3.5 5 8l4.5 4.5"/>`,
	"ext":        `<path d="M6.5 3H3.5v9.5H13V9.5M9 3h4v4M13 3 7.5 8.5"/>`,
	"copy":       `<rect x="5.5" y="5.5" width="8" height="8" rx="1.5"/><path d="M10.5 5.5V3.8c0-.7-.6-1.3-1.3-1.3H3.8c-.7 0-1.3.6-1.3 1.3v5.4c0 .7.6 1.3 1.3 1.3h1.7"/>`,
	"x":          `<path d="m4 4 8 8M12 4l-8 8"/>`,
	"check":      `<path d="m3.5 8.4 2.8 2.8 6.2-6.4"/>`,
	"log":        `<path d="M3 3.5h10v9H3z"/><path d="M5.5 6.5 7 8l-1.5 1.5M8.5 9.5h2"/>`,
	"filter":     `<path d="M2.5 4h11M4.5 8h7M6.5 12h3"/>`,
	"pipe":       `<circle cx="3.5" cy="8" r="1.6"/><circle cx="12.5" cy="8" r="1.6"/><path d="M5.1 8h5.8"/>`,
	"job":        `<rect x="2.5" y="3" width="11" height="10" rx="1.5"/><path d="M5 6.5h6M5 9.5h3.5"/>`,
	"warn":       `<path d="M8 2.2 14.2 13H1.8Z"/><path d="M8 6.5v3M8 11.3h.01"/>`,
	"info":       `<circle cx="8" cy="8" r="5.8"/><path d="M8 7.3v3.6M8 5.2h.01"/>`,
	"v_cached":   `<circle cx="8" cy="8" r="5.6"/><path d="m5.5 8.2 1.8 1.8 3.3-3.6"/>`,
	"v_first":    `<circle cx="8" cy="8" r="5.6" stroke-dasharray="2.3 2.1"/><path d="M8 5.8v4.4M5.8 8h4.4"/>`,
	"v_step":     `<path d="M4 2.5h5.3L12 5.2v8.3H4z"/><path d="M6.2 8.3h3.6M8 6.5v3.6"/>`,
	"v_files":    `<path d="M2.5 4.2h4l1.3 1.5h5.7v7H2.5z"/>`,
	"v_node":     `<path d="M2.5 5h8.5m0 0-2-2m2 2-2 2M13.5 11H5m0 0 2-2m-2 2 2 2"/>`,
	"v_unknown":  `<circle cx="8" cy="8" r="5.6"/><path d="M6.4 6.5a1.7 1.7 0 1 1 2.4 1.5c-.5.3-.8.6-.8 1.2M8 11.1h.01"/>`,
	"s_built":    `<circle cx="8" cy="8" r="3.4" fill="currentColor" stroke="none"/>`,
	"s_cached":   `<path d="m3.5 8.4 2.8 2.8 6.2-6.4"/>`,
	"s_received": `<path d="M13 8H4m0 0 3.5-3.5M4 8l3.5 3.5"/>`,
	"s_image":    `<rect x="3.5" y="3.5" width="9" height="9" rx="1.5"/>`,
	"up":         `<path d="M8 13V3.5m0 0L4.5 7M8 3.5 11.5 7"/>`,
	"clock":      `<circle cx="8" cy="8" r="5.5"/><path d="M8 5v3.2l2 1.3"/>`,
	"dfile":      `<path d="M4 2.5h5.5L12 5v8.5H4z"/><path d="M6 8h4M6 10.5h2.5"/>`,
	"tl":         `<path d="M2.5 4h6M4.5 8h7M7 12h6.5"/>`,
	"steps":      `<path d="M5.5 4h8M5.5 8h8M5.5 12h8M2.5 4h.01M2.5 8h.01M2.5 12h.01"/>`,
	"sum":        `<rect x="2.5" y="2.5" width="11" height="11" rx="1.5"/><path d="M5 10.5V8M8 10.5V5.5M11 10.5V7"/>`,
}

// icon renders a named icon as inline SVG (stroke icons, 16-unit grid).
func icon(name string, size int, class ...string) template.HTML {
	body, ok := icons[name]
	if !ok {
		return ""
	}
	c := "i"
	if len(class) > 0 && class[0] != "" {
		c += " " + class[0]
	}
	return template.HTML(fmt.Sprintf(`<svg class="%s" width="%d" height="%d" viewBox="0 0 16 16" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">%s</svg>`, c, size, size, body))
}

// stateIcon is the icon of a step state.
func stateIcon(state string) string {
	switch state {
	case "built", "cached", "received", "image":
		return "s_" + state
	case "failed":
		return "warn"
	case "canceled":
		return "x"
	}
	return "s_built"
}

var stateLabels = map[string]string{"built": "built", "cached": "from cache", "received": "from another job", "image": "base image", "failed": "failed", "canceled": "canceled"}

// ---- times ----

// dur prints a duration the way the pages do: 0.4 s, 12 s, 1 min 16 s.
func dur(d time.Duration) string { return duration(d) }

// pct prints a share as a CSS percentage with two decimals.
func pctOf(part, whole time.Duration) float64 {
	if whole <= 0 {
		return 0
	}
	return math.Max(0, math.Min(100, 100*float64(part)/float64(whole)))
}

func when(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2 Jan 15:04")
}

func shortID(id uint64) string {
	s := strconv.FormatUint(id, 10)
	if len(s) > 6 {
		return s[len(s)-6:]
	}
	return s
}

// shortDaemon is the last part of a daemon pod's name ("glab-buildkit-4hxsv"
// -> "4hxsv"): the part that tells daemons of one release apart.
func shortDaemon(name string) string {
	if i := strings.LastIndex(name, "-"); i >= 0 && i < len(name)-1 {
		return name[i+1:]
	}
	return name
}

// ---- jobs and their times ----

// jobTimes is when a job ran and how long, from GitLab's API, the job pod or,
// failing both, its builds.
type jobTimes struct {
	Start, End time.Time
	Total      time.Duration
	Known      bool // the job's own times, not only its builds'
}

func timesOf(j store.JobInfo, buildStart, buildEnd time.Time) jobTimes {
	t := jobTimes{Start: j.Started, End: j.Finished, Total: j.Duration}
	if !t.Start.IsZero() && t.Start.Before(buildStart.Add(time.Second)) {
		t.Known = true
	} else {
		t.Start = buildStart
	}
	if t.End.IsZero() || t.End.Before(buildEnd) {
		t.End = buildEnd
	}
	if t.Total <= 0 || !t.Known {
		t.Total = t.End.Sub(t.Start)
	}
	return t
}

// ---- the time bar ----

// Seg is one part of where a build's time went.
type Seg struct {
	Class, Label string
	D            time.Duration
	Flex         string // the part's share, for the bar
}

// timeSegs splits a job's time: runner and checkout (before the build),
// sending the context, base images, waiting for another job, building,
// export and push, the rest (cache checks and scheduling), and the job's
// time after its build.
func timeSegs(b *history.Build, jt jobTimes) []Seg {
	segs, total := segments(b)
	var out []Seg
	add := func(class, label string, d time.Duration) {
		if d >= 50*time.Millisecond {
			out = append(out, Seg{Class: class, Label: label, D: d})
		}
	}
	if jt.Known {
		add("runner", "Runner & checkout", b.Started.Sub(jt.Start))
	}
	for _, s := range segs {
		add(s.Class, s.Label, s.D)
	}
	if jt.Known && total > 0 {
		add("after", "After the build", jt.End.Sub(b.Completed))
	}
	var sum time.Duration
	for _, s := range out {
		sum += s.D
	}
	for i := range out {
		out[i].Flex = strconv.FormatFloat(float64(out[i].D)/float64(sum)*100, 'f', 2, 64)
	}
	return out
}

// ---- the steps table ----

// StepRow is one step of the steps table.
type StepRow struct {
	Index        int
	Key, Name    string
	Line         int
	State        string
	From         string // who it was received from
	D            time.Duration
	L, W         string // its place in the build, percent
	Start, End   time.Duration
	FirstRebuilt bool
}

// Block is a group of the steps table: a base image, one stage, or a run
// of stages that came from cache (folded at first).
type Block struct {
	Kind   string // from, stage, cached
	Name   string
	State  string
	From   string
	Meta   string
	First  bool
	Pulled bool
	Line   int
	D      time.Duration
	Rows   []StepRow
}

func blocksOf(b *history.Build, views []StepView) []Block {
	var total time.Duration
	for _, s := range b.Steps {
		if !s.Completed.IsZero() && s.Completed.Sub(b.Started) > total {
			total = s.Completed.Sub(b.Started)
		}
	}
	if d := b.Completed.Sub(b.Started); d > total {
		total = d
	}
	rowOf := func(v StepView) StepRow {
		r := StepRow{Index: v.Index, Key: v.Key, Name: v.Name, State: v.State, From: v.From, D: v.D, Start: v.Offset, End: v.Offset + v.D,
			FirstRebuilt: v.FirstRebuilt}
		if s := b.Steps[v.Index]; s.Line > 0 {
			r.Line = s.Line
		}
		if r.State == "image" {
			r.Name = b.Steps[v.Index].Name
		}
		r.L = strconv.FormatFloat(pctOf(v.Offset, total), 'f', 2, 64)
		r.W = strconv.FormatFloat(math.Max(0.6, pctOf(v.D, total)), 'f', 2, 64)
		return r
	}
	var out []Block
	for _, g := range stagesOf(views) {
		if g.State == "image" {
			v := g.Steps[0]
			out = append(out, Block{Kind: "from", Name: v.Name, State: "image", D: g.D, Pulled: g.D > 200*time.Millisecond,
				Line: b.Steps[v.Index].Line, Rows: []StepRow{rowOf(v)}})
			continue
		}
		var rows []StepRow
		built, cached, received, failed, stopped := 0, 0, 0, 0, 0
		for _, v := range g.Steps {
			rows = append(rows, rowOf(v))
			switch v.State {
			case "built":
				built++
			case "cached":
				cached++
			case "received":
				received++
			case "failed":
				failed++
			case "canceled":
				stopped++
			}
		}
		if g.State == "cached" && !g.FirstRebuilt {
			if n := len(out); n > 0 && out[n-1].Kind == "cached" {
				l := &out[n-1]
				l.Rows = append(l.Rows, rows...)
				l.D += g.D
				l.Meta = l.Meta + " · " + g.Name
				l.Name = ""
				continue
			}
			out = append(out, Block{Kind: "cached", Name: g.Name, State: "cached", D: g.D, Rows: rows, Meta: g.Name})
			continue
		}
		var meta []string
		if built > 0 {
			meta = append(meta, fmt.Sprintf("%d built", built))
		}
		if cached > 0 {
			meta = append(meta, fmt.Sprintf("%d cached", cached))
		}
		if received > 0 {
			meta = append(meta, fmt.Sprintf("%d received", received))
		}
		if failed > 0 {
			meta = append(meta, fmt.Sprintf("%d failed", failed))
		}
		if stopped > 0 {
			meta = append(meta, fmt.Sprintf("%d canceled", stopped))
		}
		out = append(out, Block{Kind: "stage", Name: g.Name, State: g.State, From: g.From, D: g.D, Rows: rows,
			Meta: strings.Join(meta, " · "), First: g.FirstRebuilt})
	}
	for i := range out {
		if out[i].Kind != "cached" {
			continue
		}
		stages := strings.Split(out[i].Meta, " · ")
		n := len(out[i].Rows)
		steps := fmt.Sprintf("%d step%s", n, plural(n))
		if len(stages) > 1 {
			out[i].Name = fmt.Sprintf("%d stages", len(stages))
			out[i].Meta = strings.Join(stages, " · ") + " · " + steps
		} else {
			out[i].Meta = steps
		}
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ---- the waterfall (timeline) ----

// WFBar is one bar of a waterfall row.
type WFBar struct {
	Class string
	L, W  string // percent of the axis
	Title string
}

// WFRow is one row of the waterfall: the pipeline, a job, a segment of a
// job (runner, context, push), a stage, or a step.
type WFRow struct {
	ID, Parent string
	Depth      int
	Kind       string // pipe, job, seg, group, step
	Glyph      string // its icon's class
	Icon       string
	Name       string
	Key        string // a step's key
	Mono       bool
	Chip       *Cat
	Crit       bool
	Open       bool // its children show at first
	Kids       bool
	Bars       []WFBar
	Label      template.HTML
	LabelL     string // where the label sits, percent
	LabelEnd   string // or, near the right edge, how far from it the label ends
	State      string // for the filters: built, cached, received, image
	Link       string
	Step       string // build/step for the step panel
	Text       string // what the search matches
	OnCrit     bool   // part of the job that sets the pipeline's time
}

// Waterfall is a pipeline (or one job of it) on a time axis.
type Waterfall struct {
	Rows  []WFRow
	Ticks []Tick
	Lanes []Lane
	Span  time.Duration
	Jobs  int
}

// Lane is a job in the overview strip above the waterfall.
type Lane struct {
	Name string
	Crit bool
	Bars []WFBar
	Link string
	Top  string // percent
}

// Tick is a time mark.
type Tick struct {
	L     string
	Label string
}

// wfJob is a job's build placed on the pipeline's time axis.
type wfJob struct {
	Name    string
	Cat     Cat
	Times   jobTimes
	Build   *history.Build
	BuildID int64
	Views   []StepView
	Link    string
	Crit    bool
}

func ticksFor(span time.Duration) []Tick {
	step := niceStep(span)
	var out []Tick
	for t := time.Duration(0); t <= span; t += step {
		out = append(out, Tick{L: strconv.FormatFloat(pctOf(t, span), 'f', 2, 64), Label: tickLabel(t)})
	}
	return out
}

func tickLabel(t time.Duration) string {
	s := t.Seconds()
	if s >= 60 && math.Mod(s, 60) == 0 {
		return fmt.Sprintf("%dm", int(s)/60)
	}
	if s == math.Trunc(s) {
		return fmt.Sprintf("%ds", int(s))
	}
	return fmt.Sprintf("%.1fs", s)
}

func niceStep(span time.Duration) time.Duration {
	for _, s := range []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second,
		30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour} {
		if span/s <= 8 {
			return s
		}
	}
	return 2 * time.Hour
}

// waterfall lays jobs out: pipeline > job > segment / stage > step.
// start is the axis origin.
func waterfall(pipeName string, jobs []wfJob, start time.Time, onlyOne bool) *Waterfall {
	var end time.Time
	for _, j := range jobs {
		if j.Times.End.After(end) {
			end = j.Times.End
		}
	}
	span := end.Sub(start)
	if span <= 0 {
		span = time.Second
	}
	// Room on the right for the labels after the last bars.
	span = span * 118 / 100
	at := func(t time.Time) string { return strconv.FormatFloat(pctOf(t.Sub(start), span), 'f', 2, 64) }
	width := func(d time.Duration) string {
		return strconv.FormatFloat(math.Max(0.25, pctOf(d, span)), 'f', 2, 64)
	}
	labelAfter := func(t time.Time, text string) (template.HTML, string) {
		return template.HTML(e(text)), strconv.FormatFloat(pctOf(t.Sub(start), span)+0.6, 'f', 2, 64)
	}
	w := &Waterfall{Ticks: ticksFor(span), Span: span, Jobs: len(jobs)}
	if !onlyOne {
		var pipeEnd time.Time
		for _, j := range jobs {
			if j.Times.End.After(pipeEnd) {
				pipeEnd = j.Times.End
			}
		}
		lab, ll := labelAfter(pipeEnd, fmt.Sprintf("%s · %d jobs", duration(pipeEnd.Sub(start)), len(jobs)))
		w.Rows = append(w.Rows, WFRow{ID: "p", Kind: "pipe", Glyph: "pipes", Icon: "pipes", Name: pipeName, Open: true, Kids: len(jobs) > 0,
			Bars: []WFBar{{Class: "pipe", L: "0", W: width(pipeEnd.Sub(start))}}, Label: lab, LabelL: ll, Text: strings.ToLower(pipeName)})
	}
	for ji, j := range jobs {
		b := j.Build
		jid := fmt.Sprintf("j%d", ji)
		parent := "p"
		depth := 1
		if onlyOne {
			parent, depth = "", 0
		}
		row := WFRow{ID: jid, Parent: parent, Depth: depth, Kind: "job", Glyph: j.Cat.Key, Icon: j.Cat.Icon, Name: j.Name, Chip: &j.Cat,
			Crit: j.Crit, Open: true, Kids: b != nil, Link: j.Link, Text: strings.ToLower(j.Name)}
		if b != nil {
			if j.Times.Known && b.Started.After(j.Times.Start) {
				row.Bars = append(row.Bars, WFBar{Class: "runner", L: at(j.Times.Start), W: width(b.Started.Sub(j.Times.Start)), Title: "Runner & checkout"})
			}
			segs, _ := segments(b)
			x := b.Started
			for _, s := range segs {
				if s.Class == "other" {
					continue
				}
				if s.Class == "push" {
					x = b.Completed.Add(-s.D)
				}
				row.Bars = append(row.Bars, WFBar{Class: s.Class, L: at(x), W: width(s.D), Title: s.Label + ": " + duration(s.D)})
				if s.Class == "xfer" {
					x = x.Add(s.D)
				}
			}
			if j.Times.Known && j.Times.End.Sub(b.Completed) >= 50*time.Millisecond {
				row.Bars = append(row.Bars, WFBar{Class: "after", L: at(b.Completed), W: width(j.Times.End.Sub(b.Completed)), Title: "After the build: " + duration(j.Times.End.Sub(b.Completed))})
			}
			row.Label, row.LabelL = labelAfter(j.Times.End, fmt.Sprintf("%s · job · %s in BuildKit", duration(j.Times.Total), duration(b.Completed.Sub(b.Started))))
		} else {
			row.Bars = append(row.Bars, WFBar{Class: "runner", L: at(j.Times.Start), W: width(j.Times.End.Sub(j.Times.Start)), Title: "Job"})
			row.Label, row.LabelL = labelAfter(j.Times.End, duration(j.Times.Total)+" · no build")
		}
		w.Rows = append(w.Rows, row)
		lane := Lane{Name: j.Name, Crit: j.Crit, Link: j.Link, Top: strconv.Itoa(ji * 10)}
		for _, br := range row.Bars {
			lane.Bars = append(lane.Bars, br)
		}
		w.Lanes = append(w.Lanes, lane)
		if b == nil {
			continue
		}
		seg := func(id, glyph, ic, name string, from, to time.Time, class, label string) {
			d := to.Sub(from)
			r := WFRow{ID: jid + id, Parent: jid, Depth: depth + 1, Kind: "seg", Glyph: glyph, Icon: ic, Name: name, Mono: true,
				Bars: []WFBar{{Class: class, L: at(from), W: width(d)}}, Text: strings.ToLower(name)}
			r.Label, r.LabelL = labelAfter(to, label)
			w.Rows = append(w.Rows, r)
		}
		if j.Times.Known && b.Started.After(j.Times.Start) {
			seg(".r", "runner", "clock", "runner & checkout", j.Times.Start, b.Started, "runner", duration(b.Started.Sub(j.Times.Start)))
		}
		segs, _ := segments(b)
		for _, s := range segs {
			if s.Class == "xfer" {
				label := duration(s.D)
				if b.ContextBytes > 0 {
					label += " · " + bytesText(b.ContextBytes)
				}
				seg(".x", "xfer", "up", "send context", b.Started, b.Started.Add(s.D), "xfer", label)
			}
		}
		for gi, g := range stagesOf(j.Views) {
			gid := fmt.Sprintf("%s.g%d", jid, gi)
			first, last := b.Completed, b.Started
			for _, v := range g.Steps {
				s := b.Steps[v.Index]
				if !s.Started.IsZero() && s.Started.Before(first) {
					first = s.Started
				}
				if s.Completed.After(last) {
					last = s.Completed
				}
			}
			if last.Before(first) {
				last = first
			}
			if g.State == "image" {
				v := g.Steps[0]
				r := WFRow{ID: gid, Parent: jid, Depth: depth + 1, Kind: "step", Glyph: "image", Icon: "s_image", Name: "FROM " + v.Name, Mono: true,
					Bars: []WFBar{{Class: "image", L: at(first), W: width(g.D)}}, State: "image", Step: fmt.Sprintf("%d/%d", j.BuildID, v.Index),
					Text: strings.ToLower(v.Name)}
				if g.D < 200*time.Millisecond {
					r.Label, r.LabelL = labelAfter(last, "already on node")
				} else {
					r.Label, r.LabelL = labelAfter(last, "pulled · "+duration(g.D))
				}
				w.Rows = append(w.Rows, r)
				continue
			}
			name := fmt.Sprintf("%s · %d step%s", g.Name, len(g.Steps), plural(len(g.Steps)))
			gr := WFRow{ID: gid, Parent: jid, Depth: depth + 1, Kind: "group", Glyph: g.State, Icon: stateIcon(g.State), Name: name,
				Kids: true, Open: g.State != "cached", State: g.State, Text: strings.ToLower(g.Name),
				Bars: []WFBar{{Class: g.State, L: at(first), W: width(last.Sub(first))}}}
			if g.FirstRebuilt {
				gr.Crit = true
			}
			word := stateLabels[g.State]
			gr.Label, gr.LabelL = labelAfter(last, word+" · "+duration(last.Sub(first)))
			w.Rows = append(w.Rows, gr)
			for _, v := range g.Steps {
				s := b.Steps[v.Index]
				st := s.Started
				if st.IsZero() {
					st = first
				}
				r := WFRow{ID: fmt.Sprintf("%s.s%d", gid, v.Index), Parent: gid, Depth: depth + 2, Kind: "step", Glyph: v.State, Icon: stateIcon(v.State),
					Name: v.Name, Key: v.Key, Mono: true, State: v.State, Crit: v.FirstRebuilt,
					Bars: []WFBar{{Class: v.State, L: at(st), W: width(v.D)}}, Step: fmt.Sprintf("%d/%d", j.BuildID, v.Index),
					Text: strings.ToLower(v.Key + " " + v.Name)}
				label := duration(v.D)
				if v.State == "received" && v.From != "" {
					label = "from " + v.From + " · " + label
				}
				r.Label, r.LabelL = labelAfter(st.Add(v.D), label)
				w.Rows = append(w.Rows, r)
			}
		}
		for _, s := range segs {
			if s.Class == "push" {
				seg(".e", "push", "up", "export & push", b.Completed.Add(-s.D), b.Completed, "push", duration(s.D))
			}
		}
	}
	// Rows of the job that sets the pipeline's time, for the critical path.
	critJob := ""
	for _, r := range w.Rows {
		if r.Kind == "job" && r.Crit {
			critJob = r.ID
		}
	}
	for i := range w.Rows {
		id := w.Rows[i].ID
		w.Rows[i].OnCrit = critJob != "" && (id == critJob || strings.HasPrefix(id, critJob+"."))
	}
	// A label that would run off the right edge ends where its bars begin.
	for i := range w.Rows {
		r := &w.Rows[i]
		if l, _ := strconv.ParseFloat(r.LabelL, 64); l > 86 {
			lo, _ := barSpan(*r)
			at := 100 - lo + 0.6 // before the bars
			if lo < 40 {
				at = 0 // at the right edge
			}
			r.LabelEnd = strconv.FormatFloat(at, 'f', 2, 64)
		}
	}
	return w
}

func bytesText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// median of a list of durations; 0 for none.
func median(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	c := append([]time.Duration(nil), ds...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}
