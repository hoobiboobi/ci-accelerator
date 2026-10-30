package history

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
)

// StepState is what happened to a Dockerfile step in one build.
type StepState string

const (
	// Cached: the result came from the daemon's cache.
	Cached StepState = "cached"
	// Built: this build executed the step.
	Built StepState = "built"
	// Received: another build executed the same step at the same time on
	// this daemon; this build waited and used its result.
	Received StepState = "received"
)

// Step is one Dockerfile step of a build.
type Step struct {
	Key       string // "[api 2/3]"
	Name      string // "COPY api/ ./api"
	StepID    string // provenance step id; empty without provenance
	Line      int    // first Dockerfile line, 0 if unknown
	Lines     []LineRange
	Identity  string // stable across builds; empty without provenance
	State     StepState
	Started   time.Time
	Completed time.Time
	Error     string
	// Usage is what the step's processes used when this build executed
	// it; zero for a step from the cache.
	Usage Usage
	// What the step printed, when this build executed it: secrets masked,
	// clipped to MaxStepOutput. A cached or received step prints nothing
	// in this build.
	Output string
	// For Received: the other build's vertex that did the work, and once
	// Link has run, that build's ref.
	ReceivedVertex string
	ReceivedFrom   string

	vertex         string // this build's own vertex for the step
	foreignStarted time.Time
	foreignCached  bool // another build's vertex for it came from cache
}

// Dockerfile returns the Dockerfile the build used and its file name, as
// recorded in the provenance; empty when the build has none. Secrets in
// it are masked, as in the steps' names, keeping its lines.
func (b *Build) Dockerfile() (name, text string) {
	switch {
	case b.prov != nil:
		name, text = b.prov.DockerfileName, b.prov.Dockerfile
	case b.errSrc != nil:
		name, text = b.errSrc.Name, b.errSrc.Text
	}
	return name, maskText(text, b.BuildArgs, true)
}

// Duration is how long the step took in this build; for a received step,
// how long this build waited for it.
func (s Step) Duration() time.Duration {
	if s.Started.IsZero() || s.Completed.IsZero() {
		return 0
	}
	return s.Completed.Sub(s.Started)
}

// Build is one build-history record, attributed and reduced to its steps.
type Build struct {
	Ref string
	// Daemon is the build-history source, set by whoever read the record
	// (the collector reads each daemon apart). The cache is per daemon.
	Daemon       string
	Attribution  Attribution
	Target       string
	BuildArgs    map[string]string
	Started      time.Time
	Completed    time.Time
	Error        string
	Steps        []Step
	ContextBytes int64
	Images       []string // pushed or exported image names
	// Warnings are the Dockerfile build checks that failed.
	Warnings []Warning
	// Vertices this build executed or took from cache itself (its own
	// digestMapping keys).
	Vertices map[string]bool

	prov *Provenance
	// errSrc is the Dockerfile and failed step of a failed build (which has
	// no provenance), from its error.
	errSrc *ErrorSource
	// When each of this build's own vertices started, as the daemon
	// reported it: the same vertex with the same start in two builds is one
	// execution.
	vertexStart map[string]time.Time
}

// stepKey matches a Dockerfile step's vertex name: "[api 2/3] COPY ...", or
// "[2/3] COPY ..." in a Dockerfile whose only stage has no name.
var stepKey = regexp.MustCompile(`^(\[(?:[^\]]+ )?\d+/\d+\]) (.*)$`)

// ParseRecord reads a record as BuildKit's controlapi type, from the JSON
// buildctl prints with `debug histories --format '{{json .Record}}'`.
func ParseRecord(data []byte) (*controlapi.BuildHistoryRecord, error) {
	var r controlapi.BuildHistoryRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("record: %w", err)
	}
	return &r, nil
}

// Config is what reading records needs to know about the installation.
type Config struct {
	// Instances are the trace-id prefixes of the GitLab instances served.
	Instances map[string]bool
	// SecretKey keys the hashes that replace secret values in stored
	// operations. It must stay the same for builds to remain comparable.
	SecretKey []byte
}

// NewBuild assembles a build from its record and blobs. provenance holds
// one attestation per result: a build for several platforms has one per
// platform, each covering only that platform's steps. provenance and trace
// may be empty: a failed build has no provenance, and a trace is attached a
// few seconds after a build or never.
//
// errBlob is the record's ExternalError blob (empty for a build that did
// not fail); what it says about the Dockerfile is used only when there is
// no provenance, and a blob that does not parse is ignored.
func NewBuild(rec *controlapi.BuildHistoryRecord, progress []*client.SolveStatus, provenance [][]byte, trace, errBlob []byte, cfg Config) (*Build, error) {
	b := &Build{
		Ref:       rec.Ref,
		Target:    rec.FrontendAttrs["target"],
		BuildArgs: map[string]string{},
		Vertices:  map[string]bool{},

		vertexStart: map[string]time.Time{},
	}
	for k, v := range rec.FrontendAttrs {
		if name, ok := strings.CutPrefix(k, "build-arg:"); ok {
			b.BuildArgs[name] = v
		}
	}
	if rec.CreatedAt != nil {
		b.Started = rec.CreatedAt.AsTime()
	}
	if rec.CompletedAt != nil {
		b.Completed = rec.CompletedAt.AsTime()
	}
	if rec.Error != nil {
		b.Error = mask(rec.Error.Message, b.BuildArgs)
	}
	for _, e := range rec.Exporters {
		if n := e.Attrs["name"]; n != "" {
			b.Images = append(b.Images, n)
		}
	}
	var provs []*Provenance
	for _, blob := range provenance {
		if len(bytes.TrimSpace(blob)) == 0 {
			continue
		}
		p, err := ParseProvenance(blob, cfg.SecretKey)
		if err != nil {
			return nil, err
		}
		provs = append(provs, p)
	}
	if len(provs) > 0 {
		b.prov = mergeProvenance(provs)
		for d := range b.prov.DigestMapping {
			b.Vertices[d] = true
		}
	}
	if b.prov == nil && rec.Error != nil {
		if es, err := ParseErrorSource(errBlob); err == nil {
			b.errSrc = es
		}
	}
	att, err := AttributeBlob(trace, cfg.Instances)
	if err != nil {
		return nil, err
	}
	b.Attribution = att
	b.Steps, b.ContextBytes = b.steps(progress)
	for i := range b.Steps {
		// The work of a step another build ran (or the cache held) is not
		// this build's: its usage belongs to whoever executed it.
		if b.Steps[i].State != Built {
			b.Steps[i].Usage = Usage{}
		}
	}
	b.Warnings = warnings(progress)
	return b, nil
}

type vertex struct {
	digest    string
	name      string
	cached    bool
	started   time.Time
	completed time.Time
	err       string
}

// steps merges the progress updates per vertex and groups the vertices by
// Dockerfile step. A step with a vertex of another build is Received; the
// build's own vertex for it then only shows the wait.
func (b *Build) steps(progress []*client.SolveStatus) ([]Step, int64) {
	vs := map[string]*vertex{}
	logs := map[string]*logBuffer{}
	var order []string
	var ctxBytes int64
	for _, st := range progress {
		for _, v := range st.Vertexes {
			d := v.Digest.String()
			x, ok := vs[d]
			if !ok {
				x = &vertex{digest: d, name: v.Name}
				vs[d] = x
				order = append(order, d)
			}
			x.cached = x.cached || v.Cached
			if v.Started != nil && (x.started.IsZero() || v.Started.Before(x.started)) {
				x.started = *v.Started
			}
			if v.Completed != nil && v.Completed.After(x.completed) {
				x.completed = *v.Completed
			}
			if v.Error != "" {
				x.err = v.Error
			}
		}
		for _, l := range st.Logs {
			d := l.Vertex.String()
			if logs[d] == nil {
				logs[d] = &logBuffer{}
			}
			if l.Stream == cutStream {
				n, _ := strconv.ParseInt(string(l.Data), 10, 64)
				logs[d].cut += n
				continue
			}
			logs[d].Write(l.Data)
		}
		for _, s := range st.Statuses {
			if strings.HasPrefix(s.ID, "transferring context") && s.Current > ctxBytes {
				ctxBytes = s.Current
			}
		}
	}
	steps := map[string]*Step{}
	var keys []string
	for _, d := range order {
		v := vs[d]
		m := stepKey.FindStringSubmatch(v.name)
		if m == nil || strings.HasPrefix(v.name, "[internal]") {
			continue
		}
		s, ok := steps[m[1]]
		if !ok {
			// BuildKit expands build arguments in a step's name.
			s = &Step{Key: m[1], Name: mask(m[2], b.BuildArgs)}
			steps[m[1]] = s
			keys = append(keys, m[1])
		}
		own := b.prov == nil || b.Vertices[d]
		if !own && v.cached {
			// Another build's vertex that came from cache: both took the
			// same cached result, nobody executed anything.
			s.foreignCached = true
			if s.State == "" || s.State == Built {
				s.State = Cached
			}
			continue
		}
		if !own {
			// Another build's vertex: it did the work. The wait runs from
			// this build reaching the step to that vertex completing.
			s.State = Received
			s.ReceivedVertex = d
			s.foreignStarted = v.started
			s.Completed = v.completed
			continue
		}
		s.vertex = d
		if !v.started.IsZero() {
			b.vertexStart[d] = v.started
		}
		if b.prov != nil {
			s.StepID = b.prov.DigestMapping[d]
			s.Identity = b.prov.Identity[s.StepID]
			if u, ok := b.prov.Usage[s.StepID]; ok {
				s.Usage.CPU += u.CPU
				s.Usage.Throttled += u.Throttled
				s.Usage.ReadBytes += u.ReadBytes
				s.Usage.WriteBytes += u.WriteBytes
				s.Usage.Memory = max(s.Usage.Memory, u.Memory)
			}
			s.Line = b.prov.Lines[s.StepID]
			s.Lines = b.prov.Ranges[s.StepID]
		}
		if b.prov == nil && b.errSrc != nil && b.errSrc.Vertex == d && len(b.errSrc.Ranges) > 0 {
			s.Lines = b.errSrc.Ranges
			s.Line = s.Lines[0].Start
		}
		if v.err != "" {
			s.Error = mask(v.err, b.BuildArgs)
		}
		if s.State == Received {
			continue
		}
		// A step that does not depend on the build context has the same
		// vertex in every build, so it is in this build's own mapping even
		// when another build executed it. One that started before this
		// build existed was executed by an earlier one (both times come from
		// the daemon's clock).
		// Image sources are resolved in every build and keep the vertex of
		// the first resolution: they are not work received from anyone.
		if !v.cached && !v.started.IsZero() && !b.Started.IsZero() && v.started.Before(b.Started) &&
			(b.prov == nil || b.prov.imageSource(s.StepID) == "") {
			s.State = Received
			s.ReceivedVertex = d
			s.foreignStarted = v.started
			s.Completed = v.completed
			continue
		}
		s.Started = v.started
		s.Completed = v.completed
		if v.cached || s.foreignCached {
			s.State = Cached
		} else if !v.started.IsZero() {
			s.State = Built
			if l := logs[d]; l != nil {
				s.Output = l.output(b.BuildArgs)
			}
		}
	}
	out := make([]Step, 0, len(keys))
	for _, k := range keys {
		if s := steps[k]; s.State != "" {
			out = append(out, *s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return lessStep(out[i], out[j]) })
	waits(b.Started, out)
	return out, ctxBytes
}

// waits sets the start of each received step to when this build reached
// it: after its previous step, and not before the other build started it.
func waits(start time.Time, steps []Step) {
	reached := start
	for i := range steps {
		s := &steps[i]
		if s.State == Received {
			s.Started = s.foreignStarted
			if reached.After(s.Started) {
				s.Started = reached
			}
			if s.Completed.Before(s.Started) {
				s.Completed = s.Started
			}
		}
		if s.Completed.After(reached) {
			reached = s.Completed
		}
	}
}

// lessStep orders steps by Dockerfile line when both are known (with
// provenance), otherwise by start time, then by their "[stage n/m]" key.
func lessStep(a, b Step) bool {
	if a.Line != 0 && b.Line != 0 && a.Line != b.Line {
		return a.Line < b.Line
	}
	if !a.Started.Equal(b.Started) && !a.Started.IsZero() && !b.Started.IsZero() {
		return a.Started.Before(b.Started)
	}
	return a.Key < b.Key
}

// Canceled reports whether a build's or step's error text says the job was
// canceled rather than failed (BuildKit reports the client's cancellation
// as a context error or a gRPC Canceled status).
func Canceled(err string) bool {
	return strings.HasSuffix(err, "context canceled") || strings.Contains(err, "code = Canceled")
}

// ExecutedTime is the wall time during which the build executed steps
// (resolving base images aside): the union of their intervals, so steps
// of parallel stages count once.
func (b *Build) ExecutedTime() time.Duration {
	type span struct{ from, to time.Time }
	var ss []span
	for _, s := range b.Steps {
		if s.State == Built && !strings.HasPrefix(s.Name, "FROM ") && !s.Started.IsZero() && s.Completed.After(s.Started) {
			ss = append(ss, span{s.Started, s.Completed})
		}
	}
	sort.Slice(ss, func(i, j int) bool { return ss[i].from.Before(ss[j].from) })
	var total time.Duration
	var cur span
	for i, s := range ss {
		switch {
		case i == 0:
			cur = s
		case !s.from.After(cur.to):
			if s.to.After(cur.to) {
				cur.to = s.to
			}
		default:
			total += cur.to.Sub(cur.from)
			cur = s
		}
	}
	if len(ss) > 0 {
		total += cur.to.Sub(cur.from)
	}
	return total
}
