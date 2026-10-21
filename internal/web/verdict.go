package web

import (
	"fmt"
	"html"
	"html/template"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// Verdict is the answer a build page leads with: why the build took what
// it took, in one sentence, with what helps.
type Verdict struct {
	Icon   string
	Tone   string // cached, built, node, unknown, first, failed
	Kicker string
	Title  template.HTML
	Detail template.HTML
	Hint   template.HTML
	Diff   []string // old, new definition of a changed step
}

// Received is work a build took from another build that ran it at the
// same time.
type Received struct {
	Stages []string
	From   string // a job name, or "another build" outside the project
	Wait   time.Duration
}

// e escapes a value for HTML: every piece of a verdict that comes from a
// build (step names, paths, job names) is data, never markup.
func e(s string) string { return html.EscapeString(s) }

func code(s string) string {
	if len(s) > 90 {
		s = s[:88] + "…"
	}
	return "<code>" + e(s) + "</code>"
}

func paths(ps []string) string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = "<code>" + e(p) + "</code>"
	}
	return strings.Join(out, ", ")
}

func receivedLine(rec []Received) string {
	var parts []string
	for _, r := range rec {
		noun, pron := "stage", "it"
		if len(r.Stages) > 1 {
			noun, pron = "stages", "them"
		}
		parts = append(parts, fmt.Sprintf("Took %s <b>%s</b> from <b>%s</b>, which was building %s at the same time (waited %s).",
			noun, e(strings.Join(r.Stages, ", ")), e(r.From), pron, duration(r.Wait)))
	}
	return strings.Join(parts, " ")
}

// verdictFor explains a build. by is the job that settled a reason
// ("" when none); built counts the steps it executed other than resolving
// images.
func verdictFor(b *history.Build, r history.Reason, by string, built int, saved time.Duration, rec []Received) Verdict {
	recLine := receivedLine(rec)
	if canceled(b.Error) {
		return Verdict{Tone: "first", Kicker: "Canceled", Icon: "x", Title: template.HTML("The build was canceled."),
			Detail: template.HTML("The job was canceled, or its client went away, while BuildKit was building: nothing in the Dockerfile failed. BuildKit said: " + e(b.Error) + ".")}
	}
	if b.Error != "" {
		return Verdict{Tone: "failed", Kicker: "Failed", Title: template.HTML("The build failed."), Detail: template.HTML(e(b.Error))}
	}
	at := ""
	if r.Step.Line > 0 {
		at = fmt.Sprintf(" (Dockerfile line %d)", r.Step.Line)
	}
	switch r.Kind {
	case "":
		if built > 0 {
			// Its project is not known yet, or nothing to compare with.
			return Verdict{Tone: "first", Kicker: "Not compared", Title: template.HTML(fmt.Sprintf("%d steps were built.", built)),
				Detail: template.HTML(strings.TrimSpace("No earlier build of this target is on record to compare with. " + recLine))}
		}
		cached, received := 0, 0
		for _, s := range b.Steps {
			switch s.State {
			case history.Cached:
				cached++
			case history.Received:
				received++
			}
		}
		// Steps another build was running at the same time are not cache
		// hits: say where they came from.
		title, kicker := fmt.Sprintf("Nothing was rebuilt: %d step%s came from cache.", cached, plural(cached)), "From cache"
		switch {
		case received > 0 && cached == 0:
			title, kicker = fmt.Sprintf("Nothing was rebuilt: %d step%s came from a build running at the same time.", received, plural(received)), "Shared with another build"
		case received > 0:
			title = fmt.Sprintf("Nothing was rebuilt: %d step%s came from cache, %d from a build running at the same time.", cached, plural(cached), received)
		}
		v := Verdict{Tone: "cached", Kicker: kicker, Title: template.HTML(title)}
		if saved > 0 {
			v.Detail = template.HTML(fmt.Sprintf("Cache saved about %s of build time.", duration(saved)))
		}
		if recLine != "" {
			v.Detail = template.HTML(strings.TrimSpace(string(v.Detail) + " " + recLine))
		}
		return v
	case history.FirstBuild:
		d := recLine
		if d == "" {
			d = "The next build with the same inputs will come from cache on this node."
		}
		return Verdict{Tone: "first", Kicker: "First build on record",
			Title:  template.HTML(fmt.Sprintf("First build of target <code>%s</code> on record — nothing to compare with yet.", e(b.Target))),
			Detail: template.HTML(d)}
	case history.StepChanged, history.NewStep:
		v := Verdict{Tone: "built", Kicker: "Dockerfile changed",
			Title:  template.HTML(fmt.Sprintf("Rebuilt from %s%s: this step changed.", code(r.Step.Name), at)),
			Detail: template.HTML(strings.TrimSpace("Every step after it was rebuilt too, because its input is new. " + recLine))}
		if r.Old != "" && r.Old != r.New {
			v.Diff = []string{r.Old, r.New}
		}
		return v
	case history.ArgChanged:
		if strings.Contains(r.Arg, ", ") {
			names := strings.Split(r.Arg, ", ")
			for i, n := range names {
				names[i] = "<code>" + e(n) + "</code>"
			}
			return Verdict{Tone: "built", Kicker: "Build arguments changed",
				Title:  template.HTML(fmt.Sprintf("Rebuilt from %s%s: build arguments %s changed.", code(r.Step.Name), at, strings.Join(names, ", "))),
				Detail: template.HTML("The Dockerfile did not change. Steps after it were rebuilt because their input changed.")}
		}
		return Verdict{Tone: "built", Kicker: "Build argument changed",
			Title:  template.HTML(fmt.Sprintf("Rebuilt from %s%s: build argument <code>%s</code> changed.", code(r.Step.Name), at, e(r.Arg))),
			Detail: template.HTML(fmt.Sprintf("<code>%s</code> → <code>%s</code>. Steps after it were rebuilt because their input changed.", e(r.Old), e(r.New)))}
	case history.BaseImageChanged:
		return Verdict{Tone: "built", Kicker: "Base image changed",
			Title:  template.HTML(fmt.Sprintf("Rebuilt from %s%s: the base image points to a new image.", code(r.Step.Name), at)),
			Detail: template.HTML(fmt.Sprintf("<code>%s</code> → <code>%s</code>.", e(r.Old), e(r.New)))}
	case history.FilesChanged:
		v := Verdict{Tone: "built", Kicker: "Files changed",
			Title:  template.HTML(fmt.Sprintf("Rebuilt from %s%s: %s changed.", code(r.Step.Name), at, paths(r.Paths))),
			Detail: template.HTML("Steps after it were rebuilt because their input changed.")}
		if by != "" {
			v.Detail += template.HTML(fmt.Sprintf(" Confirmed by <b>%s</b> in this pipeline, which ran on the node holding the cache and still had to rebuild it.", e(by)))
		}
		for _, p := range r.Paths {
			if p == "." {
				v.Hint = "<b>Tip:</b> <code>COPY . .</code> copies the whole repository, so any change to any file rebuilds this and everything after it. Copy only what this stage needs, or list the rest in <code>.dockerignore</code>."
			}
		}
		return v
	case history.OtherNode:
		if by == "" {
			return Verdict{Tone: "node", Kicker: "Not your change",
				Title: "Rebuilt because this job ran on another build node.",
				Detail: template.HTML(fmt.Sprintf("%s%s reads no files and did not change: its cache is on node <b>%s</b>, where this target was built last time, and this job ran on <b>%s</b>.",
					code(r.Step.Name), at, e(shortDaemon(r.Old)), e(shortDaemon(r.New)))),
				Hint: "<b>What helps:</b> keeping a project's jobs on the node that holds its cache (sticky routing, planned). Until then: fewer, larger build nodes, or pin the project to one node with runner tags."}
		}
		return Verdict{Tone: "node", Kicker: "Not your change",
			Title: "Rebuilt because this job ran on another build node.",
			Detail: template.HTML(fmt.Sprintf("%s%s was cached on node <b>%s</b>, where this target was built last time; this job ran on <b>%s</b>. <b>%s</b> in this pipeline got the same step from cache on its node, so the files did not change.",
				code(r.Step.Name), at, e(shortDaemon(r.Old)), e(shortDaemon(r.New)), e(by))),
			Hint: "<b>What helps:</b> keeping a project's jobs on the node that holds its cache (sticky routing, planned). Until then: fewer, larger build nodes, or pin the project to one node with runner tags."}
	case history.CacheElsewhere:
		return Verdict{Tone: "unknown", Kicker: "Files or node",
			Title: template.HTML(fmt.Sprintf("Rebuilt from %s%s.", code(r.Step.Name), at)),
			Detail: template.HTML(fmt.Sprintf("Either %s changed, or the cache is on node <b>%s</b>, where this target was built last time (this job ran on <b>%s</b>). No other job in this pipeline settles which.",
				orContext(r.Paths), e(shortDaemon(r.Old)), e(shortDaemon(r.New)))),
			Hint: "<b>Why it is unclear:</b> BuildKit's build records do not keep a fingerprint of the copied files."}
	case history.NotInCache:
		return Verdict{Tone: "unknown", Kicker: "Not in the cache",
			Title:  template.HTML(fmt.Sprintf("Rebuilt from %s%s: it was not found in the cache.", code(r.Step.Name), at)),
			Detail: "Same step as last time and it reads no files: the cache was cleaned up, or it is on another node."}
	}
	return Verdict{Tone: "built", Kicker: string(r.Kind), Title: template.HTML(fmt.Sprintf("Rebuilt from %s%s.", code(r.Step.Name), at))}
}

func orContext(ps []string) string {
	if len(ps) == 0 {
		return "its input"
	}
	return paths(ps)
}

// duration prints a duration the way the pages do: 0.4 s, 12 s, 1 min 16 s.
func duration(d time.Duration) string {
	s := d.Seconds()
	switch {
	case s < 10:
		return fmt.Sprintf("%.1f s", s)
	case s < 60:
		return fmt.Sprintf("%.0f s", s)
	case s >= 3600:
		return fmt.Sprintf("%d h %d min", int(s)/3600, int(s)%3600/60)
	default:
		return fmt.Sprintf("%d min %d s", int(s)/60, int(s)%60)
	}
}
