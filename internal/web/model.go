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

// Cat is a build's verdict tone (cached, first, built, node, unknown,
// failed) and its icon, by its reason, error and executed steps.
type Cat struct{ Tone, Icon string }

var cats = map[string]Cat{
	"cached":   {"cached", "v_cached"},
	"first":    {"first", "v_first"},
	"step":     {"built", "v_step"},
	"files":    {"built", "v_files"},
	"node":     {"node", "v_node"},
	"unknown":  {"unknown", "v_unknown"},
	"failed":   {"failed", "warn"},
	"canceled": {"first", "x"},
}

// canceled says whether a build or step error is a cancellation (the job
// was canceled or its client went away), not a failure of the Dockerfile.
func canceled(err string) bool { return history.Canceled(err) }

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
	case history.ArgChanged, history.BaseImageChanged:
		return cats["step"]
	case history.FilesChanged:
		return cats["files"]
	case history.OtherNode:
		return cats["node"]
	case history.CacheElsewhere:
		return cats["unknown"]
	case history.NotInCache:
		return cats["node"]
	}
	if built > 0 {
		return cats["first"]
	}
	return cats["cached"]
}

// ---- icons ----

var icons = map[string]string{
	"pipes":      `<path d="M2.5 4h11M2.5 8h11M2.5 12h7"/>`,
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
	"out":        `<path d="M9 3H3.5v10H9M6.5 8h7m0 0-2.4-2.4M13.5 8l-2.4 2.4"/>`,
	"log":        `<path d="M3 3.5h10v9H3z"/><path d="M5.5 6.5 7 8l-1.5 1.5M8.5 9.5h2"/>`,
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
	"tl":         `<path d="M2.5 4h6M4.5 8h7M7 12h6.5"/>`,
	"steps":      `<path d="M5.5 4h8M5.5 8h8M5.5 12h8M2.5 4h.01M2.5 8h.01M2.5 12h.01"/>`,
	"overview":   `<rect x="2.5" y="2.5" width="4.5" height="4.5" rx="1"/><rect x="9" y="2.5" width="4.5" height="4.5" rx="1"/><rect x="2.5" y="9" width="4.5" height="4.5" rx="1"/><rect x="9" y="9" width="4.5" height="4.5" rx="1"/>`,
	"builds":     `<path d="M2.5 4h11M2.5 8h11M2.5 12h11"/>`,
	"images":     `<path d="M8 2.2 13.5 5v6L8 13.8 2.5 11V5Z"/><path d="M2.5 5 8 7.8 13.5 5M8 7.8v6"/>`,
	// The five statuses: a shape each, so they read without colour.
	"st_cached":  `<circle cx="8" cy="8" r="4.6" fill="currentColor" stroke="none"/>`,
	"st_change":  `<rect x="3.6" y="3.6" width="8.8" height="8.8" rx="1.4" fill="currentColor" stroke="none"/>`,
	"st_farm":    `<path d="M8 2.6 13.4 12.6H2.6Z" fill="currentColor" stroke="none"/>`,
	"st_unclear": `<path d="M8 2.4 13.6 8 8 13.6 2.4 8Z" stroke-width="2"/>`,
	"st_failed":  `<path d="m4 4 8 8M12 4l-8 8" stroke-width="2.4"/>`,
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

// when is a moment with its day, in the reader's zone.
func when(t time.Time, loc *time.Location) Stamp { return stampOf(t, loc, "2 Jan 15:04:05") }

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
	// Queued is when the job began waiting for a runner: its start less
	// the time GitLab says it waited. (GitLab creates every job of a
	// pipeline at once, so a job's creation also counts the earlier
	// stages: not a runner's fault.)
	Queued time.Time
	// From the job's pod (whole seconds): when Kubernetes created it and
	// started its build container. Zero when not known.
	PodCreated, PodRunning time.Time
}

// startPart is a stretch of a job between its start and its first build.
type startPart struct {
	Class, Label string
	From, To     time.Time
}

// startParts splits the time from a job's start to its build: the runner
// preparing the job's pod, the pod starting (scheduled, images pulled),
// then the checkout and the job's script up to buildctl. Without the pod's
// times, one part. Kubernetes records whole seconds, so its times are
// kept within the job's own.
func startParts(t jobTimes, build time.Time) []startPart {
	if !build.After(t.Start) {
		return nil
	}
	if t.PodCreated.IsZero() || t.PodRunning.IsZero() {
		return []startPart{{"runner", "Runner & checkout", t.Start, build}}
	}
	clampT := func(x, lo, hi time.Time) time.Time {
		if x.Before(lo) {
			return lo
		}
		if x.After(hi) {
			return hi
		}
		return x
	}
	c := clampT(t.PodCreated, t.Start, build)
	r := clampT(t.PodRunning, c, build)
	var out []startPart
	for _, p := range []startPart{
		{"runner", "The runner preparing the job's pod", t.Start, c},
		{"podwait", "The job's pod starting: scheduled, images pulled", c, r},
		{"runner", "The runner attaching to the pod, the checkout and the job's script up to the build", r, build},
	} {
		if p.To.Sub(p.From) >= 50*time.Millisecond {
			out = append(out, p)
		}
	}
	return out
}

// queuedAt is when a job began waiting for a runner, zero when GitLab has
// not said.
func queuedAt(j store.JobInfo, start time.Time) time.Time {
	if j.Waited <= 0 || start.IsZero() {
		return time.Time{}
	}
	return start.Add(-j.Waited)
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
	if t.Known {
		t.Queued = queuedAt(j, t.Start)
		t.PodCreated, t.PodRunning = j.PodCreated, j.PodRunning
	}
	return t
}

// ---- the time bar ----

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

// gantt is the steps of a build on its timeline, by start: what ran,
// was pulled or was waited for. Steps from the cache take no time and are
// only counted.
func gantt(blocks []Block) ([]StepRow, int) {
	var rows []StepRow
	skip := 0
	for _, b := range blocks {
		for _, r := range b.Rows {
			if r.State == "cached" || (r.State == "image" && !b.Pulled) {
				skip++
				continue
			}
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Start < rows[j].Start })
	return rows, skip
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// bytesText writes a size in binary units with their own names (1 GiB =
// 1024 MiB): BuildKit's GC limits are binary too, and a "GB" that means
// GiB would be 7 % off.
func bytesText(n int64) string {
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%.2f TiB", float64(n)/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return unitText(float64(n)/(1<<20), "MiB")
	case n >= 1<<10:
		return unitText(float64(n)/(1<<10), "KiB")
	}
	return fmt.Sprintf("%d B", n)
}

// unitText keeps two significant digits below 10 (1.5 MiB, not 2 MiB).
func unitText(v float64, unit string) string {
	if v < 9.95 {
		return fmt.Sprintf("%.1f %s", v, unit)
	}
	return fmt.Sprintf("%.0f %s", v, unit)
}

// median of a list of durations (nearest rank, as everywhere); 0 for none.
func median(ds []time.Duration) time.Duration { return store.Percentile(ds, 50) }
