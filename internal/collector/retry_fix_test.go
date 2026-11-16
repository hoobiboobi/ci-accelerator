package collector

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
)

// A record whose daemon sends nothing for readIdle is given up with an
// error saying so, instead of holding its stream for good.
func TestStalledBlobReadIsGivenUp(t *testing.T) {
	old := readIdle
	readIdle = 200 * time.Millisecond
	t.Cleanup(func() { readIdle = old })
	c, _ := newCollector(t)
	f := recorded(t)
	slow := &stalling{fake: &fake{name: "daemon-slow", recs: f.recs, blobs: f.blobs}, release: make(chan struct{}), opened: make(chan struct{})}
	defer close(slow.release)
	done := make(chan error, 1)
	go func() { done <- c.Record(ctx, slow, f.recs[0]) }()
	select {
	case err := <-done:
		if !errors.Is(err, errStalled) {
			t.Fatalf("err = %v, want the stalled daemon's", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled blob read is not given up")
	}
	// The record was not taken: it is read again when it comes again.
	if err := c.Record(ctx, f, f.recs[0]); err != nil {
		t.Fatal(err)
	}
	if n := stored(t, c); n != 1 {
		t.Errorf("%d builds stored after the record came again, want 1", n)
	}
}

// flaky is a daemon whose blobs cannot be read the first n times, then can;
// its stream sends its records and stays open.
type flaky struct {
	*fake
	fails atomic.Int64
}

func (f *flaky) Open(ctx context.Context, d *controlapi.Descriptor) (io.ReadCloser, error) {
	if f.fails.Add(-1) >= 0 {
		return nil, errors.New("transient")
	}
	return f.fake.Open(ctx, d)
}

func (f *flaky) Listen(ctx context.Context, fn func(controlapi.BuildHistoryEventType, *controlapi.BuildHistoryRecord) error) error {
	if err := f.fake.Listen(ctx, fn); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// A record that could not be stored is tried again while its stream
// lasts: BuildKit does not send it again by itself.
func TestFailedRecordIsTriedAgain(t *testing.T) {
	old := retryEvery
	retryEvery = 50 * time.Millisecond
	t.Cleanup(func() { retryEvery = old })
	c, _ := newCollector(t)
	f := recorded(t)
	src := &flaky{fake: &fake{name: "daemon-a", recs: f.recs[:1], blobs: f.blobs}}
	src.fails.Store(1)
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		c.Run(rctx, src)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	for i := 0; i < 200 && stored(t, c) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if n := stored(t, c); n != 1 {
		t.Fatalf("%d builds stored, want the record tried again and stored", n)
	}
}
