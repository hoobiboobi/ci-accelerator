package web

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// ResourcesView is a build's Resources tab: what its executed steps used,
// the heaviest first, and how many cores they wanted together over the
// build. Every figure is BuildKit's cgroup reading of the step; nothing
// is estimated.
type ResourcesView struct {
	Rows []ResRow
	// More folds the steps after resRows into one line: how many, their
	// CPU time, run time and bytes written together.
	More                          int
	MoreCPU, MoreDur, MoreWritten string
	// Totals over the executed steps.
	CPU, Read, Written string
	// CoresText is the CPU time over the build's wall time, with the
	// node's cores when known ("2.9 of 4").
	CoresText string
	// TopMemory is the most one step's cgroup held (highest sample), and
	// which step; "" when no step reports memory (recorded before 0.3.3).
	TopMemory, TopMemoryStep string
	// Demand is the sum of the running steps' cores over the build.
	Demand *DemandChart
	// Peak says when the steps wanted more cores than the node has; ""
	// when they never did or the node's cores are unknown.
	Peak string
}

// ResRow is one executed step of the Resources tab.
type ResRow struct {
	Index   int
	Line    int
	Name    string
	Stage   string
	Cores   string // cores busy on average over the step, "1.9"
	Bar     string // Cores as a share of the widest row, percent
	CPU     string
	Dur     string
	Memory  string // "" when not recorded
	Written string
}

// DemandChart draws the cores the running steps used together: a step
// function over the build (the line and the filled area under it), the
// node's cores as a dashed line when known, and the axes around it.
type DemandChart struct {
	W, H  int
	Line  string // polyline points
	Fill  string // polygon points
	Over  string // polygon of the area above the node's cores; "" when none
	CapY  string // y of the node's cores; "" when unknown
	Top   string // the top tick's label ("6 cores")
	Cores int
	Axes  Axes
}

const resRows = 12

// executed is a step this build ran, with its cores over its run.
type executed struct {
	i    int
	s    history.Step
	rate float64
}

// resources sums what a build's executed steps used. cores is the node's
// core count now (0: unknown); wall the build's duration.
func resources(b *history.Build, views []StepView, cores int, wall time.Duration) *ResourcesView {
	var exs []executed
	var cpu time.Duration
	var read, written int64
	for i, s := range b.Steps {
		if s.State != history.Built || (s.Usage == history.Usage{}) {
			continue
		}
		e := executed{i: i, s: s}
		if d := s.Duration(); d > 0 {
			e.rate = s.Usage.CPU.Seconds() / d.Seconds()
		}
		exs = append(exs, e)
		cpu += s.Usage.CPU
		read += s.Usage.ReadBytes
		written += s.Usage.WriteBytes
	}
	if len(exs) == 0 || cpu == 0 {
		return nil
	}
	sort.SliceStable(exs, func(i, j int) bool { return exs[i].s.Usage.CPU > exs[j].s.Usage.CPU })
	v := &ResourcesView{CPU: duration(cpu), Read: bytesText(read), Written: bytesText(written)}
	stage := map[int]string{}
	for _, sv := range views {
		stage[sv.Index] = sv.Stage
	}
	widest := 0.0
	for _, e := range exs {
		widest = math.Max(widest, e.rate)
	}
	var topMem int64
	for n, e := range exs {
		if e.s.Usage.Memory > topMem {
			topMem = e.s.Usage.Memory
			v.TopMemory, v.TopMemoryStep = bytesText(topMem), e.s.Key
		}
		if n >= resRows {
			continue
		}
		row := ResRow{Index: e.i, Line: e.s.Line, Name: e.s.Name, Stage: stage[e.i], Cores: coresText(e.rate),
			CPU: duration(e.s.Usage.CPU), Dur: duration(e.s.Duration()), Written: bytesText(e.s.Usage.WriteBytes)}
		if widest > 0 {
			row.Bar = strconv.FormatFloat(100*e.rate/widest, 'f', 1, 64)
		}
		if e.s.Usage.Memory > 0 {
			row.Memory = bytesText(e.s.Usage.Memory)
		}
		v.Rows = append(v.Rows, row)
	}
	if len(exs) > resRows {
		var c, d time.Duration
		var w int64
		for _, e := range exs[resRows:] {
			c += e.s.Usage.CPU
			d += e.s.Duration()
			w += e.s.Usage.WriteBytes
		}
		v.More, v.MoreCPU, v.MoreDur, v.MoreWritten = len(exs)-resRows, duration(c), duration(d), bytesText(w)
	}
	if wall > 0 {
		v.CoresText = coresText(cpu.Seconds() / wall.Seconds())
		if cores > 0 {
			v.CoresText += " of " + strconv.Itoa(cores)
		}
	}
	var steps []history.Step
	for _, e := range exs {
		if e.rate > 0 {
			steps = append(steps, e.s)
		}
	}
	v.Demand, v.Peak = demand(steps, b.Started, wall, cores)
	return v
}

// demandAt is one point of the cores wanted over the build: the sum of
// the running steps' rates from at on.
type demandAt struct {
	at    time.Duration // from the build's start
	cores float64
}

// demandSteps makes the step function: a point at every start and end,
// with the sum of the steps running after it.
func demandSteps(steps []history.Step, start time.Time) []demandAt {
	type edge struct {
		at   time.Duration
		rate float64
	}
	var edges []edge
	for _, s := range steps {
		d := s.Duration()
		if d <= 0 || s.Started.Before(start) {
			continue
		}
		rate := s.Usage.CPU.Seconds() / d.Seconds()
		edges = append(edges, edge{s.Started.Sub(start), rate}, edge{s.Completed.Sub(start), -rate})
	}
	sort.SliceStable(edges, func(i, j int) bool { return edges[i].at < edges[j].at })
	var out []demandAt
	sum := 0.0
	for i, e := range edges {
		sum += e.rate
		if sum < 1e-9 {
			sum = 0
		}
		if i+1 < len(edges) && edges[i+1].at == e.at {
			continue
		}
		out = append(out, demandAt{e.at, sum})
	}
	return out
}

// demand draws the chart and names the peak above the node's cores.
func demand(steps []history.Step, start time.Time, wall time.Duration, cores int) (*DemandChart, string) {
	pts := demandSteps(steps, start)
	if len(pts) == 0 || wall <= 0 {
		return nil, ""
	}
	maxC := float64(cores)
	for _, p := range pts {
		maxC = math.Max(maxC, p.cores)
	}
	if maxC <= 0 {
		return nil, ""
	}
	// Round ticks, the top one at or above the highest value.
	step := chartStep(maxC)
	top := math.Ceil(maxC/step) * step
	c := &DemandChart{W: chartW, H: chartH, Cores: cores}
	x := func(at time.Duration) float64 {
		return math.Min(float64(chartW), float64(chartW)*at.Seconds()/wall.Seconds())
	}
	y := func(v float64) float64 { return plotY(v / top) }
	// poly draws the function clipped below at floor: the line itself
	// (floor 0) and the part above the node's cores (floor cores).
	poly := func(floor float64, closed bool) string {
		var b strings.Builder
		base := f1(y(floor))
		fmt.Fprintf(&b, "0,%s", base)
		prev := floor
		for _, p := range pts {
			px := f1(x(p.at))
			fmt.Fprintf(&b, " %s,%s %s,%s", px, f1(y(prev)), px, f1(y(math.Max(p.cores, floor))))
			prev = math.Max(p.cores, floor)
		}
		fmt.Fprintf(&b, " %d,%s", chartW, f1(y(prev)))
		if closed {
			fmt.Fprintf(&b, " %d,%s", chartW, base)
		}
		return b.String()
	}
	c.Line, c.Fill = poly(0, false), poly(0, true)
	for v := 0.0; v <= top+1e-9; v += step {
		c.Axes.Left = append(c.Axes.Left, ChartTick{Pct: pct(1 - v/top), Label: coresLabel(v, v >= top-1e-9)})
		if v > 0 && v < top-1e-9 {
			c.Axes.Grid = append(c.Axes.Grid, f1(y(v)))
		}
	}
	c.Top = c.Axes.Left[len(c.Axes.Left)-1].Label
	c.Axes.X = spanTicks(wall)
	for _, t := range c.Axes.X[1:4] {
		c.Axes.XGrid = append(c.Axes.XGrid, strconv.FormatFloat(float64(chartW)*mustFloat(t.Pct)/100, 'f', 1, 64))
	}
	peak := ""
	if cores > 0 {
		c.CapY = f1(y(float64(cores)))
		var above time.Duration
		most := 0.0
		for i, p := range pts {
			if p.cores > float64(cores) {
				end := wall
				if i+1 < len(pts) {
					end = pts[i+1].at
				}
				above += end - p.at
			}
			most = math.Max(most, p.cores)
		}
		if above > 0 {
			c.Over = poly(float64(cores), true)
			peak = fmt.Sprintf("The steps wanted up to %s cores of the node's %d: more than it has for %s of the build.",
				strconv.FormatFloat(most, 'f', 1, 64), cores, duration(above))
		}
	}
	return c, peak
}
