package web

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/diskprobe"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// step makes an executed step from start to end (seconds into the build)
// that used cpu seconds of CPU.
func step(t0 time.Time, key string, start, end, cpu float64, mem int64) history.Step {
	return history.Step{Key: key, Name: "RUN " + key, State: history.Built,
		Started: t0.Add(time.Duration(start * float64(time.Second))), Completed: t0.Add(time.Duration(end * float64(time.Second))),
		Usage: history.Usage{CPU: time.Duration(cpu * float64(time.Second)), Memory: mem, WriteBytes: 1 << 20}}
}

// The cores wanted over a build is the sum of the running steps' rates
// between their starts and ends; steps that overlap add up.
func TestDemandSumsTheRunningSteps(t *testing.T) {
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	steps := []history.Step{
		step(t0, "a", 0, 10, 20, 0), // 2 cores for 10 s
		step(t0, "b", 5, 15, 5, 0),  // 0.5 cores for 10 s
		step(t0, "c", 20, 22, 2, 0), // 1 core for 2 s
		{Key: "cached", State: history.Cached},
	}
	got := demandSteps(steps, t0)
	want := []demandAt{{0, 2}, {5 * time.Second, 2.5}, {10 * time.Second, 0.5}, {15 * time.Second, 0}, {20 * time.Second, 1}, {22 * time.Second, 0}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].at != want[i].at || got[i].cores < want[i].cores-0.001 || got[i].cores > want[i].cores+0.001 {
			t.Errorf("point %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	// On a 2-core node the steps wanted more than it has for 5 s (2.5
	// cores from 5 s to 10 s); the chart draws that area.
	c, peak := demand(steps, t0, 25*time.Second, 2)
	if c == nil || c.Over == "" || c.CapY == "" || !strings.Contains(peak, "up to 2.5 cores") || !strings.Contains(peak, "for 5.0 s") {
		t.Errorf("peak %q, chart %+v", peak, c)
	}
	// Enough cores: no peak, no area above, still the capacity line.
	c, peak = demand(steps, t0, 25*time.Second, 4)
	if c == nil || c.Over != "" || c.CapY == "" || peak != "" {
		t.Errorf("4 cores: peak %q, over %q", peak, c.Over)
	}
	// Unknown cores: the line and the area under it only.
	c, peak = demand(steps, t0, 25*time.Second, 0)
	if c == nil || c.CapY != "" || c.Over != "" || peak != "" || c.Line == "" || c.Fill == "" || c.Top != "3 cores" {
		t.Errorf("unknown cores: %+v %q", c, peak)
	}
	for _, pts := range []string{c.Line, c.Fill} {
		if !regexp.MustCompile(`^(-?[0-9.]+,-?[0-9.]+ ?)+$`).MatchString(pts) {
			t.Errorf("points are not numbers: %q", pts)
		}
	}
	if c, _ := demand(nil, t0, 25*time.Second, 2); c != nil {
		t.Error("a chart without steps")
	}
}

// The Resources tab sums the executed steps, the heaviest first, folds
// the rest, and says what the whole build used per core of the node.
func TestResourcesView(t *testing.T) {
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	b := &history.Build{Started: t0, Completed: t0.Add(100 * time.Second)}
	for i := 0; i < 15; i++ {
		mem := int64(0)
		if i == 3 {
			mem = 1500 << 20
		}
		b.Steps = append(b.Steps, step(t0, fmt.Sprintf("s%02d", i), float64(i), float64(i)+10, float64(15-i), mem))
	}
	b.Steps = append(b.Steps, history.Step{Key: "cached", State: history.Cached, Usage: history.Usage{CPU: time.Hour}})
	var views []StepView
	for i := range b.Steps {
		views = append(views, StepView{Index: i, Stage: "build"})
	}
	v := resources(b, views, 4, 100*time.Second)
	if v == nil {
		t.Fatal("no view")
	}
	if len(v.Rows) != resRows || v.More != 3 || v.Rows[0].Name != "RUN s00" || v.Rows[0].CPU != "15 s" || v.Rows[0].Cores != "1.5" || v.Rows[0].Bar != "100.0" {
		t.Errorf("rows: %+v more %d", v.Rows[:2], v.More)
	}
	// 15+14+...+1 = 120 s of CPU over 100 s on 4 cores.
	if v.CPU != "2 min 0 s" || v.CoresText != "1.2 of 4" || v.Written != "15 MiB" {
		t.Errorf("totals: cpu %q cores %q written %q", v.CPU, v.CoresText, v.Written)
	}
	if v.TopMemory != "1.5 GiB" || v.TopMemoryStep != "s03" || v.Rows[3].Memory != "1.5 GiB" || v.Rows[0].Memory != "" {
		t.Errorf("memory: %q at %q, rows %q %q", v.TopMemory, v.TopMemoryStep, v.Rows[0].Memory, v.Rows[3].Memory)
	}
	if v.MoreCPU != "6.0 s" || v.Demand == nil || v.Demand.Cores != 4 {
		t.Errorf("more %q, demand %+v", v.MoreCPU, v.Demand)
	}
	// Ten steps at once wanted 10.5 cores of 4: said, with the time.
	if !strings.Contains(v.Peak, "of the node's 4") {
		t.Errorf("peak: %q", v.Peak)
	}
	// Without cores: the average alone, no peak.
	if v := resources(b, views, 0, 100*time.Second); v.CoresText != "1.2" || v.Peak != "" {
		t.Errorf("unknown cores: %q %q", v.CoresText, v.Peak)
	}
	// A build whose steps all came from the cache has no tab.
	if v := resources(&history.Build{Steps: []history.Step{{State: history.Cached}}}, nil, 4, time.Minute); v != nil {
		t.Error("a tab for a cached build")
	}
}

// The build page of every recorded build that ran steps has the Resources
// tab with BuildKit's figures; a cached build has none; the drawer of an
// executed step names its memory when recorded.
func TestBuildPageResources(t *testing.T) {
	ts, st, ids := console(t)
	tabs, none := 0, 0
	for _, id := range ids {
		p, err := st.BuildPage(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		cpu := time.Duration(0)
		for _, s := range p.Build.Steps {
			cpu += s.Usage.CPU
		}
		pg := get(t, ts, fmt.Sprintf("/builds/%d", id))
		pg.safe(t)
		has := strings.Contains(pg.body, `data-tab="resources"`)
		switch {
		case cpu > 0 && !has:
			t.Errorf("build %d used %s of CPU: no Resources tab", id, cpu)
		case cpu == 0 && has:
			t.Errorf("build %d used no CPU: a Resources tab", id)
		case has:
			tabs++
			for _, want := range []string{`data-pane="resources"`, "CPU in all", "Cores the running steps used together", `class="tbox flat rtable"`, "cores busy on average"} {
				if !strings.Contains(pg.body, want) {
					t.Errorf("build %d: resources lack %q", id, want)
				}
			}
			if strings.Contains(pg.body, "the dashed line is the node") {
				t.Errorf("build %d: a capacity line without a probe", id)
			}
		default:
			none++
		}
	}
	if tabs == 0 || none == 0 {
		t.Errorf("%d builds with the tab, %d without", tabs, none)
	}
}

// With the daemon's probe reporting its cores, the tab says "of N" and
// draws the capacity line.
func TestBuildPageResourcesKnowsTheCores(t *testing.T) {
	daemons := live{{Name: "daemon-a", Connected: true, InspectedAt: testNow,
		Inspection: collector.Inspection{Node: "node-1", Load: &diskprobe.Load{At: testNow, Cores: 16, MemTotal: 64 << 30}}}}
	ts, st, ids, _ := consoleWith(t, testOpts{Live: daemons})
	for _, id := range ids {
		p, _ := st.BuildPage(ctx, id)
		cpu := time.Duration(0)
		for _, s := range p.Build.Steps {
			cpu += s.Usage.CPU
		}
		if cpu == 0 {
			continue
		}
		pg := get(t, ts, fmt.Sprintf("/builds/%d", id))
		if !strings.Contains(pg.body, " of 16</b> cores busy on average") || !strings.Contains(pg.body, "the dashed line is the node's 16 cores") {
			t.Errorf("build %d: the node's cores not used", id)
		}
		return
	}
	t.Fatal("no build with CPU")
}

// The daemons' table draws each daemon's last hour and the latest
// reading; a daemon's page explains the node, the pod and the pressure.
func TestNodesLoad(t *testing.T) {
	var series []diskprobe.Sample
	for i := 0; i < 360; i++ {
		at := testNow.Add(-time.Hour + time.Duration(i)*10*time.Second).Unix()
		series = append(series, diskprobe.Sample{At: at, NodeCPU: 0.2, NodeMem: 2 << 30, PodCPU: 0.1, PodMem: 1 << 30})
	}
	load := &diskprobe.Load{At: testNow, Cores: 4, MemTotal: 8 << 30, Node: diskprobe.Point{CPU: 3.1, Mem: 5300 << 20},
		Pod: diskprobe.Point{CPU: 2.9, Mem: 4700 << 20}, PodKnown: true, PodMemLimit: 5809 << 20,
		Pressure: &diskprobe.Pressure{CPU60: 38, IO60: 2}, Series: series}
	daemons := live{
		{Name: "box-buildkit-abcde", Connected: true, InspectedAt: testNow, Inspection: collector.Inspection{Node: "node-1", Load: load, Disk: &diskprobe.Disk{Total: 100 << 30, Free: 60 << 30, Available: 55 << 30}}},
		{Name: "box-buildkit-fghij", Connected: true, InspectedAt: testNow, Inspection: collector.Inspection{Node: "node-2", LoadErr: "load probe: 404 Not Found", Disk: &diskprobe.Disk{Total: 100 << 30, Free: 60 << 30, Available: 55 << 30}}},
	}
	ts, _, _, _ := consoleWith(t, testOpts{Live: daemons, Release: "box"})
	nodes := get(t, ts, "/nodes")
	nodes.safe(t)
	for _, want := range []string{"Load <span class=\"muted\">last hour · CPU, memory</span>", `class="spark"`, `<span class="num">3.1 of 4</span> <span class="muted">· 5.2 GiB of 8.0 GiB</span>`,
		`waiting for CPU 38 %`, `title="load probe: 404 Not Found">no answer</span>`} {
		if !strings.Contains(nodes.body, want) {
			t.Errorf("nodes page lacks %q", want)
		}
	}
	node := get(t, ts, "/nodes/box-buildkit-abcde")
	node.safe(t)
	for _, want := range []string{`<h2 class="h" id="ldT">Load</h2>`, "<b>3.1 of 4</b> cores · daemon <b>2.9</b>", "<b>5.2 GiB of 8.0 GiB</b> · daemon <b>4.6 GiB</b> of its 5.7 GiB limit",
		"some task waited for a core 38 % of the time, for memory 0 %, for disk 2 %", `class="ch load"`, ">4 cores</span>", ">8.0 GiB</span>", `class="ch-x"`, "CPU: the daemon's pod", "<b>0</b> running now"} {
		if !strings.Contains(node.body, want) {
			t.Errorf("node page lacks %q", want)
		}
	}
	// The bars split the node into the pod, the rest and what is free.
	if !strings.Contains(node.body, `title="BuildKit daemon: 2.9 cores"`) || !strings.Contains(node.body, `title="Idle: 0.9 cores"`) ||
		!strings.Contains(node.body, `title="The rest of the node: 0.2 cores"`) {
		t.Errorf("the CPU bar does not split the node")
	}
	old := get(t, ts, "/nodes/box-buildkit-fghij")
	if !strings.Contains(old.body, "reports no load (load probe: 404 Not Found)") {
		t.Errorf("a probe without /load not explained")
	}
	// cgroup v1 (the pod not measured): the node alone, said so.
	v1 := *load
	v1.PodKnown, v1.Pod = false, diskprobe.Point{}
	daemons[0].Inspection.Load = &v1
	tsV1, _, _, _ := consoleWith(t, testOpts{Live: daemons, Release: "box"})
	if p := get(t, tsV1, "/nodes/box-buildkit-abcde"); !strings.Contains(p.body, "its own share is not measured") ||
		strings.Contains(p.body, "· daemon <b>") || strings.Contains(p.body, `class="pod"`) || !strings.Contains(p.body, `title="In use: 3.1 cores"`) {
		t.Errorf("a node without the pod's figures")
	}
	// A kernel without PSI: said, not zero.
	noPSI := *load
	noPSI.Pressure = nil
	daemons[0].Inspection.Load = &noPSI
	ts2, _, _, _ := consoleWith(t, testOpts{Live: daemons, Release: "box"})
	if p := get(t, ts2, "/nodes/box-buildkit-abcde"); !strings.Contains(p.body, "not reported by this node's kernel") || strings.Contains(p.body, "waiting for CPU") {
		t.Errorf("a kernel without PSI")
	}
}

// loadView without a probe at all is nil, so pages without one change
// nothing.
func TestLoadViewWithoutAProbe(t *testing.T) {
	if v := loadView(collector.Inspection{}, testNow, time.UTC); v != nil {
		t.Errorf("a view without a probe: %+v", v)
	}
	if v := loadView(collector.Inspection{LoadErr: "timeout"}, testNow, time.UTC); v == nil || v.Err != "timeout" {
		t.Errorf("an error not kept: %+v", v)
	}
	// One sample draws nothing yet (a line needs two).
	one := &diskprobe.Load{At: testNow, Cores: 2, MemTotal: 1 << 30, Series: []diskprobe.Sample{{At: testNow.Unix()}}}
	if v := loadView(collector.Inspection{Load: one}, testNow, time.UTC); v == nil || v.Spark != nil || v.CPUText != "0.0 of 2" {
		t.Errorf("one sample: %+v", v)
	}
}
