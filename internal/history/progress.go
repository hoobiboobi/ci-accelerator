// Package history turns BuildKit build-history records into what the console
// shows: builds attributed to CI jobs, their steps with a state each (cached,
// built, received from another build), and the reason a step was rebuilt.
//
// A record comes with three blobs, read from the daemon's content store:
// the progress stream (application/vnd.buildkit.status.v0), the SLSA
// provenance (always mode=max) and the trace. Everything here is a pure
// function of those inputs.
package history

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
	digest "github.com/opencontainers/go-digest"
)

// maxFrame bounds one progress message; a larger length means a corrupt blob.
const maxFrame = 64 << 20

// maxKeptLogs bounds the logs kept of one progress blob. BuildKit bounds
// neither a build's steps nor its history's log blob, so a build can print
// far more than the console can hold in memory. Each log keeps at most its
// head and tail; once the budget is spent, a log keeps what of its head
// fits, and the length of the rest.
const maxKeptLogs = 64 << 20

// cutStream marks, in the logs DecodeProgress returns, the number of bytes
// it dropped from a vertex's log (as decimal text), between its head and
// its tail. BuildKit's streams are 1 (stdout) and 2 (stderr).
const cutStream = -1

// DecodeProgress reads a progress blob: StatusResponse messages, each
// preceded by its length as a little-endian uint32 (BuildKit's
// history.ImportStatus). Of each vertex's log it keeps what a step's
// output can show: the logs come last, in a status of their own.
func DecodeProgress(r io.Reader) ([]*client.SolveStatus, error) {
	br := bufio.NewReader(r)
	var out []*client.SolveStatus
	logs := map[digest.Digest]*logBuffer{}
	var order []digest.Digest
	budget := int64(maxKeptLogs)
	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("progress: frame header: %w", err)
		}
		n := binary.LittleEndian.Uint32(hdr)
		if n > maxFrame {
			return nil, fmt.Errorf("progress: frame of %d bytes", n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, fmt.Errorf("progress: frame body: %w", err)
		}
		var msg controlapi.StatusResponse
		if err := msg.UnmarshalVT(buf); err != nil {
			return nil, fmt.Errorf("progress: frame: %w", err)
		}
		st := client.NewSolveStatus(&msg)
		for _, l := range st.Logs {
			lb := logs[l.Vertex]
			if lb == nil {
				lb = &logBuffer{budget: &budget}
				logs[l.Vertex] = lb
				order = append(order, l.Vertex)
			}
			lb.Write(l.Data)
		}
		st.Logs = nil
		out = append(out, st)
	}
	if len(order) == 0 {
		return out, nil
	}
	last := &client.SolveStatus{}
	for _, d := range order {
		lb := logs[d]
		lb.trim()
		if len(lb.head) > 0 {
			last.Logs = append(last.Logs, &client.VertexLog{Vertex: d, Stream: 1, Data: lb.head})
		}
		if lb.cut > 0 {
			last.Logs = append(last.Logs, &client.VertexLog{Vertex: d, Stream: cutStream, Data: []byte(strconv.FormatInt(lb.cut, 10))})
		}
		if len(lb.tail) > 0 {
			last.Logs = append(last.Logs, &client.VertexLog{Vertex: d, Stream: 1, Data: lb.tail})
		}
	}
	return append(out, last), nil
}
