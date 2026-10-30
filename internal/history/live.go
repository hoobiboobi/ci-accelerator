package history

import (
	"bytes"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/moby/buildkit/client"
)

// TraceOpt is the build option that names a running build's CI job: the
// job's TRACEPARENT, which the CI component passes to buildctl
// (--opt ci-accelerator.io/traceparent=$TRACEPARENT). BuildKit hands a
// build's options out the moment it starts, long before the build's trace
// (which names the job of a completed build) exists. The Dockerfile
// frontend ignores options it does not know: neither the build nor its
// cache keys see it.
const TraceOpt = "ci-accelerator.io/traceparent"

// TraceParent attributes a running build by the TRACEPARENT in its
// options, as Attribute does a completed one by its trace's ids.
func TraceParent(opts map[string]string, instances map[string]bool) Attribution {
	parts := strings.Split(strings.TrimSpace(opts[TraceOpt]), "-")
	if len(parts) != 4 || len(parts[1]) != 32 {
		return Attribution{State: Unattributed}
	}
	return Attribute([]string{strings.ToLower(parts[1])}, instances)
}

// TailLines is how many of its last lines a running step shows.
const TailLines = 8

// maxTailLine bounds one line of a tail; a longer one keeps its start.
const maxTailLine = 400

// A private key's first and last lines, when a step prints one line by
// line.
var (
	keyBeginLine = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	keyEndLine   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
)

// The states of a running build's steps, beyond those of a completed one.
const (
	LiveWaiting  = "waiting"
	LiveRunning  = "running"
	LiveBuilt    = "built"
	LiveCached   = "cached"
	LiveFailed   = "failed"
	LiveCanceled = "canceled"
)

// LiveStep is one step of a build that is still running.
type LiveStep struct {
	Key   string // "[api 2/3]"
	Name  string // "COPY api/ ./api", secret build arguments masked
	State string
	// Started and Completed are zero until the step starts and ends.
	Started, Completed time.Time
	Error              string
	// Lines is how many lines the step printed so far; Tail the last
	// TailLines of them, secrets masked.
	Lines int
	Tail  []TailLine
}

// TailLine is one line a step printed, At after the step started.
type TailLine struct {
	At   time.Duration
	Text string
}

// Live follows a running build's progress, as BuildKit streams it, and
// keeps only what its page shows: each step's state and times and the
// last lines it printed.
type Live struct {
	args     map[string]string
	vertices map[string]*liveVertex
	order    []string
	tails    map[string]*tail // by step key
	// ContextBytes is the build context sent so far.
	ContextBytes int64
}

type liveVertex struct {
	key, name          string // key "" for vertices that are not steps
	started, completed time.Time
	cached             bool
	err                string
}

// NewLive follows a build with these build arguments (their secret values
// are masked).
func NewLive(args map[string]string) *Live {
	return &Live{args: args, vertices: map[string]*liveVertex{}, tails: map[string]*tail{}}
}

// Add takes one progress update.
func (l *Live) Add(st *client.SolveStatus) {
	for _, v := range st.Vertexes {
		d := v.Digest.String()
		x := l.vertices[d]
		if x == nil {
			x = &liveVertex{}
			if m := stepKey.FindStringSubmatch(v.Name); m != nil && !strings.HasPrefix(v.Name, "[internal]") {
				x.key, x.name = m[1], m[2]
			}
			l.vertices[d] = x
			l.order = append(l.order, d)
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
	for _, s := range st.Statuses {
		if strings.HasPrefix(s.ID, "transferring context") && s.Current > l.ContextBytes {
			l.ContextBytes = s.Current
		}
	}
	for _, lg := range st.Logs {
		x := l.vertices[lg.Vertex.String()]
		if x == nil || x.key == "" {
			continue
		}
		t := l.tails[x.key]
		if t == nil {
			t = &tail{}
			l.tails[x.key] = t
		}
		t.write(lg.Data, lg.Timestamp, l.args)
	}
}

// Steps are the build's steps so far, in the order BuildKit named them.
func (l *Live) Steps() []LiveStep {
	byKey := map[string]*LiveStep{}
	running := map[string]bool{}
	var keys []string
	for _, d := range l.order {
		v := l.vertices[d]
		if v.key == "" {
			continue
		}
		s := byKey[v.key]
		if s == nil {
			s = &LiveStep{Key: v.key, Name: mask(v.name, l.args)}
			byKey[v.key] = s
			keys = append(keys, v.key)
		}
		if !v.started.IsZero() && (s.Started.IsZero() || v.started.Before(s.Started)) {
			s.Started = v.started
		}
		if v.completed.After(s.Completed) {
			s.Completed = v.completed
		}
		if !v.started.IsZero() && v.completed.IsZero() {
			running[v.key] = true
		}
		if v.err != "" {
			s.Error = mask(v.err, l.args)
		}
		switch {
		case s.State == "":
			s.State = LiveCached
			if !v.cached {
				s.State = LiveBuilt
			}
		case !v.cached:
			s.State = LiveBuilt
		}
	}
	out := make([]LiveStep, 0, len(keys))
	for _, k := range keys {
		s := byKey[k]
		switch {
		case s.Error != "" && Canceled(s.Error):
			s.State = LiveCanceled
		case s.Error != "":
			s.State = LiveFailed
		case running[k]:
			s.State, s.Completed = LiveRunning, time.Time{}
		case s.Started.IsZero():
			s.State = LiveWaiting
		}
		if t := l.tails[k]; t != nil {
			s.Lines, s.Tail = t.lines, t.snapshot(s.Started, l.args)
		}
		out = append(out, *s)
	}
	// Steps that started come first, by start; the rest keep BuildKit's
	// order (the order of the Dockerfile's stages).
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Started, out[j].Started
		switch {
		case a.IsZero() || b.IsZero():
			return !a.IsZero() && b.IsZero()
		default:
			return a.Before(b)
		}
	})
	return out
}

// tail keeps the last lines a step printed.
type tail struct {
	lines   int
	last    []tailEntry // ring of the last TailLines lines
	partial []byte      // the line being printed
	partAt  time.Time
	inKey   bool // inside a private key block
	cut     bool // the drawing being printed outgrew its bound
}

// add appends to the line being printed. A progress bar redraws its line
// with \r: only its last whole drawing and the one being drawn are kept,
// so a bar that redraws for minutes keeps showing its latest state. Each
// drawing is kept to a bound: what a tail shows of a line that never ends
// is its start, and a drawing that outgrew the bound shows, once whole,
// as an ellipsis rather than as a piece that may hold part of a secret.
func (t *tail) add(b []byte) {
	for {
		i := bytes.IndexByte(b, '\r')
		seg := b
		if i >= 0 {
			seg = b[:i]
		}
		start := bytes.LastIndexByte(t.partial, '\r') + 1
		if room := 4*maxTailLine - (len(t.partial) - start); len(seg) > room {
			seg, t.cut = seg[:max(room, 0)], true
		}
		t.partial = append(t.partial, seg...)
		if i < 0 {
			return
		}
		cur := append([]byte(nil), t.partial[start:]...)
		if t.cut {
			cur = []byte("…")
		}
		t.partial, t.cut = append(append(t.partial[:0], cur...), '\r'), false
		b = b[i+1:]
	}
}

type tailEntry struct {
	at   time.Time
	text string
}

func (t *tail) write(p []byte, at time.Time, args map[string]string) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if len(t.partial) == 0 {
				t.partAt = at
			}
			t.add(p)
			return
		}
		if len(t.partial) == 0 {
			t.partAt = at
		}
		t.add(p[:i])
		t.line(string(t.partial), t.partAt, args)
		t.partial, t.cut = t.partial[:0], false
		p = p[i+1:]
	}
}

// line keeps one whole line, masked: a private key printed line by line
// is masked from its BEGIN line to its END line, even when the tail shows
// only its middle.
func (t *tail) line(s string, at time.Time, args map[string]string) {
	t.lines++
	// A progress bar redraws its line: what shows is the last drawing.
	if i := strings.LastIndexByte(strings.TrimRight(s, "\r"), '\r'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimRight(s, "\r")
	switch {
	case t.inKey:
		if keyEndLine.MatchString(s) {
			t.inKey = false
		}
		s = Masked
	case keyBeginLine.MatchString(s) && !keyEndLine.MatchString(s):
		t.inKey = true
		s = Masked
	default:
		s = MaskSecrets(mask(s, args))
	}
	if len(s) > maxTailLine {
		s = strings.ToValidUTF8(s[:maxTailLine], "") + "…"
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	t.last = append(t.last, tailEntry{at, s})
	if len(t.last) > TailLines {
		t.last = append(t.last[:0], t.last[len(t.last)-TailLines:]...)
	}
}

func (t *tail) snapshot(started time.Time, args map[string]string) []TailLine {
	last := t.last
	// A line still being drawn shows only once it redrew itself (a
	// progress bar): its last whole drawing. Text that is still being
	// printed may be the start of a secret masking does not recognise yet.
	if i := bytes.LastIndexByte(t.partial, '\r'); i > 0 && !t.inKey {
		draw := string(t.partial[:i])
		if j := strings.LastIndexByte(draw, '\r'); j >= 0 {
			draw = draw[j+1:]
		}
		if draw = MaskSecrets(mask(draw, args)); len(draw) > maxTailLine {
			draw = strings.ToValidUTF8(draw[:maxTailLine], "") + "…"
		}
		last = append(append([]tailEntry(nil), last...), tailEntry{t.partAt, strings.ToValidUTF8(draw, "�")})
		if len(last) > TailLines {
			last = last[len(last)-TailLines:]
		}
	}
	out := make([]TailLine, 0, len(last))
	for _, e := range last {
		var at time.Duration
		if !started.IsZero() && e.at.After(started) {
			at = e.at.Sub(started)
		}
		out = append(out, TailLine{At: at, Text: e.text})
	}
	return out
}
