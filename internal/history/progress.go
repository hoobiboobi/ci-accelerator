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

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
)

// maxFrame bounds one progress message; a larger length means a corrupt blob.
const maxFrame = 64 << 20

// DecodeProgress reads a progress blob: StatusResponse messages, each
// preceded by its length as a little-endian uint32 (BuildKit's
// history.ImportStatus).
func DecodeProgress(r io.Reader) ([]*client.SolveStatus, error) {
	br := bufio.NewReader(r)
	var out []*client.SolveStatus
	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
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
		out = append(out, client.NewSolveStatus(&msg))
	}
}
