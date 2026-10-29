package web

import (
	"math"
	"strconv"
	"time"
)

// Charts are drawn in a fixed coordinate space (chartW wide, chartH
// high, y down) that stretches to its box: the SVG keeps no aspect ratio
// and its strokes keep their width, while the tick labels are HTML around
// the plot, placed in percent, so text never scales with the page.
const (
	chartW = 1000
	chartH = 100
)

// ChartTick is one axis mark: its place along the axis in percent (from the
// top for a y axis, from the left for the x axis) and its label.
type ChartTick struct {
	Pct   string
	Label string
	// Align: "l" or "r" keeps the first and the last x label inside the
	// plot; "" centres the label on its mark.
	Align string
}

// Axes are a chart's ticks and gridlines.
type Axes struct {
	Left, Right, X []ChartTick
	Grid           []string // y of the horizontal gridlines, in the plot's coordinates
	XGrid          []string // x of the vertical gridlines
}

func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

func pct(share float64) string {
	return strconv.FormatFloat(100*math.Min(1, math.Max(0, share)), 'f', 2, 64)
}

// plotY maps a share of the axis (0 at the bottom, 1 at the top) to y.
func plotY(share float64) float64 { return chartH - chartH*math.Min(1, math.Max(0, share)) }

// quarters are y ticks at 0, 1/4, 1/2, 3/4 and the top of an axis.
func quarters(top float64, label func(float64) string) (ticks []ChartTick, grid []string) {
	for i := 0; i <= 4; i++ {
		share := float64(i) / 4
		ticks = append(ticks, ChartTick{Pct: pct(1 - share), Label: label(top * share)})
		if i > 0 && i < 4 {
			grid = append(grid, f1(plotY(share)))
		}
	}
	return ticks, grid
}

// chartStep is a round step that gives about four ticks up to top.
func chartStep(top float64) float64 {
	if top <= 0 {
		return 1
	}
	raw := top / 4
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if raw <= m*mag {
			return m * mag
		}
	}
	return 10 * mag
}

// coresLabel writes a core count: "2.5", "10", "4 cores" for the top.
func coresLabel(v float64, top bool) string {
	s := strconv.FormatFloat(math.Round(v*1000)/1000, 'f', -1, 64)
	if top {
		return s + " cores"
	}
	return s
}

// clockTicks are the quarter hours inside [start, end], as x ticks.
func clockTicks(start, end time.Time, loc *time.Location) []ChartTick {
	var ticks []ChartTick
	span := end.Sub(start).Seconds()
	t := start.In(loc).Truncate(15 * time.Minute)
	if t.Before(start) {
		t = t.Add(15 * time.Minute)
	}
	for ; !t.After(end); t = t.Add(15 * time.Minute) {
		ticks = append(ticks, ChartTick{Pct: pct(t.Sub(start).Seconds() / span), Label: t.In(loc).Format("15:04")})
	}
	return ticks
}

// spanTicks are x ticks at 0, 1/4, 1/2, 3/4 and the end of a duration.
func spanTicks(wall time.Duration) []ChartTick {
	var ticks []ChartTick
	for i := 0; i <= 4; i++ {
		d := time.Duration(float64(wall) * float64(i) / 4)
		t := ChartTick{Pct: pct(float64(i) / 4), Label: duration(d.Round(100 * time.Millisecond))}
		switch i {
		case 0:
			t.Align, t.Label = "l", "0 s"
		case 4:
			t.Align = "r"
		}
		ticks = append(ticks, t)
	}
	return ticks
}

// coresText writes cores busy on average: one decimal, "<0.1" for next
// to nothing rather than a "0.0" that reads as nothing at all.
func coresText(v float64) string {
	if v > 0 && v < 0.05 {
		return "<0.1"
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}
