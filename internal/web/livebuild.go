package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// ---- a build that is still running ----

// LiveView is a running build on its job's page: what runs now and how
// long the rest takes if it goes as the last time this image was built,
// the node's load, and every step with its last lines.
type LiveView struct {
	Head     string        // "Building now · 13 min 02 s · dev-worker2"
	Sentence template.HTML // what runs now, and when it is likely done
	Detail   template.HTML // faster or slower than the last time; may be empty
	// Done: BuildKit finished the build; the console stores it in a moment
	// and the page becomes the build's.
	Done bool
	Err  string // its progress could not be read
	// Steps done of all; how many came from the cache.
	Finished, Total, Cached int
	Tiles                   []LiveTile
	Ticks                   []Tick
	NowL, NowLabel          string
	Rows                    []LiveRow
}

// LiveTile is one figure of the node's load.
type LiveTile struct {
	K, V, S string
	Spark   *LoadChart
	Mem     bool // the sparkline is the memory's
}

// LiveRow is one step on the running build's time axis.
type LiveRow struct {
	ID         string // stable across updates: the step's key, hashed
	Key, Name  string
	State      string // waiting, running, built, cached, failed, canceled
	Bar, Ghost *WFBar // what it took so far; what it took the last time
	Label      template.HTML
	LabelL     string
	LabelEnd   string // near the right edge: how far from it the label ends
	// Open: the step's lines show at first (the running steps).
	Open  bool
	Lines string // "9 106 lines so far"
	Took  string // "10 min 32 s, last time 11 min 41 s"
	Tail  []LiveLine
	Note  string // instead of lines: "The step printed nothing."
}

// liveLane is a step's lane with its build's view (the "now" mark).
type liveLane struct {
	View *LiveView
	Row  LiveRow
}

// LiveLine is one printed line: seconds into its step, and the text.
type LiveLine struct{ At, Text string }

// liveRef is the build a running one is compared with: the last build of
// the same image that executed the step running now.
type liveRef struct {
	wall  time.Duration
	when  time.Time
	steps map[string]refStep // by key and name
}

type refStep struct {
	off, dur time.Duration // from the build's start
	built    bool
}

func liveKey(key, name string) string { return key + "\x00" + name }

func refOf(b *history.Build) *liveRef {
	r := &liveRef{wall: b.Completed.Sub(b.Started), when: b.Started, steps: map[string]refStep{}}
	for _, s := range b.Steps {
		if s.Started.IsZero() || s.Completed.Before(s.Started) {
			continue
		}
		r.steps[liveKey(s.Key, s.Name)] = refStep{off: s.Started.Sub(b.Started), dur: s.Duration(), built: s.State == history.Built}
	}
	return r
}

// prediction is where a running build stands against its reference.
type prediction struct {
	// delta is how much later than the reference the build is (negative:
	// earlier), at the end of its latest step that both executed; known
	// says there is one.
	delta     time.Duration
	known     bool
	by        history.LiveStep // that step
	byRef     refStep
	total     time.Duration // the build's likely length
	remaining time.Duration
}

// predict compares a running build with the reference: the build is as
// far ahead as at the end of its latest step both executed, and the rest
// takes as long as it did then; a running step that already took longer
// than the last time pushes the end out by as much.
func predict(steps []history.LiveStep, started, now time.Time, ref *liveRef) prediction {
	var p prediction
	for _, s := range steps {
		r, ok := ref.steps[liveKey(s.Key, s.Name)]
		if !ok || !r.built || s.State != history.LiveBuilt || s.Completed.IsZero() {
			continue
		}
		if !p.known || s.Completed.After(p.by.Completed) {
			p.known, p.by, p.byRef = true, s, r
			p.delta = s.Completed.Sub(started) - (r.off + r.dur)
		}
	}
	p.total = ref.wall + p.delta
	var over time.Duration
	for _, s := range steps {
		r, ok := ref.steps[liveKey(s.Key, s.Name)]
		if s.State != history.LiveRunning || !ok || !r.built {
			continue
		}
		over = max(over, now.Sub(s.Started)-r.dur)
	}
	p.total += max(over, 0)
	p.remaining = p.total - now.Sub(started)
	return p
}

// refCache keeps the reference of each running build's image for a while:
// a running build's page asks for itself every two seconds.
type refCache struct {
	mu sync.Mutex
	m  map[string]refEntry
}

type refEntry struct {
	ref *liveRef // nil: none found
	at  time.Time
}

const refFor = time.Minute

// liveReference finds the build a running one is compared with: of the
// project's last builds of the same target, those that succeeded, the
// job of the same name first; the first that executed the step running
// now (or, with nothing running, shares half the steps).
func (s *Server) liveReference(ctx context.Context, instance string, project uint64, lb *collector.LiveBuild, jobName string) *liveRef {
	running := ""
	keys := map[string]bool{}
	for _, st := range lb.Steps {
		keys[liveKey(st.Key, st.Name)] = true
		if st.State == history.LiveRunning && running == "" {
			running = liveKey(st.Key, st.Name)
		}
	}
	ck := fmt.Sprintf("%s/%d/%s/%s/%s", instance, project, lb.Target, jobName, running)
	s.refs.mu.Lock()
	if e, ok := s.refs.m[ck]; ok && s.now().Sub(e.at) < refFor {
		s.refs.mu.Unlock()
		return e.ref
	}
	s.refs.mu.Unlock()
	q := store.BuildQuery{Project: store.ProjectKey{Instance: instance, ID: project}, Target: lb.Target, NoTarget: lb.Target == "", Limit: 30}
	rows, err := s.data.Builds(ctx, q)
	if err != nil {
		return nil
	}
	var cands []store.BuildRow
	for pass := 0; pass < 2; pass++ {
		for _, r := range rows {
			if r.Completed.IsZero() || r.Error != "" || r.Ref == lb.Ref || (r.JobName == jobName) != (pass == 0) {
				continue
			}
			cands = append(cands, r)
		}
	}
	var found *liveRef
	for i, r := range cands {
		if i == 4 { // each candidate is a build read whole
			break
		}
		page, err := s.data.BuildPage(ctx, r.ID)
		if err != nil || page == nil || page.Build == nil {
			continue
		}
		ref := refOf(page.Build)
		shared := 0
		for k := range keys {
			if _, ok := ref.steps[k]; ok {
				shared++
			}
		}
		if running != "" && ref.steps[running].built || running == "" && 2*shared >= len(keys) && shared > 0 {
			found = ref
			break
		}
	}
	s.refs.mu.Lock()
	if s.refs.m == nil || len(s.refs.m) > 500 {
		s.refs.m = map[string]refEntry{}
	}
	s.refs.m[ck] = refEntry{found, s.now()}
	s.refs.mu.Unlock()
	return found
}

// liveBuildsOf are the running builds that say they are for a job.
func (s *Server) liveBuildsOf(ctx context.Context, instance string, pipeline, job uint64) []*collector.LiveBuild {
	var out []*collector.LiveBuild
	for _, d := range s.data.Daemons() {
		for _, a := range d.Active {
			// A build counts as the job's only when its pipeline agrees,
			// as a completed one's does.
			if a.Instance != instance || a.Job != job || a.Pipeline != pipeline {
				continue
			}
			lb, err := s.data.LiveBuild(ctx, a.Daemon, a.Ref)
			switch {
			case err != nil:
				out = append(out, &collector.LiveBuild{Active: a, Err: liveErr(err)})
			case lb != nil:
				out = append(out, lb)
			}
		}
	}
	return out
}

func liveErr(err error) string {
	if strings.Contains(err.Error(), collector.ErrTooManyWatched.Error()) {
		return "the console follows too many running builds at once; this one shows when it is done"
	}
	return "its progress could not be read: " + err.Error()
}

// stepNameShort is a step's command as a sentence names it.
func stepNameShort(name string) string {
	n := strings.TrimPrefix(name, "RUN ")
	if len([]rune(n)) > 70 {
		n = string([]rune(n)[:69]) + "…"
	}
	return n
}

// liveView lays a running build out for its job's page.
func (s *Server) liveView(ctx context.Context, lb *collector.LiveBuild, instance string, project uint64, jobName, node string, loc *time.Location) *LiveView {
	now := s.now()
	v := &LiveView{Done: lb.Done, Err: lb.Err}
	elapsed := now.Sub(lb.Started)
	if lb.Started.IsZero() || elapsed < 0 {
		elapsed = 0
	}
	if node == "" {
		node = shortDaemon(lb.Daemon)
	}
	v.Head = joinSet(" · ", "Building now", duration(elapsed.Truncate(time.Second)), node)
	if lb.Done {
		v.Head = joinSet(" · ", "Finished", node)
	}
	var running []history.LiveStep
	for _, st := range lb.Steps {
		v.Total++
		switch st.State {
		case history.LiveRunning:
			running = append(running, st)
		case history.LiveCached:
			v.Cached++
			v.Finished++
		case history.LiveBuilt, history.LiveFailed, history.LiveCanceled:
			v.Finished++
		}
	}
	var ref *liveRef
	if !lb.Done && lb.Err == "" {
		ref = s.liveReference(ctx, instance, project, lb, jobName)
	}
	var p prediction
	if ref != nil {
		p = predict(lb.Steps, lb.Started, now, ref)
	}
	// What runs now.
	var sb strings.Builder
	switch {
	case lb.Done:
		sb.WriteString("BuildKit finished this build; the console stores it in a moment, and this page becomes the build's.")
	case len(running) == 1:
		fmt.Fprintf(&sb, "<code>%s</code> for %s", e(stepNameShort(running[0].Name)), e(duration(now.Sub(running[0].Started).Truncate(time.Second))))
	case len(running) > 1:
		fmt.Fprintf(&sb, "%d steps at once: <code>%s</code>", len(running), e(stepNameShort(running[0].Name)))
		if len(running) == 2 {
			fmt.Fprintf(&sb, " and <code>%s</code>", e(stepNameShort(running[1].Name)))
		} else {
			fmt.Fprintf(&sb, " and %d more", len(running)-1)
		}
	case v.Total > 0 && v.Finished == v.Total:
		sb.WriteString("Every step is done: exporting and pushing")
	default:
		sb.WriteString("Preparing: reading the build definition and sending the context")
	}
	if !lb.Done && ref != nil && v.Total > 0 {
		switch {
		case p.remaining > 5*time.Second:
			fmt.Fprintf(&sb, "; if the rest goes as last time, done in about %s", e(duration(p.remaining.Round(10*time.Second))))
		default:
			sb.WriteString("; if the rest goes as last time, done any moment now")
		}
	}
	v.Sentence = template.HTML(sb.String())
	if p.known && !lb.Done {
		word, d := "faster", -p.delta
		if p.delta > 0 {
			word, d = "slower", p.delta
		}
		if d >= time.Second {
			v.Detail = template.HTML(fmt.Sprintf("So far <b>%s %s</b> than the last build of this image that ran these steps (%s): <code>%s</code> took %s against %s.",
				e(duration(d.Round(time.Second))), word, e(ref.when.In(loc).Format("2 Jan")), e(stepNameShort(p.by.Name)),
				e(duration(p.by.Completed.Sub(p.by.Started))), e(duration(p.byRef.dur))))
		}
	}
	if lb.Load != nil {
		v.Tiles = liveTiles(lb, now, loc)
	}
	// The time axis: the build so far, or as long as it likely takes.
	span := max(elapsed, p.total, time.Second)
	span = span * 112 / 100
	v.Ticks = ticksFor(span)
	at := func(d time.Duration) string { return strconv.FormatFloat(pctOf(d, span), 'f', 2, 64) }
	width := func(d time.Duration) string { return strconv.FormatFloat(max(0.3, pctOf(d, span)), 'f', 2, 64) }
	v.NowL, v.NowLabel = at(elapsed), "now "+clockLabel(elapsed)
	for _, st := range lb.Steps {
		r := LiveRow{Key: st.Key, Name: st.Name, State: st.State, Open: st.State == history.LiveRunning}
		sum := sha256.Sum256([]byte(lb.Ref + "\x00" + st.Key + "\x00" + st.Name))
		r.ID = "ls-" + hex.EncodeToString(sum[:6])
		var rs refStep
		hasRef := false
		if ref != nil {
			rs, hasRef = ref.steps[liveKey(st.Key, st.Name)]
			hasRef = hasRef && rs.built
		}
		end := elapsed
		switch {
		case st.State == history.LiveWaiting:
		case st.State == history.LiveRunning:
			off := st.Started.Sub(lb.Started)
			r.Bar = &WFBar{Class: "running", L: at(off), W: width(end - off)}
			if hasRef {
				r.Ghost = &WFBar{Class: "ghost", L: at(off), W: width(rs.dur)}
				end = max(end, off+rs.dur)
			}
		default:
			off, d := st.Started.Sub(lb.Started), st.Completed.Sub(st.Started)
			r.Bar = &WFBar{Class: st.State, L: at(off), W: width(d)}
			end = off + d
		}
		if st.State == history.LiveWaiting && hasRef {
			off := rs.off + p.delta
			if off < elapsed {
				off = elapsed
			}
			r.Ghost = &WFBar{Class: "ghost", L: at(off), W: width(rs.dur)}
			end = off + rs.dur
		}
		label := ""
		switch st.State {
		case history.LiveRunning:
			if hasRef {
				label = "last time " + duration(rs.dur)
			}
		case history.LiveWaiting:
			if hasRef && st.Key == lastKey(lb.Steps) {
				label = "≈ " + clockLabel(p.total)
			}
		case history.LiveCached:
			label = "from cache"
		default:
			label = duration(st.Completed.Sub(st.Started))
		}
		r.Label, r.LabelL = template.HTML(e(label)), strconv.FormatFloat(pctOf(end, span)+0.6, 'f', 2, 64)
		if pctOf(end, span) > 80 && label != "" {
			// Near the right edge the label ends where the bars begin.
			lo := 100.0
			for _, b := range []*WFBar{r.Bar, r.Ghost} {
				if b != nil {
					lo = min(lo, mustFloat(b.L))
				}
			}
			r.LabelEnd = strconv.FormatFloat(100-lo+0.6, 'f', 2, 64)
		}
		switch st.State {
		case history.LiveWaiting:
		case history.LiveRunning:
			r.Lines = fmt.Sprintf("%s line%s so far · %s", thousands(st.Lines), plural(st.Lines), duration(now.Sub(st.Started).Truncate(time.Second)))
		default:
			r.Lines = fmt.Sprintf("%s line%s", thousands(st.Lines), plural(st.Lines))
			r.Took = duration(st.Completed.Sub(st.Started))
			if hasRef {
				r.Took += ", last time " + duration(rs.dur)
			}
		}
		for _, l := range st.Tail {
			r.Tail = append(r.Tail, LiveLine{At: strconv.FormatFloat(l.At.Seconds(), 'f', 1, 64), Text: l.Text})
		}
		switch {
		case st.Error != "":
			r.Note = st.Error
		case len(r.Tail) == 0 && st.State == history.LiveRunning:
			r.Note = "Nothing printed yet."
		case len(r.Tail) == 0 && st.State == history.LiveCached:
			r.Note = "From the cache: nothing ran."
		case len(r.Tail) == 0 && st.State != history.LiveWaiting:
			r.Note = "The step printed nothing."
		}
		v.Rows = append(v.Rows, r)
	}
	return v
}

// lastKey is the key of the last step BuildKit named (the image's last
// instruction, usually): its ghost carries the build's likely end.
func lastKey(steps []history.LiveStep) string {
	if len(steps) == 0 {
		return ""
	}
	return steps[len(steps)-1].Key
}

func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + " " + s[i:]
	}
	return s
}

// liveTiles are the node's CPU and memory, the builds on the daemon, and
// how long tasks waited for a core.
func liveTiles(lb *collector.LiveBuild, now time.Time, loc *time.Location) []LiveTile {
	lv := loadView(collector.Inspection{Load: lb.Load}, now, loc)
	if lv == nil || lv.Err != "" {
		return nil
	}
	cpu := LiveTile{K: "Node CPU", V: lv.CPUText + " cores", Spark: lv.Spark}
	mem := LiveTile{K: "Node memory", V: lv.MemText, Spark: lv.Spark, Mem: true}
	if lv.PodKnown {
		cpu.S, mem.S = "daemon "+lv.PodCPU+" cores", "daemon "+lv.PodMem
	}
	runs := LiveTile{K: "On this daemon", V: strconv.Itoa(lb.Running), S: "build running (this one)"}
	if lb.Running > 1 {
		runs.S = "builds running, this one included"
	}
	press := LiveTile{K: "Waiting for CPU", V: "—", S: "not reported by this node's kernel"}
	if p := lb.Load.Pressure; p != nil {
		press.V, press.S = fmt.Sprintf("%.0f %%", p.CPU60), "of the last minute some task waited for a core"
	}
	return []LiveTile{cpu, mem, runs, press}
}

// liveBuild turns a running build into a build the pipeline's timeline
// draws: its steps that started, the running ones up to now.
func liveBuild(lb *collector.LiveBuild, now time.Time) (*history.Build, []StepView) {
	b := &history.Build{Ref: lb.Ref, Daemon: lb.Daemon, Target: lb.Target, Started: lb.Started, Completed: now}
	for _, st := range lb.Steps {
		if st.State == history.LiveWaiting || st.Started.IsZero() {
			continue
		}
		hs := history.Step{Key: st.Key, Name: st.Name, State: history.StepState(st.State), Started: st.Started, Completed: st.Completed, Error: st.Error}
		if st.State == history.LiveRunning {
			hs.Completed = now
		}
		switch st.State {
		case history.LiveFailed, history.LiveCanceled:
			hs.State = history.Built
		case history.LiveRunning:
			hs.State = history.StepState(history.LiveRunning)
		}
		b.Steps = append(b.Steps, hs)
	}
	return b, steps(b, history.Reason{}, func(string) string { return "" }, nil)
}
