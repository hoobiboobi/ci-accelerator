package web

import (
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

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
	Chip       *Status
	Note       string // a job without a build: what GitLab says of it
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
	Span  time.Duration
	Jobs  int
}

// Tick is a time mark.
type Tick struct {
	L     string
	Label string
}

// wfJob is a job's build placed on the pipeline's time axis.
type wfJob struct {
	Name    string
	Status  *Status // nil: a job that built no image
	Note    string  // its GitLab status, for a job without a build
	Times   jobTimes
	Build   *history.Build
	BuildID int64
	Views   []StepView
	Link    string
	Crit    bool
}

// waitedFor says how long a job waited for a runner, when GitLab said.
func waitedFor(t jobTimes) string {
	if t.Queued.IsZero() || !t.Queued.Before(t.Start) {
		return ""
	}
	return "waited " + duration(t.Start.Sub(t.Queued)) + " · "
}

func ticksFor(span time.Duration) []Tick {
	step := niceStep(span)
	var out []Tick
	for t := time.Duration(0); t <= span; t += step {
		label := tickLabel(t)
		if span >= time.Minute {
			// One format along the axis: 0:00, 0:20, 1:40.
			label = clockLabel(t)
		}
		out = append(out, Tick{L: strconv.FormatFloat(pctOf(t, span), 'f', 2, 64), Label: label})
	}
	return out
}

// clockLabel is a time from the start as minutes and seconds (h:mm:ss
// past an hour).
func clockLabel(t time.Duration) string {
	s := int(t.Round(time.Second).Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
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
		glyph, ic := "job", "job"
		if j.Status != nil {
			glyph, ic = j.Status.Key, j.Status.Icon
		}
		row := WFRow{ID: jid, Parent: parent, Depth: depth, Kind: "job", Glyph: glyph, Icon: ic, Name: j.Name, Chip: j.Status, Note: j.Note,
			Crit: j.Crit, Open: onlyOne || len(jobs) == 1, Kids: b != nil, Link: j.Link, Text: strings.ToLower(j.Name)}
		// Waiting for a runner, when GitLab said when it queued the job.
		if q := j.Times.Queued; !q.IsZero() && !q.Before(start) && j.Times.Known && q.Before(j.Times.Start) {
			row.Bars = append(row.Bars, WFBar{Class: "queue", L: at(q), W: width(j.Times.Start.Sub(q)), Title: "Waiting for a runner: " + duration(j.Times.Start.Sub(q))})
		}
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
			row.Label, row.LabelL = labelAfter(j.Times.End, waitedFor(j.Times)+fmt.Sprintf("%s · job · %s in BuildKit", duration(j.Times.Total), duration(b.Completed.Sub(b.Started))))
		} else {
			class := "job"
			if j.Note == "failed" {
				class = "job-failed"
			}
			row.Bars = append(row.Bars, WFBar{Class: class, L: at(j.Times.Start), W: width(j.Times.End.Sub(j.Times.Start)), Title: j.Note + ": " + duration(j.Times.Total)})
			label := waitedFor(j.Times) + duration(j.Times.Total)
			if j.Note != "" {
				label += " · " + j.Note
			}
			row.Label, row.LabelL = labelAfter(j.Times.End, label)
		}
		w.Rows = append(w.Rows, row)
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

// barSpan is where a row's bars begin and end, percent of the axis.
func barSpan(r WFRow) (float64, float64) {
	lo, hi := 100.0, 0.0
	for _, b := range r.Bars {
		l, _ := strconv.ParseFloat(b.L, 64)
		w, _ := strconv.ParseFloat(b.W, 64)
		lo, hi = min(lo, l), max(hi, l+w)
	}
	if hi < lo {
		return 0, 0
	}
	return lo, hi
}

// more than the build took; the bar is relative to the parts.
func segments(b *history.Build) ([]Segment, time.Duration) {
	if b.Started.IsZero() || b.Completed.IsZero() || len(b.Steps) == 0 {
		return nil, 0
	}
	total := b.Completed.Sub(b.Started)
	first, last := b.Completed, b.Started
	var pull, recv, built time.Duration
	for _, s := range b.Steps {
		if !s.Started.IsZero() && s.Started.Before(first) {
			first = s.Started
		}
		if s.Completed.After(last) {
			last = s.Completed
		}
		switch {
		case isFrom(s):
			pull += s.Duration()
		case s.State == history.Received:
			recv += s.Duration()
		case s.State == history.Built:
			built += s.Duration()
		}
	}
	segs := []Segment{
		{Class: "xfer", Label: "Send context", D: clamp(first.Sub(b.Started))},
		{Class: "image", Label: "Base image", D: pull},
		{Class: "received", Label: "Waiting for another job", D: recv},
		{Class: "built", Label: "Build steps", D: built},
		{Class: "push", Label: "Export & push", D: clamp(b.Completed.Sub(last))},
	}
	var all time.Duration
	for _, s := range segs {
		all += s.D
	}
	if rest := total - all; rest > 0 {
		segs = append(segs, Segment{Class: "other", Label: "Other (cache checks, scheduling)", D: rest})
	}
	var sum time.Duration
	var kept []Segment
	for _, s := range segs {
		if s.D >= 50*time.Millisecond {
			kept = append(kept, s)
			sum += s.D
		}
	}
	x := 0.0
	for i := range kept {
		kept[i].X = x
		kept[i].W = 1000 * float64(kept[i].D) / float64(sum)
		x += kept[i].W
	}
	return kept, total
}

func clamp(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}
