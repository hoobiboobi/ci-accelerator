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
	"strings"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/solver/pb"
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

// The bounds of what DecodeProgress keeps of a blob besides its logs.
// BuildKit bounds neither a build's vertices nor its warnings, and a
// build of raw LLB or a frontend of the job's choosing can send hundreds
// of thousands: the console keeps what its pages can show.
const (
	// maxProgressVertices and maxProgressNames bound the vertices kept and
	// the bytes of their names; a vertex beyond either is dropped, and the
	// build reads as truncated.
	maxProgressVertices = 50000
	maxProgressNames    = 16 << 20
	// maxRawError bounds a vertex's error as kept (a step shows less).
	maxRawError = 64 << 10
	// maxProgressWarnings bounds the warnings kept, maxWarningText each of
	// their texts.
	maxProgressWarnings = 1000
	maxWarningText      = 4 << 10
	maxWarningParts     = 16
)

// droppedVertices is a vertex DecodeProgress adds when it dropped some:
// the build's steps are then not all its steps.
const droppedVertices = digest.Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000")

// DecodeProgress reads a progress blob: StatusResponse messages, each
// preceded by its length as a little-endian uint32 (BuildKit's
// history.ImportStatus). It returns what NewBuild reads, bounded: each
// vertex once, its updates merged (first name, cached by any, earliest
// start, latest end, last error), in the order they first appeared; the
// largest count of the build context sent; the warnings; and of each
// vertex's log what a step's output can show. The logs come last, in a
// status of their own.
func DecodeProgress(r io.Reader) ([]*client.SolveStatus, error) {
	br := bufio.NewReader(r)
	vertices := map[digest.Digest]*client.Vertex{}
	var kept client.SolveStatus
	var names int
	dropped := false
	var sent *client.VertexStatus
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
		for _, v := range st.Vertexes {
			x := vertices[v.Digest]
			if x == nil {
				if v.Digest == droppedVertices || len(kept.Vertexes) >= maxProgressVertices || names+len(v.Name) > maxProgressNames {
					dropped = true
					continue
				}
				names += len(v.Name)
				x = &client.Vertex{Digest: v.Digest, Name: v.Name}
				vertices[v.Digest] = x
				kept.Vertexes = append(kept.Vertexes, x)
			}
			x.Cached = x.Cached || v.Cached
			if v.Started != nil && (x.Started == nil || v.Started.Before(*x.Started)) {
				x.Started = v.Started
			}
			if v.Completed != nil && (x.Completed == nil || v.Completed.After(*x.Completed)) {
				x.Completed = v.Completed
			}
			if v.Error != "" {
				x.Error = cutRaw(v.Error, maxRawError)
			}
		}
		for _, s := range st.Statuses {
			if strings.HasPrefix(s.ID, "transferring context") && (sent == nil || s.Current > sent.Current) {
				sent = &client.VertexStatus{ID: s.ID, Current: s.Current}
			}
		}
		for _, w := range st.Warnings {
			if len(kept.Warnings) < maxProgressWarnings {
				kept.Warnings = append(kept.Warnings, boundWarning(w))
			}
		}
		for _, l := range st.Logs {
			lb := logs[l.Vertex]
			if lb == nil {
				lb = &logBuffer{budget: &budget}
				logs[l.Vertex] = lb
				order = append(order, l.Vertex)
			}
			lb.Write(l.Data)
		}
	}
	if sent != nil {
		kept.Statuses = []*client.VertexStatus{sent}
	}
	if dropped {
		kept.Vertexes = append(kept.Vertexes, &client.Vertex{Digest: droppedVertices})
	}
	var out []*client.SolveStatus
	if len(kept.Vertexes) > 0 || len(kept.Statuses) > 0 || len(kept.Warnings) > 0 {
		out = append(out, &kept)
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

// boundWarning keeps of a warning what warnings reads, each text bounded:
// of its source, the file name only (BuildKit sends the whole Dockerfile
// with every warning).
func boundWarning(w *client.VertexWarning) *client.VertexWarning {
	x := &client.VertexWarning{Vertex: w.Vertex, Level: w.Level, Short: []byte(cutRaw(string(w.Short), maxWarningText)), URL: cutRaw(w.URL, maxWarningText)}
	for _, d := range w.Detail[:min(len(w.Detail), maxWarningParts)] {
		x.Detail = append(x.Detail, []byte(cutRaw(string(d), maxWarningText)))
	}
	if w.SourceInfo != nil {
		x.SourceInfo = &pb.SourceInfo{Filename: cutRaw(w.SourceInfo.Filename, maxWarningText)}
	}
	x.Range = w.Range[:min(len(w.Range), maxWarningParts)]
	return x
}

// cutRaw bounds a text that is masked later: a text longer than n is cut
// at its last line break or space before n, so that no part of a secret
// cut at n is left for masking to miss.
func cutRaw(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:lastBreak(s[:n])]
}
