package collector

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
	"google.golang.org/protobuf/proto"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// stalling is a daemon whose blob reads hang until released (or their
// context ends): a daemon that accepted the call and stopped answering.
type stalling struct {
	*fake
	release chan struct{}
	opened  chan struct{}
	once    sync.Once
}

func (s *stalling) Open(ctx context.Context, d *controlapi.Descriptor) (io.ReadCloser, error) {
	s.once.Do(func() { close(s.opened) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.fake.Open(ctx, d)
}

// A daemon that stalls in the middle of a record holds up only its own
// records: another daemon's are stored, and a caller of JobKnown with a
// deadline stops waiting when it passes.
func TestStalledDaemonDoesNotHoldTheOthers(t *testing.T) {
	c, _ := newCollector(t)
	f := recorded(t)
	slow := &stalling{fake: &fake{name: "daemon-slow", recs: f.recs, blobs: f.blobs}, release: make(chan struct{}), opened: make(chan struct{})}
	var wg sync.WaitGroup
	defer wg.Wait()
	defer close(slow.release)
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Record(ctx, slow, f.recs[0])
	}()
	<-slow.opened

	other := &fake{name: "daemon-b", recs: f.recs, blobs: f.blobs}
	done := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		done <- c.Record(ctx, other, f.recs[1])
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("other daemon's record: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("another daemon's record waits for the stalled daemon's blob read")
	}

	jdone := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		jctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		jdone <- c.JobKnown(jctx, "x", 1)
	}()
	select {
	case <-jdone:
	case <-time.After(3 * time.Second):
		t.Fatal("JobKnown with a 200 ms deadline still waits after 3 s")
	}
}

func stored(t *testing.T, c *Collector) int {
	t.Helper()
	builds, err := c.store.Recent(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(builds)
}

// A record whose trace is larger than the collector reads (any build can
// add spans to it) is stored without its job, not dropped.
func TestOversizedTraceStoresTheBuildUnattributed(t *testing.T) {
	c, _ := newCollector(t)
	f := recorded(t)
	rec := proto.Clone(f.recs[0]).(*controlapi.BuildHistoryRecord)
	if rec.Trace == nil {
		t.Skip("the fixture has no trace")
	}
	blobs := map[string][]byte{}
	for k, v := range f.blobs {
		blobs[k] = v
	}
	blobs[rec.Trace.Digest] = bytes.Repeat([]byte(" "), 64<<20+1)
	if err := c.Record(ctx, &fake{name: "daemon-a", blobs: blobs}, rec); err != nil {
		t.Fatalf("not stored: %v", err)
	}
	builds, err := c.store.Recent(ctx, 10)
	if err != nil || len(builds) != 1 {
		t.Fatalf("%d builds, %v", len(builds), err)
	}
	if a := builds[0].Build.Attribution; a.State != history.Unattributed {
		t.Errorf("attribution %+v, want unattributed", a)
	}
}

// panicking is a daemon whose blobs make the collector panic.
type panicking struct{ *fake }

func (p *panicking) Open(context.Context, *controlapi.Descriptor) (io.ReadCloser, error) {
	panic("a record nobody foresaw")
}

// A record that panics is that record's error: the stream goes on (the
// daemon would send the same record again after a restart).
func TestPanickingRecordDoesNotEndTheController(t *testing.T) {
	c, _ := newCollector(t)
	f := recorded(t)
	if err := c.Run(ctx, &panicking{&fake{name: "daemon-a", recs: f.recs[:2], blobs: f.blobs}}); err != nil {
		t.Fatal(err)
	}
}

// A daemon unready for a moment leaves the Service for one lookup and
// comes back: its stream goes on, and Drain later does not cut it.
func TestSupervisorKeepsADaemonBackFromAMomentUnready(t *testing.T) {
	c, _ := newCollector(t)
	var mu sync.Mutex
	on := true
	var dials atomic.Int64
	ended := make(chan error, 4)
	sup := &Supervisor{Collector: c, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Every: 10 * time.Millisecond, Drain: 300 * time.Millisecond,
		Resolve: func(context.Context) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			if on {
				return []string{"tcp://a:1"}, nil
			}
			return nil, nil
		},
		Dial: func(ctx context.Context, addr string) (Source, error) {
			dials.Add(1)
			return &watched{blocking: &blocking{addr: "pod-a", break_: make(chan struct{})}, ended: ended}, nil
		}}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		sup.Run(rctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	on = false
	mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	on = true
	mu.Unlock()
	time.Sleep(4 * sup.Drain)
	select {
	case err := <-ended:
		t.Errorf("the stream of a daemon back in the Service was cut: %v", err)
	default:
	}
	if n := dials.Load(); n != 1 {
		t.Errorf("%d dials, want the first stream kept", n)
	}
	if ds := c.Daemons(); len(ds) != 1 || !ds[0].Connected {
		t.Errorf("daemons %+v", ds)
	}
}

// watched tells when its stream ends.
type watched struct {
	*blocking
	ended chan error
}

func (w *watched) Listen(ctx context.Context, fn func(controlapi.BuildHistoryEventType, *controlapi.BuildHistoryRecord) error) error {
	err := w.blocking.Listen(ctx, fn)
	if errors.Is(context.Cause(ctx), ErrGone) {
		w.ended <- context.Cause(ctx)
	}
	return err
}

// BuildKit sends each event in its own goroutine: a STARTED that comes
// after its build's COMPLETE does not leave the build running for good.
func TestStartedAfterCompleteIsNotRunning(t *testing.T) {
	c := liveCollector()
	c.live.connect("d1")
	c.live.event("d1", controlapi.BuildHistoryEventType_COMPLETE, startedRec("r1", nil))
	c.live.event("d1", controlapi.BuildHistoryEventType_STARTED, startedRec("r1", nil))
	if ds := c.Daemons(); len(ds) != 1 || len(ds[0].Active) != 0 {
		t.Errorf("a completed build counts as running: %+v", ds)
	}
	// A new stream starts afresh.
	c.live.connect("d1")
	c.live.event("d1", controlapi.BuildHistoryEventType_STARTED, startedRec("r1", nil))
	if ds := c.Daemons(); len(ds[0].Active) != 1 {
		t.Errorf("a new stream's running build is not running: %+v", ds)
	}
}

// stuckStatus sends one update of a build's progress and then nothing.
type stuckStatus struct{ progressDaemon }

func (s *stuckStatus) Status(ctx context.Context, ref string, fn func(*client.SolveStatus)) error {
	fn(&client.SolveStatus{})
	<-ctx.Done()
	return ctx.Err()
}

// Builds that all name one job (a build names its job itself) do not take
// every watcher: another job's running build can still be followed.
func TestBuildsOfOneJobDoNotTakeEveryWatcher(t *testing.T) {
	shortWatch(t)
	c := liveCollector()
	src := &stuckStatus{}
	c.live.connectSource("d1", src)
	opts := map[string]string{history.TraceOpt: traceparent(1001, 2002)}
	for i := 0; i < 32; i++ {
		ref := "forged-" + string(rune('a'+i))
		c.live.event("d1", controlapi.BuildHistoryEventType_STARTED, startedRec(ref, opts))
		c.LiveBuild(ctx, "d1", ref)
	}
	c.live.event("d1", controlapi.BuildHistoryEventType_STARTED, startedRec("other", map[string]string{history.TraceOpt: traceparent(1003, 2004)}))
	if _, err := c.LiveBuild(ctx, "d1", "other"); err != nil {
		t.Errorf("another job's running build: %v", err)
	}
}
