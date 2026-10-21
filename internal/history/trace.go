package history

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The runner's pre_build_script gives every CI job its own trace (scheme 01):
// trace-id = 01 | 3 bytes of sha256(GitLab URL) | pipeline id (6 bytes) |
// job id (6 bytes); see docs/adr/0028-one-trace-per-job.md.
const schemeV1 = "01"

// Attribution says which CI job a build belongs to.
type Attribution struct {
	State    AttributionState
	TraceID  string
	Instance string // prefix of the GitLab instance (6 hex characters)
	Pipeline uint64
	Job      uint64
}

// AttributionState tells how far a build could be attributed.
type AttributionState string

const (
	// Attributed: the trace id follows scheme 01 for a known GitLab.
	Attributed AttributionState = "attributed"
	// Unattributed: no trace, or a trace id of another origin (a job that
	// set its own TRACEPARENT, a client outside the runner).
	Unattributed AttributionState = "unattributed"
	// Ambiguous: the record's spans belong to several trace ids. Never
	// resolved by guessing.
	Ambiguous AttributionState = "ambiguous"
)

// InstancePrefix is the trace-id prefix the runner derives from a GitLab
// URL, exactly as configured (gitlabUrl).
func InstancePrefix(gitlabURL string) string {
	sum := sha256.Sum256([]byte(gitlabURL))
	return hex.EncodeToString(sum[:])[:6]
}

// TraceIDs reads the distinct trace ids of a record's trace blob: a stream
// of JSON span stubs.
func TraceIDs(r io.Reader) ([]string, error) {
	dec := json.NewDecoder(r)
	seen := map[string]bool{}
	var out []string
	for {
		var span struct {
			SpanContext struct {
				TraceID string `json:"TraceID"`
			} `json:"SpanContext"`
		}
		if err := dec.Decode(&span); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("trace: %w", err)
		}
		if id := strings.ToLower(span.SpanContext.TraceID); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
}

// Attribute maps a record's trace ids to a CI job. Only scheme 01 ids of a
// known GitLab instance count: a random trace id starting with "01" matches
// a known 3-byte prefix with odds of 1 in 2^24.
func Attribute(traceIDs []string, instances map[string]bool) Attribution {
	switch len(traceIDs) {
	case 0:
		return Attribution{State: Unattributed}
	case 1:
	default:
		return Attribution{State: Ambiguous, TraceID: strings.Join(traceIDs, ",")}
	}
	id := traceIDs[0]
	a := Attribution{State: Unattributed, TraceID: id}
	if len(id) != 32 || !strings.HasPrefix(id, schemeV1) || !instances[id[2:8]] {
		return a
	}
	pipeline, err1 := strconv.ParseUint(id[8:20], 16, 64)
	job, err2 := strconv.ParseUint(id[20:32], 16, 64)
	if err1 != nil || err2 != nil || pipeline == 0 || job == 0 {
		return a
	}
	return Attribution{State: Attributed, TraceID: id, Instance: id[2:8], Pipeline: pipeline, Job: job}
}

// AttributeBlob is TraceIDs followed by Attribute; an empty blob is an
// unattributed build.
func AttributeBlob(trace []byte, instances map[string]bool) (Attribution, error) {
	if len(bytes.TrimSpace(trace)) == 0 {
		return Attribution{State: Unattributed}, nil
	}
	ids, err := TraceIDs(bytes.NewReader(trace))
	if err != nil {
		return Attribution{}, err
	}
	return Attribute(ids, instances), nil
}
