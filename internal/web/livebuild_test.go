package web

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/diskprobe"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// following is the daemons' state with the running builds the collector
// follows.
type following struct {
	live
	builds map[string]*collector.LiveBuild // by ref
}

func (f following) LiveBuild(ctx context.Context, daemon, ref string) (*collector.LiveBuild, error) {
	lb := f.builds[ref]
	if lb == nil || lb.Daemon != daemon {
		return nil, nil
	}
	return lb, nil
}

// nb puts the pages' no-break space between a number and its unit.
func nb(s string) string { return numUnit.ReplaceAllString(s, "$1\u00a0$2") }

var numUnit = regexp.MustCompile(`(\d) (min|s|h)\b`)

// kernelSteps are the steps of a kernel build (win-runner, 2026-10-30),
// at seconds from the build's start; to < 0: still running; from < 0: not
// started.
func kernelSteps(t0 time.Time, at map[string][2]float64, state map[string]string) []history.LiveStep {
	keys := []struct{ key, name string }{
		{"[toolchain 2/2]", "RUN apt-get update && apt-get install -y build-essential"},
		{"[source 2/2]", "RUN curl -fsSL https://cdn.kernel.org/linux-6.12.6.tar.xz | tar -xJ"},
		{"[vmlinux 1/1]", `RUN make -j"32" vmlinux bzImage`},
		{"[modules 1/1]", `RUN make -j"32" modules`},
		{"[install 1/1]", "RUN make INSTALL_MOD_PATH=/out modules_install"},
	}
	sec := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	var out []history.LiveStep
	for _, k := range keys {
		a, ok := at[k.key]
		s := history.LiveStep{Key: k.key, Name: k.name, State: state[k.key]}
		if ok && a[0] >= 0 {
			s.Started = sec(a[0])
			if a[1] >= 0 {
				s.Completed = sec(a[1])
			}
		}
		out = append(out, s)
	}
	return out
}

// saveKernelRef stores the last cold build of the kernel image (28 Oct):
// vmlinux took 11 min 41 s, the whole build 18 min 20 s.
func saveKernelRef(t *testing.T, st *store.Store, inst, jobName string, pipe, job uint64) {
	t.Helper()
	pt0 := testNow.Add(-48 * time.Hour)
	prev := &history.Build{Ref: fmt.Sprint("kernel-prev-", job), Daemon: "box-buildkit-abcde", Started: pt0, Completed: pt0.Add(1100 * time.Second),
		Attribution: history.Attribution{State: history.Attributed, Instance: inst, Pipeline: pipe, Job: job}}
	for _, s := range kernelSteps(pt0, map[string][2]float64{"[toolchain 2/2]": {4, 21}, "[source 2/2]": {21, 34}, "[vmlinux 1/1]": {39, 740},
		"[modules 1/1]": {740, 1010}, "[install 1/1]": {1010, 1058}}, nil) {
		prev.Steps = append(prev.Steps, history.Step{Key: s.Key, Name: s.Name, State: history.Built, Started: s.Started, Completed: s.Completed})
	}
	runningFixture{t, st, inst}.job(pipe, store.Job{JobID: job, Name: jobName, Stage: "build", StartedAt: pt0})
	if _, err := st.SaveBuild(ctx, store.Build{Build: prev, Generation: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestRunningBuildOnItsJobPage(t *testing.T) {
	inst := history.InstancePrefix("https://gitlab.example.com/")
	const pipe, job = 3_000_000_201, 9_700_001
	t0 := testNow.Add(-(13*time.Minute + 2*time.Second))
	steps := kernelSteps(t0, map[string][2]float64{"[toolchain 2/2]": {4.4, 22}, "[source 2/2]": {22, 35}, "[vmlinux 1/1]": {39, 671}, "[modules 1/1]": {671, -1}},
		map[string]string{"[toolchain 2/2]": "built", "[source 2/2]": "cached", "[vmlinux 1/1]": "built", "[modules 1/1]": "running", "[install 1/1]": "waiting"})
	steps[2].Lines, steps[2].Tail = 30671, []history.TailLine{{At: 631400 * time.Millisecond, Text: "Kernel: arch/x86/boot/bzImage is ready  (#1)"}}
	steps[3].Lines, steps[3].Tail = 9106, []history.TailLine{{At: 110600 * time.Millisecond, Text: "  CC [M]  sound/soc/codecs/snd-soc-wm8995.mod.o"},
		{At: 110700 * time.Millisecond, Text: "  CC [M]  " + evil}}
	now := collector.LiveBuild{Active: collector.Active{Daemon: "box-buildkit-abcde", Ref: "kernel-now", Started: t0, Instance: inst, Pipeline: pipe, Job: job},
		Steps: steps, Running: 1, Load: &diskprobe.Load{At: testNow, Cores: 32, MemTotal: 64 << 30, Node: diskprobe.Point{CPU: 31.2, Mem: 14 << 30},
			Pod: diskprobe.Point{CPU: 30.4, Mem: 11 << 30}, PodKnown: true, Pressure: &diskprobe.Pressure{CPU60: 22}}}
	// A build that names the job with another pipeline is not the job's.
	other := now
	other.Ref, other.Pipeline = "kernel-elsewhere", pipe+1
	other.Steps = []history.LiveStep{{Key: "[x 1/1]", Name: "RUN echo another pipeline's build", State: "running", Started: t0}}
	f := following{live: live{{Name: "box-buildkit-abcde", Connected: true, Inspection: collector.Inspection{Node: "node-1"},
		Active: []collector.Active{now.Active, other.Active}}}, builds: map[string]*collector.LiveBuild{"kernel-now": &now, "kernel-elsewhere": &other}}
	ts, st, _, _ := consoleWith(t, testOpts{Live: f})
	rf := runningFixture{t, st, inst}
	rf.job(pipe, store.Job{JobID: job, Name: "build-image", Stage: "build", StartedAt: t0.Add(-5 * time.Second),
		PodCreated: t0.Add(-8 * time.Second), PodRunning: t0.Add(-5 * time.Second)})
	saveKernelRef(t, st, inst, "build-image", 3_000_000_101, 9_700_000)

	p := get(t, ts, "/jobs/"+inst+"/9700001")
	p.safe(t)
	for _, want := range []string{
		`data-refresh-every="2"`,
		nb("Building now · 13 min 2 s · node-1"),
		nb(`<code>make -j&#34;32&#34; modules</code> for 1 min 51 s; if the rest goes as last time, done in about 4 min 10 s`),
		nb("So far <b>1 min 9 s faster</b> than the last build of this image that ran these steps"),
		nb("took 10 min 32 s against 11 min 41 s"),
		"31.2 of 32 cores", "daemon 30.4 cores", "22 %",
		"3 of 5 done · 1 from cache",
		nb("9 106 lines so far · 1 min 51 s"), "30 671 lines",
		nb("10 min 32 s, last time 11 min 41 s"),
		"&lt;script&gt;alert(1)&lt;/script&gt;",
		"From the cache: nothing ran.",
	} {
		if !strings.Contains(p.body, want) {
			t.Errorf("job page lacks %q", want)
		}
	}
	if strings.Contains(p.body, "another pipeline") {
		t.Error("a build that names the job with another pipeline shows on its page")
	}
	// The running step opens and says so to the script (data-auto); the
	// others are folded; the step not started has nothing to open but its
	// time the last time.
	running := regexp.MustCompile(`<details class="lstep st-running" id="ls-[0-9a-f]{12}" data-auto open>`)
	if !running.MatchString(p.body) {
		t.Error("the running step is not open")
	}
	if regexp.MustCompile(`<details class="lstep st-built"[^>]* open>`).MatchString(p.body) {
		t.Error("a step that is done is open")
	}
	if !strings.Contains(p.body, `<div class="lstep wait">`) || !strings.Contains(p.body, "c-ghost") {
		t.Error("the waiting step and the last time's bars")
	}
	if strings.Contains(p.body, "has no finished image build") {
		t.Error("the page still says the job has no build while it shows its build")
	}

	// The pipeline's timeline opens the running job into its stages so far.
	pp := get(t, ts, "/pipelines/"+inst+"/3000000201")
	pp.safe(t)
	if !regexp.MustCompile(`class="row k-job[^"]*"[^>]*data-text="build-image" aria-expanded="true"`).MatchString(pp.body) {
		t.Error("the running job is not open on its pipeline's timeline")
	}
	if !strings.Contains(pp.body, nb("running · ")) || !strings.Contains(pp.body, "c-running") || !strings.Contains(pp.body, `data-refresh-every="5"`) {
		t.Error("the running job's timeline: label, running bars or refresh")
	}
	if regexp.MustCompile(`data-wstep="0/`).MatchString(pp.body) {
		t.Error("a running build's steps open the step panel of build 0")
	}

	// BuildKit finished the build: the page says it is being stored.
	now.Done = true
	p = get(t, ts, "/jobs/"+inst+"/9700001")
	if !strings.Contains(p.body, "BuildKit finished this build") || strings.Contains(p.body, "done in about") {
		t.Error("a finished build")
	}
	// A build whose progress cannot be read says so.
	now.Done, now.Err = false, "its progress could not be read: no such job"
	if p = get(t, ts, "/jobs/"+inst+"/9700001"); !strings.Contains(p.body, "The build runs, but its progress could not be read") {
		t.Error("a build whose progress cannot be read")
	}
}

// Without an earlier build of the image there is nothing to say how long
// the rest takes: the page says what runs, nothing more.
func TestRunningBuildWithoutHistory(t *testing.T) {
	inst := history.InstancePrefix("https://gitlab.example.com/")
	t0 := testNow.Add(-30 * time.Second)
	lb := collector.LiveBuild{Active: collector.Active{Daemon: "box-buildkit-abcde", Ref: "new", Target: "brand-new", Started: t0, Instance: inst, Pipeline: 3_000_000_301, Job: 9_800_001},
		Steps: []history.LiveStep{{Key: "[base 1/2]", Name: "RUN make", State: "running", Started: t0.Add(time.Second)}}, Running: 3}
	f := following{live: live{{Name: "box-buildkit-abcde", Active: []collector.Active{lb.Active}}}, builds: map[string]*collector.LiveBuild{"new": &lb}}
	ts, st, _, _ := consoleWith(t, testOpts{Live: f})
	runningFixture{t, st, inst}.job(3_000_000_301, store.Job{JobID: 9_800_001, Name: "build-new", StartedAt: t0, PodRunning: t0})
	p := get(t, ts, "/jobs/"+inst+"/9800001")
	p.safe(t)
	if !strings.Contains(p.body, nb("<code>make</code> for 29 s</div>")) || strings.Contains(p.body, "goes as last time") || strings.Contains(p.body, "faster") {
		t.Errorf("no history: %s", between(p.body, `class="vt"`, "</div>"))
	}
	if !strings.Contains(p.body, "Nothing printed yet.") || strings.Contains(p.body, "Node CPU") {
		t.Error("a step with no lines yet; no load without a probe")
	}
}

func TestPredict(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 11, 37, 0, 0, time.UTC)
	ref := &liveRef{wall: 1100 * time.Second, steps: map[string]refStep{
		liveKey("[a 1/1]", "RUN a"): {off: 0, dur: 100 * time.Second, built: true},
		liveKey("[b 1/1]", "RUN b"): {off: 100 * time.Second, dur: 300 * time.Second, built: true},
	}}
	s := func(key, name, state string, from, to float64) history.LiveStep {
		st := history.LiveStep{Key: key, Name: name, State: state, Started: t0.Add(time.Duration(from) * time.Second)}
		if to >= 0 {
			st.Completed = t0.Add(time.Duration(to) * time.Second)
		}
		return st
	}
	// 20 s behind after a; b runs.
	p := predict([]history.LiveStep{s("[a 1/1]", "RUN a", "built", 0, 120), s("[b 1/1]", "RUN b", "running", 120, -1)}, t0, t0.Add(200*time.Second), ref)
	if !p.known || p.delta != 20*time.Second || p.total != 1120*time.Second || p.remaining != 920*time.Second {
		t.Errorf("behind: %+v", p)
	}
	// b already ran 100 s longer than the last time: the end moves out.
	p = predict([]history.LiveStep{s("[a 1/1]", "RUN a", "built", 0, 120), s("[b 1/1]", "RUN b", "running", 120, -1)}, t0, t0.Add(520*time.Second), ref)
	if p.total != 1220*time.Second {
		t.Errorf("overrun: %+v", p)
	}
	// A step from the cache this time says nothing of the pace.
	p = predict([]history.LiveStep{s("[a 1/1]", "RUN a", "cached", 0, 0)}, t0, t0.Add(10*time.Second), ref)
	if p.known || p.total != 1100*time.Second {
		t.Errorf("cached: %+v", p)
	}
}
