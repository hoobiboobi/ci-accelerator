package web

import (
	"fmt"
	"html/template"
	"regexp"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// Segment is one part of where a build's time went.
type Segment struct {
	Class, Label string
	D            time.Duration
	X, W         float64 // position and width in a 0..1000 bar
}

// StepView is one step of a build page.
type StepView struct {
	Index        int
	Key, Name    string
	Stage        string
	Lines        string
	State        string // built, cached, received, image, failed, canceled
	D            time.Duration
	Offset       time.Duration // from the build's start
	From         string        // who it was received from
	FirstRebuilt bool
	HasOutput    bool
}

// StageView groups the steps of one Dockerfile stage.
type StageView struct {
	Name         string
	State        string
	Steps        []StepView
	D            time.Duration
	FirstRebuilt bool
	From         string
}

// Line is one line of the Dockerfile view.
type Line struct {
	N      int
	HTML   template.HTML
	Step   int    // index into the steps, -1 for none
	State  string // the step's state when Step >= 0
	D      time.Duration
	Dim    bool // in a stage this target does not use
	Meta   bool // an instruction that changes only image metadata
	First  bool // the first line of the first rebuilt step
	Opens  bool // the step's first line (where its marker goes)
	Stage  string
	Head   bool   // the FROM line of a stage
	Showed string // duration text on the step's first line
}

func isFrom(s history.Step) bool { return strings.HasPrefix(s.Name, "FROM ") }

var stageKey = regexp.MustCompile(`^\[(?:(.+) )?\d+/\d+\]$`)

func stageOf(key string) string {
	if m := stageKey.FindStringSubmatch(key); m != nil && m[1] != "" {
		return m[1]
	}
	return "build"
}

func stateOf(s history.Step) string {
	if canceled(s.Error) {
		return "canceled"
	}
	if s.Error != "" {
		return "failed"
	}
	if isFrom(s) {
		return "image"
	}
	return string(s.State)
}

func fromName(name string) string {
	n := strings.TrimPrefix(name, "FROM ")
	n = strings.TrimPrefix(n, "docker.io/library/")
	if i := strings.Index(n, "@sha256:"); i >= 0 {
		n = n[:i]
	}
	return n
}

// steps builds the step rows. received names who a step came from.
func steps(b *history.Build, r history.Reason, received func(ref string) string, withOutput map[int]bool) []StepView {
	out := make([]StepView, len(b.Steps))
	for i, s := range b.Steps {
		v := StepView{Index: i, Key: s.Key, Name: s.Name, Stage: stageOf(s.Key), State: stateOf(s), D: s.Duration(),
			Lines: lineText(s.Lines), HasOutput: withOutput[i]}
		// A step received from another build may have started a moment
		// before this build did.
		if !s.Started.IsZero() && !b.Started.IsZero() && s.Started.After(b.Started) {
			v.Offset = s.Started.Sub(b.Started)
		}
		if s.State == history.Received {
			v.From = received(s.ReceivedFrom)
		}
		v.FirstRebuilt = s.Key == r.Step.Key
		if isFrom(s) {
			v.Stage, v.Name = "", fromName(s.Name)
		}
		out[i] = v
	}
	return out
}

func lineText(rs []history.LineRange) string {
	var parts []string
	for _, r := range rs {
		if r.End > r.Start {
			parts = append(parts, fmt.Sprintf("%d–%d", r.Start, r.End))
		} else {
			parts = append(parts, fmt.Sprint(r.Start))
		}
	}
	return strings.Join(parts, ", ")
}

func stagesOf(steps []StepView) []StageView {
	var out []StageView
	for _, s := range steps {
		name := s.Stage
		if s.State == "image" {
			name = s.Name
		}
		if len(out) == 0 || out[len(out)-1].Name != name || s.State == "image" {
			out = append(out, StageView{Name: name})
		}
		g := &out[len(out)-1]
		g.Steps = append(g.Steps, s)
		g.D += s.D
		g.FirstRebuilt = g.FirstRebuilt || s.FirstRebuilt
		if s.From != "" {
			g.From = s.From
		}
	}
	for i := range out {
		g := &out[i]
		g.State = "cached"
		for _, s := range g.Steps {
			switch {
			case s.State == "image":
				g.State = "image"
			case s.State == "built":
				g.State = "built"
			case s.State == "received" && g.State == "cached":
				g.State = "received"
			}
		}
		for _, s := range g.Steps {
			if s.State == "failed" || (s.State == "canceled" && g.State != "failed") {
				g.State = s.State
			}
		}
	}
	return out
}

// segments splits a build's time: sending the context (before the first
// step), pulling base images, waiting for other builds, building, and
// exporting and pushing (after the last step), and the rest: cache checks
// and scheduling between steps. Steps that ran in parallel can add up to
// more than the build took; the bar is relative to the parts.
func segments(b *history.Build) ([]Segment, time.Duration) {
	if b.Started.IsZero() || b.Completed.IsZero() || len(b.Steps) == 0 {
		return nil, 0
	}
	total := b.Completed.Sub(b.Started)
	first, last := b.Completed, b.Started
	var pull, recv, built time.Duration
	for _, s := range b.Steps {
		if !s.Started.IsZero() && s.Started.Before(first) {
			first = s.Started
		}
		if s.Completed.After(last) {
			last = s.Completed
		}
		switch {
		case isFrom(s):
			pull += s.Duration()
		case s.State == history.Received:
			recv += s.Duration()
		case s.State == history.Built:
			built += s.Duration()
		}
	}
	segs := []Segment{
		{Class: "xfer", Label: "Send context", D: clamp(first.Sub(b.Started))},
		{Class: "image", Label: "Base image", D: pull},
		{Class: "received", Label: "Waiting for another job", D: recv},
		{Class: "built", Label: "Build steps", D: built},
		{Class: "push", Label: "Export & push", D: clamp(b.Completed.Sub(last))},
	}
	var all time.Duration
	for _, s := range segs {
		all += s.D
	}
	if rest := total - all; rest > 0 {
		segs = append(segs, Segment{Class: "other", Label: "Other (cache checks, scheduling)", D: rest})
	}
	var sum time.Duration
	var kept []Segment
	for _, s := range segs {
		if s.D >= 50*time.Millisecond {
			kept = append(kept, s)
			sum += s.D
		}
	}
	x := 0.0
	for i := range kept {
		kept[i].X = x
		kept[i].W = 1000 * float64(kept[i].D) / float64(sum)
		x += kept[i].W
	}
	return kept, total
}

func clamp(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

var (
	instr    = regexp.MustCompile(`^(\s*)([A-Za-z]+)(\s.*)?$`)
	metaInst = map[string]bool{"ENV": true, "ARG": true, "USER": true, "CMD": true, "ENTRYPOINT": true, "EXPOSE": true, "LABEL": true,
		"VOLUME": true, "STOPSIGNAL": true, "HEALTHCHECK": true, "SHELL": true, "MAINTAINER": true, "ONBUILD": true}
	fromAs = regexp.MustCompile(`(?i)^\s*FROM\s+.*\s+AS\s+([A-Za-z0-9_.-]+)\s*$`)
	flags  = regexp.MustCompile(`(--[a-z-]+)(=\S*)?`)
	varRef = regexp.MustCompile(`\$\{?[A-Za-z_][A-Za-z0-9_]*\}?`)
)

// dockerfile lays the build's Dockerfile out with each line's step.
func dockerfile(text string, stepViews []StepView, b *history.Build) []Line {
	if text == "" {
		return nil
	}
	byLine := map[int]int{}
	for i, s := range b.Steps {
		for _, r := range s.Lines {
			for l := r.Start; l <= r.End; l++ {
				if _, ok := byLine[l]; !ok {
					byLine[l] = i
				}
			}
		}
	}
	used := map[string]bool{}
	named := false
	for _, s := range stepViews {
		if s.State != "image" {
			used[s.Stage] = true
			if s.Stage != "build" {
				named = true
			}
		}
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	out := make([]Line, len(lines))
	stage, cont := "", false
	opened := map[int]bool{}
	for i, raw := range lines {
		n := i + 1
		l := Line{N: n, Step: -1, HTML: highlight(raw, cont)}
		if m := fromAs.FindStringSubmatch(raw); m != nil {
			stage = strings.ToLower(m[1]) // as in BuildKit's step names
		} else if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(raw)), "FROM ") {
			stage = ""
		}
		l.Stage = stage
		l.Head = strings.HasPrefix(strings.ToUpper(strings.TrimSpace(raw)), "FROM ")
		if idx, ok := byLine[n]; ok {
			s := stepViews[idx]
			l.Step, l.State, l.D = idx, s.State, s.D
			if !opened[idx] {
				opened[idx], l.Opens, l.Showed = true, true, duration(s.D)
				l.First = s.FirstRebuilt
			}
		} else if m := instr.FindStringSubmatch(raw); m != nil && !cont && metaInst[strings.ToUpper(m[2])] {
			l.Meta = true
		}
		l.Dim = named && stage != "" && !used[stage] && l.Step < 0
		l.Meta = l.Meta && !l.Dim
		cont = strings.HasSuffix(strings.TrimSpace(raw), "\\")
		out[i] = l
	}
	return out
}

// highlight marks a Dockerfile line's instruction, flags, variables and
// comments. Everything is escaped first.
func highlight(raw string, continued bool) template.HTML {
	t := strings.TrimSpace(raw)
	if strings.HasPrefix(t, "#") {
		return template.HTML(`<span class="syn-c">` + e(raw) + `</span>`)
	}
	if continued {
		return template.HTML(decorate(raw))
	}
	m := instr.FindStringSubmatch(raw)
	if m == nil {
		return template.HTML(e(raw))
	}
	rest := decorate(m[3])
	if strings.EqualFold(m[2], "FROM") {
		body, as := m[3], ""
		if am := fromAsTail.FindStringSubmatchIndex(body); am != nil {
			as = `<span class="syn-k">` + e(body[am[2]:am[3]]) + `</span><span class="syn-s">` + e(body[am[4]:am[5]]) + `</span>`
			body = body[:am[0]]
		}
		rest = fromImage.ReplaceAllStringFunc(decorateFlagsOnly(body), func(x string) string {
			sp := fromImage.FindStringSubmatch(x)
			return sp[1] + `<span class="syn-img">` + sp[2] + `</span>`
		}) + as
	} else if strings.EqualFold(m[2], "ENV") || strings.EqualFold(m[2], "ARG") {
		// Keys first, on the escaped text: later steps add markup that
		// looks like key=value.
		keyed := envKey.ReplaceAllString(e(m[3]), `$1<span class="syn-v">$2</span>=`)
		keyed = flags.ReplaceAllString(keyed, `<span class="syn-f">$1</span>$2`)
		rest = varRef.ReplaceAllString(keyed, `<span class="syn-v">$0</span>`)
	}
	return template.HTML(e(m[1]) + `<span class="syn-i">` + e(m[2]) + `</span>` + rest)
}

var (
	fromAsTail = regexp.MustCompile(`(?i)(\s+AS\s+)([A-Za-z0-9_.-]+)\s*$`)
	// The image of a FROM line: the first word that is not a flag (escaped).
	fromImage = regexp.MustCompile(`^(\s*(?:<span class="syn-f">--[a-z-]+</span>=?\S*\s+)*)([^\s<]+)`)
	envKey    = regexp.MustCompile(`(^|\s)([A-Za-z_][A-Za-z0-9_]*)=`)
)

// decorateFlagsOnly escapes s and marks its flags.
func decorateFlagsOnly(s string) string {
	return flags.ReplaceAllString(e(s), `<span class="syn-f">$1</span>$2`)
}

func decorate(s string) string {
	escaped := e(s)
	escaped = flags.ReplaceAllString(escaped, `<span class="syn-f">$1</span>$2`)
	return varRef.ReplaceAllString(escaped, `<span class="syn-v">$0</span>`)
}
