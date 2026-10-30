package collector

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestClassify(t *testing.T) {
	now := time.Now()
	u := Classify([]Record{
		{Type: "regular", Description: "pulled from docker.io/library/python:3.13-slim@sha256:abc", Size: 100},
		{Type: "regular", Description: "pulled from docker.io/library/python:3.13-slim@sha256:abc", Size: 50},
		{Type: "regular", Description: "pulled from ghcr.io/org/base:1", Size: 7},
		{Type: "regular", Description: "mount / from exec /bin/sh -c pip install", Size: 30},
		{Type: "regular", Description: "[api 1/2] COPY api/ ./api", Size: 3},
		{Type: "exec.cachemount", Description: "cached mount /root/.cache/pip", Size: 20, LastUsed: &now},
		{Type: "exec.cachemount", Description: "cached mount /go", Size: 40},
		{Type: "source.local", Description: "local source for context", Size: 5},
		{Type: "frontend", Description: "x", Size: 1},
	})
	if u.Total != 256 || u.Images != 157 || u.Layers != 33 || u.Mounts != 60 || u.Contexts != 5 || u.Other != 1 {
		t.Errorf("%+v", u)
	}
	if u.ImageSizes["python:3.13-slim"] != 150 || u.ImageSizes["ghcr.io/org/base:1"] != 7 || len(u.ImageSizes) != 2 {
		t.Errorf("images %v", u.ImageSizes)
	}
	if len(u.MountList) != 2 || u.MountList[0].Size != 40 || !u.MountList[1].LastUsed.Equal(now) {
		t.Errorf("mounts %+v", u.MountList)
	}
}

func TestLiveState(t *testing.T) {
	var c Collector
	calls := 0
	c.OnLiveChange(func() { calls++ })
	stream := c.live.connect("d1")
	rec := func(ref, target string) *controlapi.BuildHistoryRecord {
		return &controlapi.BuildHistoryRecord{Ref: ref, FrontendAttrs: map[string]string{"target": target},
			CreatedAt: timestamppb.New(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))}
	}
	c.live.event("d1", controlapi.BuildHistoryEventType_STARTED, rec("r1", "api"))
	c.live.event("d1", controlapi.BuildHistoryEventType_STARTED, rec("r2", "worker"))
	c.live.event("d1", controlapi.BuildHistoryEventType_COMPLETE, rec("r1", "api"))
	// Records completed before the stream began do not announce anything.
	c.live.event("d1", controlapi.BuildHistoryEventType_COMPLETE, rec("old", "api"))
	ds := c.Daemons()
	if len(ds) != 1 || !ds[0].Connected || len(ds[0].Active) != 1 || ds[0].Active[0].Target != "worker" || ds[0].Active[0].Started.IsZero() {
		t.Fatalf("%+v", ds)
	}
	if calls != 4 {
		t.Errorf("%d announcements, want 4", calls)
	}
	// An inspection is announced when it finds something new, not every
	// minute.
	c.live.inspected("d1", Inspection{Node: "n1"}, nil)
	c.live.inspected("d1", Inspection{Node: "n1"}, nil)
	if calls != 5 {
		t.Errorf("%d announcements after two equal inspections, want 5", calls)
	}
	c.live.inspected("d1", Inspection{Node: "n1", Usage: Usage{Total: 1}}, nil)
	if calls != 6 {
		t.Errorf("a changed inspection was not announced")
	}
	c.live.disconnected("d1", stream)
	ds = c.Daemons()
	if ds[0].Connected || len(ds[0].Active) != 0 || ds[0].Inspection.Node != "n1" || ds[0].InspectedAt.IsZero() {
		t.Errorf("after the stream ended: %+v", ds[0])
	}
}

func TestForgetGoneDaemon(t *testing.T) {
	var c Collector
	old := c.live.connect("old")
	c.live.connect("new")
	c.live.forget("old", old)
	if ds := c.Daemons(); len(ds) != 1 || ds[0].Name != "new" {
		t.Errorf("%+v", ds)
	}
}

// An inspection that ends after its daemon was forgotten (a slow disk
// probe during a rollout) does not bring the daemon back.
func TestInspectionDoesNotResurrect(t *testing.T) {
	var l live
	l.forget("d", l.connect("d"))
	l.inspected("d", Inspection{Version: "v0.33.1"}, nil)
	l.inspected("d", Inspection{}, errors.New("timeout"))
	if _, ok := l.daemons["d"]; ok {
		t.Error("a forgotten daemon came back")
	}
}

// An event for a daemon the console has forgotten is taken, not a crash.
func TestEventAfterForget(t *testing.T) {
	var l live
	l.forget("d", l.connect("d"))
	l.event("d", controlapi.BuildHistoryEventType_STARTED, &controlapi.BuildHistoryRecord{Ref: "r"})
	if len(l.active["d"]) != 1 {
		t.Errorf("%+v", l.active)
	}
}

// scripted is a daemon whose stream sends recs, then waits for release,
// announces after as started, and stays open until its context ends.
type scripted struct {
	name      string
	recs      []*controlapi.BuildHistoryRecord
	after     []*controlapi.BuildHistoryRecord
	blobs     map[string][]byte
	listening chan struct{} // closed once recs are sent
	release   chan struct{}
	sent      chan struct{} // closed once after is sent
}

func (s *scripted) Name() string { return s.name }
func (s *scripted) Listen(ctx context.Context, fn func(controlapi.BuildHistoryEventType, *controlapi.BuildHistoryRecord) error) error {
	for _, r := range s.recs {
		if err := fn(controlapi.BuildHistoryEventType_COMPLETE, r); err != nil {
			return err
		}
	}
	close(s.listening)
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	for _, r := range s.after {
		if err := fn(controlapi.BuildHistoryEventType_STARTED, r); err != nil {
			return err
		}
	}
	close(s.sent)
	<-ctx.Done()
	return ctx.Err()
}
func (s *scripted) Open(ctx context.Context, d *controlapi.Descriptor) (io.ReadCloser, error) {
	return (&fake{blobs: s.blobs}).Open(ctx, d)
}

func newScripted(name string) *scripted {
	return &scripted{name: name, listening: make(chan struct{}), release: make(chan struct{}), sent: make(chan struct{})}
}

// The stream of a daemon that is gone ends late, after a new stream to
// the same daemon began (its address left the Service for a moment): the
// old stream's end leaves the new one's state alone.
func TestLateEndOfAnOldStream(t *testing.T) {
	c, _ := newCollector(t)
	oldCtx, gone := context.WithCancelCause(ctx)
	old := newScripted("d")
	oldDone := make(chan error)
	go func() { oldDone <- c.Run(oldCtx, old) }()
	<-old.listening

	curCtx, stop := context.WithCancel(ctx)
	defer stop()
	cur := newScripted("d")
	cur.after = []*controlapi.BuildHistoryRecord{{Ref: "r", CreatedAt: timestamppb.Now()}}
	go c.Run(curCtx, cur)
	<-cur.listening

	gone(ErrGone)
	<-oldDone
	close(cur.release)
	select {
	case <-cur.sent:
	case <-time.After(10 * time.Second):
		t.Fatal("the new stream's events were not taken")
	}
	ds := c.Daemons()
	if len(ds) != 1 || !ds[0].Connected || len(ds[0].Active) != 1 || ds[0].Active[0].Ref != "r" {
		t.Errorf("after the old stream ended: %+v", ds)
	}
}

// A daemon that is gone leaves no builds behind in memory; one whose
// stream only broke keeps them for when it comes back.
func TestGoneDaemonLeavesMemory(t *testing.T) {
	f := recorded(t)
	c, _ := newCollector(t)
	run := func(name string, cause error) {
		rctx, cancel := context.WithCancelCause(ctx)
		src := newScripted(name)
		src.recs, src.blobs = f.recs[:1], f.blobs
		done := make(chan error)
		go func() { done <- c.Run(rctx, src) }()
		<-src.listening
		cancel(cause)
		<-done
	}
	run("gone", ErrGone)
	run("broken", nil)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.daemons["gone"]; ok {
		t.Error("the builds of a daemon that is gone stay in memory")
	}
	if w := c.daemons["broken"]; w == nil || len(w.builds) != 1 {
		t.Error("a broken stream dropped its daemon's builds")
	}
}
