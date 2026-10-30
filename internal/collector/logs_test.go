package collector

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	digest "github.com/opencontainers/go-digest"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// loud is a daemon with one record whose steps printed a lot: its progress
// blob is made as it is read.
type loud struct {
	steps, frames, frameBytes int
	t0                        time.Time
}

func (l *loud) Name() string { return "loud" }
func (l *loud) Listen(context.Context, func(controlapi.BuildHistoryEventType, *controlapi.BuildHistoryRecord) error) error {
	return nil
}

func (l *loud) record() *controlapi.BuildHistoryRecord {
	return &controlapi.BuildHistoryRecord{Ref: "loud", CreatedAt: timestamppb.New(l.t0), CompletedAt: timestamppb.New(l.t0.Add(time.Hour)),
		Logs: &controlapi.Descriptor{Digest: "sha256:logs", Size: int64(l.steps * l.frames * l.frameBytes)}}
}

// progress writes the blob: each step's vertex, then its log in frames,
// the last line of each "END <step>".
func (l *loud) progress(w io.Writer) error {
	frame := func(m *controlapi.StatusResponse) error {
		b, err := m.MarshalVT()
		if err != nil {
			return err
		}
		if err := binary.Write(w, binary.LittleEndian, uint32(len(b))); err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	}
	line := bytes.Repeat([]byte("x"), l.frameBytes-1)
	line = append(line, '\n')
	for i := range l.steps {
		d := digest.FromString(fmt.Sprint(i)).String()
		start := timestamppb.New(l.t0.Add(time.Duration(i+1) * time.Second))
		if err := frame(&controlapi.StatusResponse{Vertexes: []*controlapi.Vertex{{Digest: d,
			Name: fmt.Sprintf("[s %d/%d] RUN step %d", i+1, l.steps, i), Started: start, Completed: start}}}); err != nil {
			return err
		}
		for j := range l.frames {
			msg := line
			if j == l.frames-1 {
				msg = fmt.Appendf(nil, "END %d\n", i)
			}
			if err := frame(&controlapi.StatusResponse{Logs: []*controlapi.VertexLog{{Vertex: d, Stream: 1, Msg: msg}}}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *loud) Open(ctx context.Context, d *controlapi.Descriptor) (io.ReadCloser, error) {
	r, w := io.Pipe()
	go func() { w.CloseWithError(l.progress(w)) }()
	return r, nil
}

// A build whose steps printed far more than a step's output can show is
// read in memory bounded by what is kept, not by what was printed: the
// controller would otherwise run out of memory on it, and again on every
// start, as the daemon sends the record again.
func TestLoudBuildIsReadInBoundedMemory(t *testing.T) {
	src := &loud{steps: 100, frames: 128, frameBytes: 16 << 10, t0: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)} // 200 MiB
	c := &Collector{}
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var (
		peak uint64
		wg   sync.WaitGroup
	)
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			peak = max(peak, m.HeapAlloc)
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	b, err := c.read(ctx, src, src.record())
	close(done)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	grown := int64(peak) - int64(base.HeapAlloc)
	if grown > 128<<20 {
		t.Errorf("reading a 200 MiB log took %d MiB", grown>>20)
	}
	t.Logf("reading a 200 MiB log took %d MiB", grown>>20)
	if len(b.Steps) != src.steps {
		t.Fatalf("%d steps, want %d", len(b.Steps), src.steps)
	}
	for i, s := range b.Steps {
		if s.State != history.Built || len(s.Output) > history.MaxStepOutput+64 ||
			!strings.HasSuffix(s.Output, fmt.Sprintf("END %d\n", i)) || !strings.Contains(s.Output, "bytes cut") {
			t.Fatalf("%s: %s, %d bytes of output ending %q", s.Key, s.State, len(s.Output), s.Output[max(0, len(s.Output)-20):])
		}
	}
}
