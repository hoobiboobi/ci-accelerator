package web

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// TestUI runs the browser checks of test/ui against the console serving
// the fixtures, with a running job and two daemons, while one pipeline
// keeps changing (its page must update without losing what the reader
// typed). It needs Node >= 22 and Chrome: `make ui-test` (UI_TEST=1).
func TestUI(t *testing.T) {
	if os.Getenv("UI_TEST") == "" {
		t.Skip("browser checks: make ui-test")
	}
	node := os.Getenv("NODE")
	if node == "" {
		node = "node"
	}
	inst := history.InstancePrefix("https://gitlab.example.com/")
	started := time.Now().Add(-42 * time.Second)
	rj := store.Job{Instance: inst, JobID: 777001, PipelineID: 999, ProjectID: 42, Name: "deploy" + evil, StartedAt: started,
		CommitTitle: "running " + evil, Node: "node-1"}
	daemons := live{{Name: "box-buildkit-abcde", Connected: true, InspectedAt: time.Now(),
		Inspection: collector.Inspection{Node: "node-1", Version: "v0.33.1",
			GC:    []collector.GCRule{{Filters: []string{"type==exec.cachemount"}, Keep: 336 * time.Hour}, {MaxUsed: 40 << 30, MinFree: 16 << 30}},
			Usage: collector.Usage{Total: 300 << 20, Images: 200 << 20, Layers: 90 << 20, Mounts: 10 << 20}},
		Active: []collector.Active{{Daemon: "box-buildkit-abcde", Target: "api" + evil, Started: started}}},
		{Name: "box-buildkit-fghij", Connected: false}}
	ts, st, _, _ := consoleWith(t, Options{Running: running{rj}, Live: daemons, Release: "box"})
	if err := st.SaveJob(ctx, rj); err != nil {
		t.Fatal(err)
	}
	ps, err := st.Pipelines(ctx, 100)
	if err != nil || len(ps) == 0 {
		t.Fatal(ps, err)
	}
	key := store.PipelineKey(ps[0].Instance, ps[0].ID)

	stop, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stop.Done():
				return
			case <-tick.C:
				st.Announce(key)
			}
		}
	}()
	run, end := context.WithTimeout(ctx, 9*time.Minute)
	defer end()
	cmd := exec.CommandContext(run, node, "../../test/ui/ui.mjs", "--base", ts.URL, "--live", "/pipelines/"+key)
	out, err := cmd.CombinedOutput()
	t.Logf("\n%s", out)
	if err != nil {
		t.Fatalf("browser checks: %v", err)
	}
}
