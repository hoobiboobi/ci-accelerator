package history

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"

	"github.com/moby/buildkit/solver/errdefs"
	"github.com/moby/buildkit/util/grpcerrors"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ErrorSource is where a failed build stopped in its Dockerfile. BuildKit
// records no provenance for a failed build; the error it keeps with the
// record (the ExternalError blob, a google.rpc.Status with its details)
// carries the Dockerfile's text and the failed instruction's lines, the
// same details buildctl prints under a failed build.
type ErrorSource struct {
	Name, Text string
	// Vertex is the failed step's vertex; empty when the Dockerfile did
	// not parse (no step ran).
	Vertex string
	Ranges []LineRange
}

// ParseErrorSource reads a record's ExternalError blob. It returns nil
// when the error names no Dockerfile (a canceled build, an error outside
// the Dockerfile).
func ParseErrorSource(data []byte) (*ErrorSource, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var st spb.Status
	if err := proto.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("error blob: %w", err)
	}
	err := grpcerrors.FromGRPC(status.FromProto(&st).Err())
	srcs := errdefs.Sources(err)
	pick := -1
	for i, s := range srcs {
		if s.Info == nil {
			continue
		}
		if pick < 0 || (s.Info.Language == "Dockerfile" && srcs[pick].Info.Language != "Dockerfile") {
			pick = i
		}
	}
	if pick < 0 {
		return nil, nil
	}
	info := srcs[pick].Info
	es := &ErrorSource{Name: info.Filename, Text: string(info.Data)}
	for _, s := range srcs {
		if s.Info == nil || s.Info.Filename != info.Filename || string(s.Info.Data) != es.Text {
			continue
		}
		for _, r := range s.Ranges {
			if r == nil || r.Start == nil {
				continue
			}
			lr := LineRange{Start: int(r.Start.Line), End: int(r.Start.Line)}
			if r.End != nil && int(r.End.Line) > lr.End {
				lr.End = int(r.End.Line)
			}
			es.Ranges = append(es.Ranges, lr)
		}
	}
	es.Ranges = mergeRanges(es.Ranges)
	var ve *errdefs.VertexError
	if errors.As(err, &ve) {
		es.Vertex = ve.Digest
	}
	return es, nil
}

// mergeRanges sorts ranges and joins the ones that touch: BuildKit reports
// a RUN continued over two lines as two one-line ranges in an error and as
// one range in provenance.
func mergeRanges(rs []LineRange) []LineRange {
	if len(rs) < 2 {
		return rs
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Start < rs[j].Start })
	out := rs[:1]
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		if r.Start <= last.End+1 {
			last.End = max(last.End, r.End)
			continue
		}
		out = append(out, r)
	}
	return out
}

// parseError is how BuildKit's Dockerfile frontend words an instruction
// it cannot parse (frontend/dockerfile/instructions, v0.33-v0.34): the
// line the instruction starts on, and why.
var parseError = regexp.MustCompile(`^dockerfile parse error on line ([0-9]{1,7}): `)

// ParseErrorLine is the Dockerfile line a build's error says BuildKit
// could not parse; 0 when the error is not a parse error. A stored build
// keeps its error, not the error blob's ranges, and a parse error stops a
// build before any step, so this line is what marks the Dockerfile.
func ParseErrorLine(buildErr string) int {
	m := parseError.FindStringSubmatch(buildErr)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
