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
		// modules: 270 s then, 111 s so far; install: 48 s then.
		nb(`<code>make -j&#34;32&#34; modules</code> for 1 min 51 s; if the rest goes as last time, about 3 min 30 s left, from 2 of 2 remaining steps`),
		"Each step still to run counts as long as it took on",
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
	if !strings.Contains(p.body, "BuildKit finished this build") || strings.Contains(p.body, "left, from") {
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

// st is a step at seconds from t0: from < 0 not started, to < 0 not done.
type st struct {
	key, name, state string
	from, to         float64
}

func liveSteps(t0 time.Time, in ...st) []history.LiveStep {
	sec := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	var out []history.LiveStep
	for _, s := range in {
		l := history.LiveStep{Key: s.key, Name: s.name, State: s.state}
		if s.from >= 0 {
			l.Started = sec(s.from)
			if s.to >= 0 {
				l.Completed = sec(s.to)
			}
		}
		out = append(out, l)
	}
	return out
}

// refBuild is a stored build of these steps (state built or cached).
func refBuild(t0 time.Time, wall float64, in ...st) *history.Build {
	b := &history.Build{Started: t0, Completed: t0.Add(time.Duration(wall * float64(time.Second)))}
	for _, s := range liveSteps(t0, in...) {
		b.Steps = append(b.Steps, history.Step{Key: s.Key, Name: s.Name, State: history.StepState(s.State), Started: s.Started, Completed: s.Completed})
	}
	return b
}

// The rest of a running build takes what its steps still to run took in
// the reference, whatever came from the cache then or now.
func TestPredict(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 11, 37, 0, 0, time.UTC)
	r0 := t0.Add(-48 * time.Hour)
	sec := func(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
	for _, c := range []struct {
		name    string
		ref     *history.Build
		now     []st
		at      float64 // now, seconds from the start
		says    bool
		left    float64
		sure    int
		todo    int
		unknown []string
		place   map[float64]float64 // reference start -> likely start now
		delta   float64             // with known
		known   bool
	}{{
		// Finding: a ran then (10 min), comes from the cache now. The old
		// figure was the whole reference less 15 s: 14 min 5 s.
		name: "cached now, built then",
		ref:  refBuild(r0, 925, st{"[s 1/3]", "RUN a", "built", 0, 600}, st{"[s 2/3]", "RUN b", "built", 600, 900}, st{"[s 3/3]", "RUN c", "built", 900, 920}),
		now:  []st{{"[s 1/3]", "RUN a", "cached", 0, 0}, {"[s 2/3]", "RUN b", "running", 5, -1}, {"[s 3/3]", "RUN c", "waiting", -1, -1}},
		at:   15, says: true, left: 290 + 20, sure: 2, todo: 2,
		place: map[float64]float64{900: 15 + 290},
	}, {
		// Finding: a comes from the cache in the reference and runs now:
		// no time for it, the other three count; 3 of 4 is enough.
		name: "built now, cached then",
		ref: refBuild(r0, 335, st{"[s 1/4]", "RUN a", "cached", 0, 0}, st{"[s 2/4]", "RUN b", "built", 0, 300},
			st{"[s 3/4]", "RUN c", "built", 300, 320}, st{"[s 4/4]", "RUN d", "built", 320, 330}),
		now: []st{{"[s 1/4]", "RUN a", "running", 0, -1}, {"[s 2/4]", "RUN b", "waiting", -1, -1},
			{"[s 3/4]", "RUN c", "waiting", -1, -1}, {"[s 4/4]", "RUN d", "waiting", -1, -1}},
		at: 10, says: true, left: 330, sure: 3, todo: 4, unknown: []string{"[s 1/4]"},
		place: map[float64]float64{0: 10, 300: 310, 320: 330},
	}, {
		name: "too few steps with a time",
		ref:  refBuild(r0, 335, st{"[s 2/4]", "RUN b", "built", 0, 300}, st{"[s 3/4]", "RUN c", "built", 300, 320}),
		now: []st{{"[s 1/4]", "RUN a", "running", 0, -1}, {"[s 2/4]", "RUN b", "waiting", -1, -1},
			{"[s 3/4]", "RUN c", "waiting", -1, -1}, {"[s 4/4]", "RUN e", "waiting", -1, -1}},
		at: 10, says: false, left: 320, sure: 2, todo: 4, unknown: []string{"[s 1/4]", "[s 4/4]"},
	}, {
		name: "side by side then: counted once",
		ref:  refBuild(r0, 330, st{"[x 1/1]", "RUN x", "built", 0, 100}, st{"[y 1/1]", "RUN y", "built", 0, 300}, st{"[z 1/1]", "RUN z", "built", 300, 320}),
		now:  []st{{"[x 1/1]", "RUN x", "waiting", -1, -1}, {"[y 1/1]", "RUN y", "waiting", -1, -1}, {"[z 1/1]", "RUN z", "waiting", -1, -1}},
		at:   0, says: true, left: 320, sure: 3, todo: 3,
		place: map[float64]float64{0: 0, 300: 300},
	}, {
		// Finding 292: a build argument expanded in the text.
		name: "text changed: by place",
		ref:  refBuild(r0, 320, st{"[b 2/3]", "RUN make ID=1", "built", 10, 310}),
		now:  []st{{"[b 2/3]", "RUN make ID=2", "running", 12, -1}},
		at:   72, says: true, left: 240, sure: 1, todo: 1,
	}, {
		name: "another instruction at the place",
		ref:  refBuild(r0, 320, st{"[b 2/3]", "COPY . .", "built", 10, 310}),
		now:  []st{{"[b 2/3]", "RUN make ID=2", "running", 12, -1}},
		at:   72, says: false, left: 0, sure: 0, todo: 1, unknown: []string{"[b 2/3]"},
	}, {
		name: "a place two steps share matches nothing",
		ref:  refBuild(r0, 320, st{"[b 2/3]", "RUN make ID=1", "built", 10, 100}, st{"[b 2/3]", "RUN test ID=1", "built", 100, 310}),
		now:  []st{{"[b 2/3]", "RUN make ID=2", "running", 12, -1}},
		at:   72, says: false, sure: 0, todo: 1, unknown: []string{"[b 2/3]"},
	}, {
		name: "no key: no place",
		ref:  refBuild(r0, 320, st{"", "RUN make ID=1", "built", 10, 100}),
		now:  []st{{"", "RUN make ID=2", "running", 12, -1}},
		at:   72, says: false, sure: 0, todo: 1, unknown: []string{"make ID=2"},
	}, {
		name: "longer than last time: done any moment",
		ref:  refBuild(r0, 110, st{"[b 1/1]", "RUN b", "built", 0, 100}),
		now:  []st{{"[b 1/1]", "RUN b", "running", 0, -1}},
		at:   150, says: true, left: 0, sure: 1, todo: 1,
	}, {
		name: "20 s behind after a step both ran",
		ref:  refBuild(r0, 1100, st{"[a 1/1]", "RUN a", "built", 0, 100}, st{"[b 1/1]", "RUN b", "built", 100, 400}),
		now:  []st{{"[a 1/1]", "RUN a", "built", 0, 120}, {"[b 1/1]", "RUN b", "running", 120, -1}},
		at:   200, says: true, left: 220, sure: 1, todo: 1, known: true, delta: 20,
	}, {
		name: "every step done: nothing to say",
		ref:  refBuild(r0, 110, st{"[a 1/1]", "RUN a", "built", 0, 100}),
		now:  []st{{"[a 1/1]", "RUN a", "cached", 0, 0}},
		at:   10, says: false,
	}} {
		t.Run(c.name, func(t *testing.T) {
			p := predict(liveSteps(t0, c.now...), t0, t0.Add(sec(c.at)), refOf(c.ref))
			if p.says != c.says || p.left != sec(c.left) || p.sure != c.sure || p.todo != c.todo || p.total != sec(c.at+c.left) {
				t.Errorf("says %v left %v sure %d of %d total %v; want %v %v %d of %d", p.says, p.left, p.sure, p.todo, p.total, c.says, sec(c.left), c.sure, c.todo)
			}
			if strings.Join(p.unknown, "|") != strings.Join(c.unknown, "|") {
				t.Errorf("unknown %q, want %q", p.unknown, c.unknown)
			}
			if p.known != c.known || c.known && p.delta != sec(c.delta) {
				t.Errorf("known %v delta %v", p.known, p.delta)
			}
			for off, want := range c.place {
				if got := p.place(sec(off)); got != sec(want) {
					t.Errorf("place(%v) = %v, want %v", sec(off), got, sec(want))
				}
			}
		})
	}
}

// A step whose text changes every pipeline is found by its place in the
// Dockerfile; another image's build of the project is not a reference
// once the job's image is known; too few known steps: no figure, and why.
func TestRunningBuildByPlaceAndImage(t *testing.T) {
	inst := history.InstancePrefix("https://gitlab.example.com/")
	const pipe, job = 3_000_000_401, 9_900_001
	t0 := testNow.Add(-65 * time.Second)
	steps := liveSteps(t0, st{"[app 1/3]", "RUN npm ci", "cached", 0, 0}, st{"[app 2/3]", "RUN npm run build -- --build-id=48213377", "running", 5, -1},
		st{"[app 3/3]", "RUN ./gen.sh", "waiting", -1, -1})
	lb := collector.LiveBuild{Active: collector.Active{Daemon: "box-buildkit-abcde", Ref: "web-now", Started: t0, Instance: inst, Pipeline: pipe, Job: job},
		Steps: steps, Running: 1}
	f := following{live: live{{Name: "box-buildkit-abcde", Active: []collector.Active{lb.Active}}}, builds: map[string]*collector.LiveBuild{"web-now": &lb}}
	ts, sto, _, _ := consoleWith(t, testOpts{Live: f})
	rf := runningFixture{t, sto, inst}
	rf.job(pipe, store.Job{JobID: job, Name: "build-web", Stage: "build", StartedAt: t0, PodRunning: t0})
	save := func(ref, jobName string, pid, jid uint64, ago time.Duration, image string, in ...st) {
		t.Helper()
		b := refBuild(testNow.Add(-ago), 700, in...)
		b.Ref, b.Daemon, b.Images = ref, "box-buildkit-abcde", []string{image}
		b.Attribution = history.Attribution{State: history.Attributed, Instance: inst, Pipeline: pid, Job: jid}
		rf.job(pid, store.Job{JobID: jid, Name: jobName, Stage: "build", StartedAt: b.Started})
		if _, err := sto.SaveBuild(ctx, store.Build{Build: b, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	// The job's last build: its image, but the step came from the cache.
	save("web-prev", "build-web", pipe-100, job-100, time.Hour, "registry.example.com/web:abc",
		st{"[app 1/3]", "RUN npm ci", "cached", 0, 0}, st{"[app 2/3]", "RUN npm run build -- --build-id=1", "cached", 0, 0})
	// Another image's build, newer than the right one: not a reference.
	save("docs", "build-docs", pipe-200, job-200, 2*time.Hour, "registry.example.com/docs:1",
		st{"[app 2/3]", "RUN npm run build -- --build-id=2", "built", 0, 100}, st{"[app 3/3]", "RUN ./gen.sh", "built", 100, 110})
	// The same image from another job: the reference, matched by place.
	save("web-nightly", "nightly-web", pipe-300, job-300, 3*time.Hour, "registry.example.com/web:nightly",
		st{"[app 1/3]", "RUN npm ci", "built", 0, 600}, st{"[app 2/3]", "RUN npm run build -- --build-id=3", "built", 600, 790},
		st{"[app 3/3]", "RUN ./gen.sh", "built", 790, 810})

	p := get(t, ts, "/jobs/"+inst+"/9900001")
	p.safe(t)
	// 190 s then, 60 s so far; gen.sh 20 s.
	for _, want := range []string{
		nb("for 1 min 0 s; if the rest goes as last time, about 2 min 30 s left, from 2 of 2 remaining steps"),
		nb("last time 3 min 10 s"), ">by place</abbr>",
		"matched by place in the Dockerfile and instruction: <code>[app 2/3]</code> (RUN)",
		"from cache (last time built, ",
	} {
		if !strings.Contains(p.body, want) {
			t.Errorf("job page lacks %q", want)
		}
	}
	if strings.Contains(p.body, nb("last time 1 min 40 s")) {
		t.Error("another image's build is the reference")
	}

	// Two of the three steps to run have no earlier run: no figure.
	lb.Steps = liveSteps(t0, st{"[app 1/4]", "RUN npm ci", "built", 0, 3}, st{"[app 2/4]", "RUN npm run build -- --build-id=48213377", "running", 5, -1},
		st{"[app 3/4]", "RUN npx playwright install chromium", "waiting", -1, -1}, st{"[app 4/4]", "RUN ./smoke.sh", "waiting", -1, -1})
	save("web-4", "build-web", pipe-400, job-400, 4*time.Hour, "registry.example.com/web:4",
		st{"[app 1/4]", "RUN npm ci", "built", 0, 600}, st{"[app 2/4]", "RUN npm run build -- --build-id=4", "built", 600, 790})
	p = get(t, ts, "/jobs/"+inst+"/9900001")
	p.safe(t)
	for _, want := range []string{
		"; no time left to say: 2 of the 3 steps still to run have no earlier run",
		"No earlier run: <code>[app 3/4]</code>, <code>[app 4/4]</code>. The page says a time once at least 3 of every 4 steps still to run ran before.",
		">no earlier run<",
	} {
		if !strings.Contains(p.body, want) {
			t.Errorf("job page lacks %q", want)
		}
	}
	if strings.Contains(p.body, "left, from") {
		t.Error("a figure from too few known steps")
	}
}
