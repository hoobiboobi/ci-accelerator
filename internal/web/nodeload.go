package web

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/diskprobe"
)

// LoadView is a daemon's node and pod load as the probe next to the
// daemon read it: the latest reading, and the last hour drawn.
type LoadView struct {
	// Err: the probe did not report load (an older probe image, or it
	// did not answer); the rest is empty then.
	Err string
	// The latest reading as text: "0.2 of 4", "2.1 of 7.7 GiB".
	CPUText, MemText string
	Cores            int
	MemTotal         string
	// The daemon pod's share of it; PodKnown false: not measured (the
	// node runs cgroup v1, or the pod's cgroup was not found).
	PodKnown       bool
	PodCPU, PodMem string
	PodMemLimit    string // "" without a limit
	// Bars: the node's cores and memory split into the daemon's pod, the
	// rest of the node, and what is free (percent of the node).
	CPUSegs, MemSegs []NSeg
	// Pressure is the node's PSI: "" when the kernel reports none.
	Pressure     string
	PressureChip string // "waiting for CPU 38 %" when some task waited for a core
	Measured     string
	// Spark is the hour's sparkline (the table); Chart the hour drawn
	// larger (a daemon's page).
	Spark *LoadChart
	Chart *LoadChart
}

// LoadChart is the last hour's readings as polyline points: the node's
// CPU and the pod's CPU as shares of the cores, memory as a share of the
// node's. The sparkline (the table) keeps its own small space; the chart
// (a daemon's page) is drawn in the charts' space with Axes around it.
type LoadChart struct {
	W, H            int
	NodeCPU, PodCPU string // polyline points
	PodFill         string // polygon under the pod's CPU
	Mem             string
	Start, End      string // the hour's ends
	// Since is set when the readings start well after the hour's start
	// (the probe started then): the lines cover only part of the plot.
	Since string
	Axes  Axes
}

const (
	sparkW, sparkH = 72, 18
)

// loadView reads a daemon's load; nil without a probe at all.
func loadView(in collector.Inspection, at time.Time, loc *time.Location) *LoadView {
	if in.Load == nil {
		if in.LoadErr != "" {
			return &LoadView{Err: in.LoadErr}
		}
		return nil
	}
	l := in.Load
	v := &LoadView{Cores: l.Cores}
	cores := float64(l.Cores)
	v.CPUText = fmt.Sprintf("%s of %d", strconv.FormatFloat(l.Node.CPU, 'f', 1, 64), l.Cores)
	v.MemTotal = bytesText(int64(l.MemTotal))
	v.MemText = fmt.Sprintf("%s of %s", bytesText(int64(l.Node.Mem)), v.MemTotal)
	v.PodCPU = strconv.FormatFloat(l.Pod.CPU, 'f', 1, 64)
	v.PodMem = bytesText(int64(l.Pod.Mem))
	if l.PodMemLimit > 0 {
		v.PodMemLimit = bytesText(int64(l.PodMemLimit))
	}
	pct := func(part, whole float64) string {
		if whole <= 0 {
			return "0"
		}
		return strconv.FormatFloat(100*math.Max(0, math.Min(part, whole))/whole, 'f', 2, 64)
	}
	v.PodKnown = l.PodKnown
	// Without the pod's figures the whole used part is the node's.
	podCPU, podMem := 0.0, 0.0
	if l.PodKnown {
		podCPU, podMem = math.Min(l.Pod.CPU, l.Node.CPU), float64(min(l.Pod.Mem, l.Node.Mem))
	}
	rest := "The rest of the node"
	if !l.PodKnown {
		rest = "In use"
	}
	total := float64(l.MemTotal)
	if l.PodKnown {
		v.CPUSegs = append(v.CPUSegs, NSeg{Class: "daemon", Label: "BuildKit daemon", Size: strconv.FormatFloat(podCPU, 'f', 1, 64) + " cores", Flex: pct(podCPU, cores)})
		v.MemSegs = append(v.MemSegs, NSeg{Class: "mem", Label: "BuildKit daemon", Size: bytesText(int64(podMem)), Flex: pct(podMem, total)})
	}
	v.CPUSegs = append(v.CPUSegs, NSeg{Class: "host", Label: rest, Size: strconv.FormatFloat(l.Node.CPU-podCPU, 'f', 1, 64) + " cores", Flex: pct(l.Node.CPU-podCPU, cores)},
		NSeg{Class: "free", Label: "Idle", Size: strconv.FormatFloat(math.Max(0, cores-l.Node.CPU), 'f', 1, 64) + " cores", Flex: pct(cores-l.Node.CPU, cores)})
	v.MemSegs = append(v.MemSegs, NSeg{Class: "host", Label: rest, Size: bytesText(int64(float64(l.Node.Mem) - podMem)), Flex: pct(float64(l.Node.Mem)-podMem, total)},
		NSeg{Class: "free", Label: "Available", Size: bytesText(int64(l.MemTotal - l.Node.Mem)), Flex: pct(total-float64(l.Node.Mem), total)})
	if p := l.Pressure; p != nil {
		v.Pressure = fmt.Sprintf("in the last minute some task waited for a core %.0f %% of the time, for memory %.0f %%, for disk %.0f %%", p.CPU60, p.Mem60, p.IO60)
		if p.CPU60 >= 10 {
			v.PressureChip = fmt.Sprintf("waiting for CPU %.0f %%", p.CPU60)
		}
	}
	if !l.At.IsZero() {
		v.Measured = "measured " + stampOf(l.At, loc, "15:04:05").Text
	}
	if len(l.Series) >= 2 {
		v.Spark = loadChart(l, at, loc, sparkW, sparkH, false)
		v.Chart = loadChart(l, at, loc, chartW, chartH, true)
	}
	return v
}

// loadChart draws the series over the hour that ends at now: the
// sparkline in a w x h space of its own (axes: false), the chart in the
// charts' space with ticks (axes: true).
func loadChart(l *diskprobe.Load, now time.Time, loc *time.Location, w, h int, axes bool) *LoadChart {
	c := &LoadChart{W: w, H: h}
	end := now
	if last := time.Unix(l.Series[len(l.Series)-1].At, 0); last.After(end) {
		end = last
	}
	start := end.Add(-diskprobe.Keep)
	c.Start, c.End = start.In(loc).Format("15:04"), end.In(loc).Format("15:04")
	pad := 1
	if axes {
		pad = 0
	}
	x := func(at int64) float64 {
		return float64(w) * math.Min(1, math.Max(0, float64(at-start.Unix())/diskprobe.Keep.Seconds()))
	}
	y := func(share float64) float64 {
		return float64(h-pad) - math.Min(1, math.Max(0, share))*float64(h-2*pad)
	}
	cores, total := float64(l.Cores), float64(l.MemTotal)
	var node, pod, mem, fill strings.Builder
	for i, s := range l.Series {
		px := f1(x(s.At))
		if i > 0 {
			node.WriteByte(' ')
			pod.WriteByte(' ')
			mem.WriteByte(' ')
			fill.WriteByte(' ')
		}
		fmt.Fprintf(&node, "%s,%s", px, f1(y(s.NodeCPU/cores)))
		fmt.Fprintf(&pod, "%s,%s", px, f1(y(s.PodCPU/cores)))
		fmt.Fprintf(&mem, "%s,%s", px, f1(y(float64(s.NodeMem)/total)))
		fmt.Fprintf(&fill, "%s,%s", px, f1(y(s.PodCPU/cores)))
	}
	first, last := l.Series[0], l.Series[len(l.Series)-1]
	c.NodeCPU, c.Mem = node.String(), mem.String()
	if l.PodKnown {
		c.PodCPU = pod.String()
		c.PodFill = fmt.Sprintf("%s,%s %s %s,%s", f1(x(first.At)), f1(y(0)), fill.String(), f1(x(last.At)), f1(y(0)))
	}
	if !axes {
		return c
	}
	c.Axes.Left, c.Axes.Grid = quarters(cores, func(v float64) string { return coresLabel(v, v == cores) })
	c.Axes.Right, _ = quarters(total, func(v float64) string { return bytesText(int64(v)) })
	c.Axes.X = clockTicks(start, end, loc)
	for _, t := range c.Axes.X {
		c.Axes.XGrid = append(c.Axes.XGrid, strconv.FormatFloat(float64(w)*mustFloat(t.Pct)/100, 'f', 1, 64))
	}
	if since := time.Unix(first.At, 0); since.Sub(start) > 2*time.Minute {
		c.Since = "readings since " + since.In(loc).Format("15:04")
	}
	return c
}

func mustFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}
