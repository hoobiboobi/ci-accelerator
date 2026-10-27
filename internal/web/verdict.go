package web

import (
	"fmt"
	"html"
	"html/template"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/route"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
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
		v := Verdict{Tone: "failed", Kicker: "Failed", Title: template.HTML("The build failed."), Detail: template.HTML(e(b.Error))}
		switch r.Kind {
		case history.StepChanged:
			v.Title = template.HTML(fmt.Sprintf("The build failed at %s, a step this build changed.", code(r.Step.Name)))
			if r.Old != "" && r.Old != r.New {
				v.Diff = []string{r.Old, r.New}
			}
		case history.NewStep:
			v.Title = template.HTML(fmt.Sprintf("The build failed at %s, a step new in this build.", code(r.Step.Name)))
		}
		return v
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
		// Without a settling (see withVia for the GitLab-verified case) the
		// paths are candidates, never a verified diff: "changed" may only
		// name a file the comparison listed.
		v := Verdict{Tone: "built", Kicker: "Files changed",
			Title:  template.HTML(fmt.Sprintf("Rebuilt from %s%s: a file it builds on changed.", code(r.Step.Name), at)),
			Detail: template.HTML(fmt.Sprintf("It builds on %s from the repository; which of them changed, BuildKit's records cannot tell. Steps after it were rebuilt because their input changed.", orContext(r.Paths)))}
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
				Hint: nodeHint(nil)}
		}
		return Verdict{Tone: "node", Kicker: "Not your change",
			Title: "Rebuilt because this job ran on another build node.",
			Detail: template.HTML(fmt.Sprintf("%s%s was cached on node <b>%s</b>, where this target was built last time; this job ran on <b>%s</b>. <b>%s</b> in this pipeline got the same step from cache on its node, so the files did not change.",
				code(r.Step.Name), at, e(shortDaemon(r.Old)), e(shortDaemon(r.New)), e(by))),
			Hint: nodeHint(nil)}
	case history.CacheElsewhere:
		detail := fmt.Sprintf("Either %s changed, or this node's cache no longer had the step. No other job in this pipeline settles which.",
			orContext(r.Paths))
		if r.Old != "" {
			detail = fmt.Sprintf("Either %s changed, or the cache is on node <b>%s</b>, where this target was built last time (this job ran on <b>%s</b>). No other job in this pipeline settles which.",
				orContext(r.Paths), e(shortDaemon(r.Old)), e(shortDaemon(r.New)))
		}
		return Verdict{Tone: "unknown", Kicker: "Files or node",
			Title:  template.HTML(fmt.Sprintf("Rebuilt from %s%s.", code(r.Step.Name), at)),
			Detail: template.HTML(detail),
			Hint:   "<b>Why it is unclear:</b> BuildKit's build records do not keep a fingerprint of the copied files."}
	case history.NotInCache:
		v := Verdict{Tone: "unknown", Kicker: "Not in the cache",
			Title:  template.HTML(fmt.Sprintf("Rebuilt from %s%s: it was not found in the cache.", code(r.Step.Name), at)),
			Detail: "Same step as last time, and nothing below it reads the build context: this node's cache no longer had it — cleaned up, or never built here."}
		if by != "" {
			v.Detail = template.HTML(fmt.Sprintf("Same step as last time. <b>%s</b> in this pipeline got it from cache on its node, so the files did not change: this node's cache no longer had it — cleaned up, or never built here.", e(by)))
		}
		return v
	}
	return Verdict{Tone: "built", Kicker: string(r.Kind), Title: template.HTML(fmt.Sprintf("Rebuilt from %s%s.", code(r.Step.Name), at))}
}

// withVia tells how a reason settled after the build was stored: GitLab's
// comparison of the commits of this build and of the image's previous one.
func withVia(v Verdict, r history.Reason, via string) Verdict {
	cmp, ok := strings.CutPrefix(via, "gitlab ")
	switch {
	case strings.HasPrefix(via, "same commit"):
		cmp, ok = "", true
	case !ok:
		return v
	}
	source := "The previous build of this image built the same commit"
	if cmp != "" {
		source = fmt.Sprintf("GitLab's comparison of the commits of the previous build and this one (<code>%s</code>)", e(cmp))
	}
	switch r.Kind {
	case history.OtherNode:
		v.Detail = template.HTML(fmt.Sprintf("%s copies %s, and %s shows no change there. Its cache is on node <b>%s</b>, where the previous build ran; this job ran on <b>%s</b>.",
			code(r.Step.Name), orContext(r.Paths), source, e(shortDaemon(r.Old)), e(shortDaemon(r.New))))
		if cmp == "" {
			v.Detail = template.HTML(fmt.Sprintf("%s: nothing %s copies changed. Its cache is on node <b>%s</b>, where that build ran; this job ran on <b>%s</b>.",
				source, code(r.Step.Name), e(shortDaemon(r.Old)), e(shortDaemon(r.New))))
		}
	case history.FilesChanged:
		// Settled by the comparison: the paths are its verified hits, so
		// naming them as changed is proven.
		at := ""
		if r.Step.Line > 0 {
			at = fmt.Sprintf(" (Dockerfile line %d)", r.Step.Line)
		}
		v.Title = template.HTML(fmt.Sprintf("Rebuilt from %s%s: %s changed.", code(r.Step.Name), at, paths(r.Paths)))
		if r.Old != "" {
			v.Detail = template.HTML(fmt.Sprintf("Steps after it were rebuilt because their input changed. %s lists them; the previous build ran on another node, so it would have been rebuilt there too.", source))
		} else {
			v.Detail = template.HTML(fmt.Sprintf("Steps after it were rebuilt because their input changed. %s lists them.", source))
		}
	case history.NotInCache:
		if cmp != "" {
			v.Detail = template.HTML(fmt.Sprintf("%s shows no change in what feeds this step. This node's cache no longer had it — cleaned up, or never built here.", source))
		} else {
			v.Detail = template.HTML(fmt.Sprintf("%s, so its files did not change. This node's cache no longer had the step — cleaned up, or never built here.", source))
		}
	}
	return v
}

// nodeHint says why a build ran away from its image's cache, from how the
// build router placed the job (nil: the router did not place it).
func nodeHint(d *store.Decision) template.HTML {
	if d == nil {
		return "<b>What helps:</b> the build router (on by default in the box) sends each project's jobs to the node that holds the project's cache. Without it: fewer, larger build nodes, or pin the project to one node with runner tags."
	}
	switch d.Reason {
	case route.LocalWarm, route.ZoneWarm:
		return "<b>Why here:</b> the build router follows the project, not each image: it sent the job to the node where the project built most recently, and this image's previous build ran on another node."
	case route.ZoneHash:
		return "<b>Why here:</b> when the job started, no node's cache was known to hold the project's layers (its builds were older than the router's window, or their daemons were replaced), so the build router chose the project's node by hash."
	case route.CrossZone:
		return "<b>Why here:</b> no daemon in the job's zone was ready, so the build router sent the job to another zone."
	case route.UnknownLocal, route.UnknownHash:
		return "<b>Why here:</b> the build router could not tell which project the job belongs to (its pod was not found in time), so it used a daemon without looking at the cache."
	}
	return ""
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
	// A no-break space keeps a number with its unit.
	case s < 10:
		return fmt.Sprintf("%.1f\u00a0s", s)
	case s < 60:
		return fmt.Sprintf("%.0f\u00a0s", s)
	case s >= 3600:
		return fmt.Sprintf("%d\u00a0h %d\u00a0min", int(s)/3600, int(s)%3600/60)
	default:
		return fmt.Sprintf("%d\u00a0min %d\u00a0s", int(s)/60, int(s)%60)
	}
}
