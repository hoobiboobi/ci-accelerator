package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// liveFixture is the daemons' state with one running build the browser
// checks move along: a step ends and the next one runs, a line is
// printed.
type liveFixture struct {
	mu      sync.Mutex
	daemons live
	build   *collector.LiveBuild
}

func (f *liveFixture) Daemons() []collector.DaemonState {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append(live(nil), f.daemons...)
	if f.build != nil && len(out) > 0 {
		out[0].Active = append(append([]collector.Active(nil), out[0].Active...), f.build.Active)
	}
	return out
}

func (f *liveFixture) LiveBuild(ctx context.Context, daemon, ref string) (*collector.LiveBuild, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.build == nil || f.build.Daemon != daemon || f.build.Ref != ref {
		return nil, nil
	}
	cp := *f.build
	cp.Steps = append([]history.LiveStep(nil), f.build.Steps...)
	return &cp, nil
}

const (
	uiLivePipeline = 3_000_736_800
	uiLiveJob      = 9_736_801
)

// seedLive stores a pipeline whose one job builds the kernel now, and
// returns its pages and a hook: POST /next ends the running step and runs
// the next, POST /line prints a line.
func seedLive(t *testing.T, st *store.Store, f *liveFixture) (pipe, job string, hook *httptest.Server) {
	t.Helper()
	keys, _ := pipelines(t, st)
	inst, _, _ := strings.Cut(keys[0], "/")
	t0 := testNow.Add(-(13*time.Minute + 2*time.Second))
	j := store.Job{Instance: inst, JobID: uiLiveJob, PipelineID: uiLivePipeline, ProjectID: 42, Name: "build-kernel", Stage: "build",
		Ref: "main", CommitTitle: "build the kernel", PipelineCreatedAt: t0.Add(-20 * time.Second), StartedAt: t0.Add(-10 * time.Second),
		PodCreated: t0.Add(-15 * time.Second), PodRunning: t0.Add(-10 * time.Second), URL: "https://gitlab.example.com/example/app/-/jobs/" + strconv.Itoa(uiLiveJob)}
	if err := st.SaveJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	saveKernelRef(t, st, inst, "build-kernel", uiLivePipeline-1, uiLiveJob-1)
	if _, err := st.SaveAPIPipeline(ctx, store.APIPipeline{Instance: inst, ID: uiLivePipeline, ProjectID: 42, Status: "running", Read: testNow,
		Jobs: []store.APIJob{{ID: uiLiveJob, Name: "build-kernel", Stage: "build", Status: "running", Queued: t0.Add(-14 * time.Second), Started: t0.Add(-10 * time.Second)}}}); err != nil {
		t.Fatal(err)
	}
	steps := kernelSteps(t0, map[string][2]float64{"[toolchain 2/2]": {4.4, 22}, "[source 2/2]": {22, 35}, "[vmlinux 1/1]": {39, 671}, "[modules 1/1]": {671, -1}},
		map[string]string{"[toolchain 2/2]": "built", "[source 2/2]": "built", "[vmlinux 1/1]": "built", "[modules 1/1]": "running", "[install 1/1]": "waiting"})
	steps[2].Lines, steps[2].Tail = 30671, []history.TailLine{{At: 631400 * time.Millisecond, Text: "Kernel: arch/x86/boot/bzImage is ready  (#1)"}}
	steps[3].Lines = 9100
	for i := 0; i < history.TailLines; i++ {
		steps[3].Tail = append(steps[3].Tail, history.TailLine{At: time.Duration(110000+100*i) * time.Millisecond, Text: fmt.Sprintf("  CC [M]  sound/soc/codecs/snd-soc-wm89%02d.mod.o", i)})
	}
	f.mu.Lock()
	f.build = &collector.LiveBuild{Active: collector.Active{Daemon: "box-buildkit-abcde", Ref: "ui-live-kernel", Started: t0, Instance: inst, Pipeline: uiLivePipeline, Job: uiLiveJob},
		Steps: steps, Running: 1, Load: fixtureLoad()}
	f.mu.Unlock()
	lines := 0
	hook = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		ss := f.build.Steps
		switch r.URL.Path {
		case "/line":
			for i := range ss {
				if ss[i].State == history.LiveRunning {
					lines++
					ss[i].Lines++
					ss[i].Tail = append(ss[i].Tail[1:], history.TailLine{At: time.Duration(120+lines) * time.Second, Text: fmt.Sprintf("  LD [M]  printed-line-%d.ko", lines)})
				}
			}
		case "/next":
			for i := range ss {
				if ss[i].State == history.LiveRunning {
					ss[i].State, ss[i].Completed = history.LiveBuilt, testNow.Add(-time.Second)
					if i+1 < len(ss) {
						ss[i+1].State, ss[i+1].Started = history.LiveRunning, testNow.Add(-time.Second)
						ss[i+1].Tail = []history.TailLine{{At: 0, Text: "  INSTALL /out/lib/modules/6.12.6/kernel/fs/ext4/ext4.ko"}}
						ss[i+1].Lines = 1
					}
					break
				}
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(hook.Close)
	return "/pipelines/" + store.PipelineKey(inst, uiLivePipeline), "/jobs/" + inst + "/" + strconv.Itoa(uiLiveJob), hook
}
