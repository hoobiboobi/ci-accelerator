package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
	digest "github.com/opencontainers/go-digest"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/hoobiboobi/ci-accelerator/internal/diskprobe"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// progressDaemon is a daemon whose running builds' progress the test
// feeds.
type progressDaemon struct {
	updates chan *client.SolveStatus
	err     error
	load    *diskprobe.Load

	mu    sync.Mutex
	calls int
}

func (f *progressDaemon) Name() string { return "d1" }
func (f *progressDaemon) Listen(ctx context.Context, fn func(controlapi.BuildHistoryEventType, *controlapi.BuildHistoryRecord) error) error {
	<-ctx.Done()
	return nil
}
func (f *progressDaemon) Open(ctx context.Context, d *controlapi.Descriptor) (io.ReadCloser, error) {
	return nil, errors.New("no blobs")
}
func (f *progressDaemon) Status(ctx context.Context, ref string, fn func(*client.SolveStatus)) error {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case s, ok := <-f.updates:
			if !ok {
				return nil
			}
			fn(s)
		}
	}
}
func (f *progressDaemon) ReadLoad(ctx context.Context) (*diskprobe.Load, error) { return f.load, nil }

func (f *progressDaemon) statusCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// sourceOnly is a daemon whose progress cannot be read.
type sourceOnly struct{}

func (sourceOnly) Name() string { return "d2" }
func (sourceOnly) Listen(ctx context.Context, fn func(controlapi.BuildHistoryEventType, *controlapi.BuildHistoryRecord) error) error {
	return nil
}
func (sourceOnly) Open(ctx context.Context, d *controlapi.Descriptor) (io.ReadCloser, error) {
	return nil, errors.New("no blobs")
}

const testGitLab = "https://gitlab.com"

func startedRec(ref string, opts map[string]string) *controlapi.BuildHistoryRecord {
	return &controlapi.BuildHistoryRecord{Ref: ref, FrontendAttrs: opts, CreatedAt: timestamppb.New(time.Date(2026, 10, 30, 11, 37, 0, 0, time.UTC))}
}

func traceparent(pipeline, job uint64) string {
	return fmt.Sprintf("00-01%s%012x%012x-0000000419350c4d-01", history.InstancePrefix(testGitLab), pipeline, job)
}

func liveCollector() *Collector {
	return &Collector{live: live{instances: map[string]bool{history.InstancePrefix(testGitLab): true}}}
}

func shortWatch(t *testing.T) {
	t.Helper()
	for _, v := range []*time.Duration{&watchFor, &watchTick, &firstWait} {
		old := *v
		t.Cleanup(func() { *v = old })
	}
	watchFor, watchTick, firstWait = 150*time.Millisecond, 20*time.Millisecond, 500*time.Millisecond
}

func TestRunningBuildNamesItsJob(t *testing.T) {
	c := liveCollector()
	c.live.connect("d1")
	c.live.event("d1", controlapi.BuildHistoryEventType_STARTED, startedRec("r1", map[string]string{
		history.TraceOpt: traceparent(3007867305, 17603024205), "build-arg:API_TOKEN": "s3cr3t-value", "target": "app"}))
	c.live.event("d1", controlapi.BuildHistoryEventType_STARTED, startedRec("r2", map[string]string{history.TraceOpt: "00-" + strings.Repeat("ab", 16) + "-0000000419350c4d-01"}))
	c.live.event("d1", controlapi.BuildHistoryEventType_STARTED, startedRec("r3", nil))
	ds := c.Daemons()
	if len(ds) != 1 || len(ds[0].Active) != 3 {
		t.Fatalf("%+v", ds)
	}
	by := map[string]Active{}
	for _, a := range ds[0].Active {
		by[a.Ref] = a
	}
	if a := by["r1"]; a.Instance != history.InstancePrefix(testGitLab) || a.Pipeline != 3007867305 || a.Job != 17603024205 || a.Target != "app" {
		t.Errorf("r1: %+v", a)
	}
	if a := by["r2"]; a.Job != 0 || a.Instance != "" {
		t.Errorf("a trace of no known GitLab names no job: %+v", a)
	}
	if a := by["r3"]; a.Job != 0 {
		t.Errorf("no option, no job: %+v", a)
	}
	// What the daemons' state shows the web carries no build argument.
	if b, _ := json.Marshal(ds); strings.Contains(string(b), "s3cr3t") {
		t.Errorf("a build argument leaves the collector: %s", b)
	}
	c.live.event("d1", controlapi.BuildHistoryEventType_COMPLETE, startedRec("r1", nil))
	if _, ok := c.live.args["d1"]["r1"]; ok {
		t.Error("a completed build's arguments are kept")
	}
}

func TestLiveBuildProgress(t *testing.T) {
	shortWatch(t)
	f := &progressDaemon{updates: make(chan *client.SolveStatus, 8), load: &diskprobe.Load{Cores: 32}}
	c := liveCollector()
	c.live.connectSource("d1", f)
	c.live.event("d1", controlapi.BuildHistoryEventType_STARTED, startedRec("r1", map[string]string{"build-arg:API_TOKEN": "s3cr3t-value"}))
	ctx := context.Background()
	if lb, err := c.LiveBuild(ctx, "d1", "nope"); lb != nil || err != nil {
		t.Fatalf("a build the daemon does not run: %+v %v", lb, err)
	}
	started := time.Now()
	run := "[app 2/3] RUN ./build --token s3cr3t-value"
	f.updates <- &client.SolveStatus{Vertexes: []*client.Vertex{{Digest: digest.FromString(run), Name: run, Started: &started}},
		Logs: []*client.VertexLog{{Vertex: digest.FromString(run), Data: []byte("using s3cr3t-value\n"), Timestamp: started}}}
	lb, err := c.LiveBuild(ctx, "d1", "r1")
	if err != nil || lb == nil {
		t.Fatalf("%+v %v", lb, err)
	}
	if len(lb.Steps) != 1 || lb.Steps[0].State != history.LiveRunning || lb.Running != 1 || lb.Ref != "r1" {
		t.Fatalf("%+v", lb)
	}
	if b, _ := json.Marshal(lb); strings.Contains(string(b), "s3cr3t") {
		t.Errorf("a secret build argument shows: %s", b)
	}
	// The node's load arrives beside the progress.
	for i := 0; lb.Load == nil && i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		lb, _ = c.LiveBuild(ctx, "d1", "r1")
	}
	if lb.Load == nil || lb.Load.Cores != 32 {
		t.Errorf("load %+v", lb.Load)
	}
	// One reading per build, however often the page asks.
	if n := f.statusCalls(); n != 1 {
		t.Errorf("%d readings of one build", n)
	}
	close(f.updates)
	for i := 0; !lb.Done && i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		lb, _ = c.LiveBuild(ctx, "d1", "r1")
	}
	if !lb.Done {
		t.Error("a build whose stream ended is not done")
	}
}

// Nobody asks for a minute: the reading stops, and the next ask reads the
// build again from its start.
func TestLiveBuildStopsUnwatched(t *testing.T) {
	shortWatch(t)
	f := &progressDaemon{updates: make(chan *client.SolveStatus)}
	c := liveCollector()
	c.live.connectSource("d1", f)
	c.live.event("d1", controlapi.BuildHistoryEventType_STARTED, startedRec("r1", nil))
	ctx := context.Background()
	if _, err := c.LiveBuild(ctx, "d1", "r1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * watchFor)
	c.watch.mu.Lock()
	w := c.watch.m["d1\x00r1"]
	c.watch.mu.Unlock()
	w.mu.Lock()
	ended, done := w.ended, w.done
	w.mu.Unlock()
	if !ended || done {
		t.Fatalf("an unwatched build's reading: ended %v, done %v", ended, done)
	}
	if _, err := c.LiveBuild(ctx, "d1", "r1"); err != nil {
		t.Fatal(err)
	}
	if n := f.statusCalls(); n != 2 {
		t.Errorf("%d readings, want 2", n)
	}
}

func TestLiveBuildLimits(t *testing.T) {
	shortWatch(t)
	watchFor, firstWait = 10*time.Second, 10*time.Millisecond
	ctx := context.Background()
	c := liveCollector()
	c.live.connectSource("d2", sourceOnly{})
	c.live.event("d2", controlapi.BuildHistoryEventType_STARTED, startedRec("r", nil))
	if _, err := c.LiveBuild(ctx, "d2", "r"); !errors.Is(err, ErrNoProgress) {
		t.Errorf("a daemon without progress: %v", err)
	}
	f := &progressDaemon{updates: make(chan *client.SolveStatus)}
	c.live.connectSource("d1", f)
	for i := 0; i <= MaxWatched; i++ {
		ref := fmt.Sprint("r", i)
		c.live.event("d1", controlapi.BuildHistoryEventType_STARTED, startedRec(ref, nil))
		_, err := c.LiveBuild(ctx, "d1", ref)
		if i < MaxWatched && err != nil {
			t.Fatalf("build %d: %v", i, err)
		}
		if i == MaxWatched && !errors.Is(err, ErrTooManyWatched) {
			t.Fatalf("build %d over the bound: %v", i, err)
		}
	}
	// A reading that failed shows its error, and is not tried again on
	// every ask.
	g := &progressDaemon{err: errors.New("no such job")}
	c3 := liveCollector()
	c3.live.connectSource("d1", g)
	c3.live.event("d1", controlapi.BuildHistoryEventType_STARTED, startedRec("r", nil))
	lb, _ := c3.LiveBuild(ctx, "d1", "r")
	for i := 0; i < 3; i++ {
		lb, _ = c3.LiveBuild(ctx, "d1", "r")
	}
	if lb == nil || lb.Err == "" || g.statusCalls() != 1 {
		t.Errorf("failed reading: %+v after %d tries", lb, g.statusCalls())
	}
}
