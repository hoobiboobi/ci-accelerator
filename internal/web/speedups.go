package web

import (
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/hints"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// Speedup is one step's cache-mount hint as the pages show it: in the
// Speed-ups card of the build page, on the Dockerfile tab and in the
// step's panel.
type Speedup struct {
	N         int // its place in the card, from 1
	Step      int
	Key       string
	Line      int
	LineText  string // "line 6", "lines 6–9"
	Title     template.HTML
	Broken    bool // a cache mount that is there but cannot work
	Order     bool // installs again because of the Dockerfile's order
	Chip      string
	Why       []template.HTML
	Figures   string
	Fragment  string
	FragLines int
	Replace   string // what the fragment replaces: "line 6", "the step"
	Caveats   []string
	// Path commands print a manager's cache folder in the image, for a
	// hint that could not place it.
	PathCommands []string
}

var managerNames = map[string]string{
	"apt": "apt", "apk": "apk", "dnf": "dnf", "zypper": "zypper", "pip": "pip", "uv": "uv", "poetry": "Poetry",
	"conda": "conda", "npm": "npm", "yarn": "Yarn", "pnpm": "pnpm", "bun": "Bun", "go": "Go", "cargo": "Cargo",
	"maven": "Maven", "gradle": "Gradle", "sbt": "sbt", "nuget": "NuGet", "composer": "Composer", "bundler": "Bundler",
	"pub": "pub", "hex": "Hex", "stack": "Stack", "cabal": "cabal", "swift": "SwiftPM", "deno": "Deno",
	"ccache": "ccache", "sccache": "sccache", "next": "Next.js", "turbo": "Turborepo", "nx": "Nx",
}

func managerName(m string) string {
	if n, ok := managerNames[m]; ok {
		return n
	}
	return m
}

// speedups turns a build's stored hints into what the pages show: the
// cache-mount hints and the order hints together, the costliest first
// (what the step cost the image's builds in the window).
func speedups(hs []store.StepHints, order []store.OrderHint) []Speedup {
	type item struct {
		cost time.Duration
		v    Speedup
	}
	var items []item
	for _, h := range hs {
		items = append(items, item{h.RunTime, speedupOf(0, h)})
	}
	for _, o := range order {
		items = append(items, item{o.RunTime, orderSpeedup(o)})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].cost > items[j].cost })
	out := make([]Speedup, 0, len(items))
	for i, it := range items {
		it.v.N = i + 1
		out = append(out, it.v)
	}
	return out
}

// orderSpeedup is an order hint as the pages show it.
func orderSpeedup(o store.OrderHint) Speedup {
	v := Speedup{Step: o.Step, Key: o.Key, Order: true, Chip: "order", Fragment: o.Fragment, Replace: "the copy and the step"}
	first, last := 0, 0
	if len(o.CopyLines) > 0 {
		first = o.CopyLines[0].Start
	}
	if len(o.Lines) > 0 {
		v.Line = o.Lines[0].Start
		last = o.Lines[len(o.Lines)-1].End
		if first == 0 {
			first = v.Line
		}
	}
	if first > 0 && last > first {
		v.LineText = fmt.Sprintf("lines %d–%d", first, last)
		v.Replace = v.LineText
	} else if first > 0 {
		v.LineText = fmt.Sprintf("line %d", first)
	}
	if v.Fragment != "" {
		v.FragLines = strings.Count(v.Fragment, "\n") + 1
	}
	var names []string
	for _, m := range o.Managers {
		names = append(names, managerName(m))
	}
	who := strings.Join(names, " and ")
	copyCmd := code(o.CopyName)
	v.Title = template.HTML("Install the dependencies before copying the code")
	v.Why = append(v.Why, template.HTML(fmt.Sprintf("%s comes before the step that runs %s, so a change to any file of the build context installs the dependencies again, though only %s decide what %s installs.",
		copyCmd, e(who), filesText(o.Files), e(who))))
	for _, x := range o.Others {
		v.Why = append(v.Why, template.HTML(fmt.Sprintf("The step also runs %s, which needs the code: its time counts in the figure below too.", code(x))))
	}
	if v.Fragment == "" {
		v.Why = append(v.Why, template.HTML(fmt.Sprintf("Copy %s first and install from %s before %s; then copy the rest and run what needs the code.",
			filesText(o.Files), oneOrMany(len(o.Files), "it", "them"), copyCmd)))
	}
	v.Caveats = orderCaveats(o)
	v.Figures = orderFigures(o)
	return v
}

// filesText names files: "pyproject.toml", "go.mod and go.sum", "a, b and c".
func filesText(fs []string) string {
	var cs []string
	for _, f := range fs {
		cs = append(cs, string(code(f)))
	}
	switch len(cs) {
	case 0:
		return "its own files"
	case 1:
		return cs[0]
	}
	return strings.Join(cs[:len(cs)-1], ", ") + " and " + cs[len(cs)-1]
}

func oneOrMany(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// orderFigures is what installing again cost the image in the window:
// measured step times, never what the order would save.
func orderFigures(o store.OrderHint) string {
	days := int(store.HintWindow / (24 * time.Hour))
	if o.Runs <= 1 {
		return fmt.Sprintf("Installed again without a change to those files once in the last %d days of this image's builds: %s.", days, duration(o.RunTime))
	}
	return fmt.Sprintf("Installed again without a change to those files %d times in the last %d days of this image's builds: %s in total.", o.Runs, days, duration(o.RunTime))
}

// orderCaveats are what to keep in mind with the fragment.
func orderCaveats(o store.OrderHint) []string {
	var out []string
	for _, m := range o.Managers {
		switch m {
		case "npm", "yarn", "pnpm", "bun":
			out = append(out, "COPY --parents copies every package.json below the project with its folders, as a workspace (monorepo) needs; it needs the Dockerfile syntax 1.20 or later, which BuildKit v0.34 has built in.")
		case "pip", "uv":
			out = append(out, "The first install takes the dependencies pyproject.toml lists (requirement files without the lines that name the project or other files); the command after the copy adds the project and anything the files do not list.")
		case "composer":
			out = append(out, "The first composer install skips the autoloader and the project's scripts: they need the code, and the command after the copy runs them.")
		case "bundler":
			out = append(out, "A Gemfile that reads the project's gemspec (gemspec) needs the gemspec and the files it reads copied too.")
		}
	}
	out = append(out, "An install that reads other files of the repository (a local package, a patch, a config) needs them copied with the files above.")
	return out
}

func speedupOf(n int, h store.StepHints) Speedup {
	v := Speedup{N: n, Step: h.Step, Key: h.Key, Fragment: h.Fragment, Replace: "the step"}
	if len(h.Lines) > 0 {
		v.Line = h.Lines[0].Start
		v.LineText = fmt.Sprintf("line %d", h.Lines[0].Start)
		last := h.Lines[len(h.Lines)-1].End
		if last > v.Line {
			v.LineText = fmt.Sprintf("lines %d–%d", v.Line, last)
		}
		v.Replace = v.LineText
	}
	if v.Fragment != "" {
		v.FragLines = strings.Count(v.Fragment, "\n") + 1
	}
	var names []string
	for _, x := range h.Hints {
		names = append(names, managerName(x.Manager))
		v.Broken = v.Broken || x.Broken()
		for _, p := range x.Problems {
			v.Why = append(v.Why, problemText(x, p))
		}
		for _, c := range x.Caveats {
			if !contains(v.Caveats, c) {
				v.Caveats = append(v.Caveats, c)
			}
		}
		if x.PathCommand != "" && unplaced(x) {
			v.PathCommands = append(v.PathCommands, x.PathCommand)
		}
	}
	who := strings.Join(names, " and ")
	if v.Broken {
		v.Chip = "does not work"
		v.Title = template.HTML(fmt.Sprintf("The cache mount for %s does not work", e(who)))
		if onlyLocking(h.Hints) {
			v.Title = template.HTML(fmt.Sprintf("Lock the cache mount for %s", e(who)))
		}
	} else {
		v.Chip = "missing"
		v.Title = template.HTML(fmt.Sprintf("Add a cache mount for %s", e(who)))
	}
	v.Figures = figures(h)
	return v
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func unplaced(h hints.Hint) bool {
	for _, c := range h.Caches {
		if c.Path == "" {
			return true
		}
	}
	return false
}

func onlyLocking(hs []hints.Hint) bool {
	for _, h := range hs {
		for _, p := range h.Problems {
			if p.Kind != hints.NotLocked {
				return false
			}
		}
	}
	return true
}

// figures is what running the step cost the image in the window: counted
// runs, never an estimate of what a cache mount would save.
func figures(h store.StepHints) string {
	days := int(store.HintWindow / (24 * time.Hour))
	if h.Runs == 0 {
		return fmt.Sprintf("The image's builds did not run this step in the last %d days: it came from the cache.", days)
	}
	if h.Runs == 1 {
		return fmt.Sprintf("Ran once in the last %d days of this image's builds: %s.", days, duration(h.RunTime))
	}
	return fmt.Sprintf("Ran %d times in the last %d days of this image's builds: %s in total, median %s.",
		h.Runs, days, duration(h.RunTime), duration(h.Median))
}

func cacheLabel(h hints.Hint, label string) (string, string) {
	for _, c := range h.Caches {
		if c.Label == label {
			return c.Label, c.Path
		}
	}
	return label, ""
}

// problemText says what is wrong in one sentence; every name in it comes
// from the build and is escaped.
func problemText(h hints.Hint, p hints.Problem) template.HTML {
	m := e(managerName(h.Manager))
	switch p.Kind {
	case hints.Missing:
		label, path := cacheLabel(h, p.Cache)
		if path == "" {
			return template.HTML(fmt.Sprintf("No cache mount holds the %s of %s, so it fetches everything again whenever the step runs. Where it keeps it depends on what the record does not show (the image's user or settings).", e(label), m))
		}
		return template.HTML(fmt.Sprintf("No cache mount holds the %s of %s (%s), so it fetches everything again whenever the step runs.", e(label), m, code(path)))
	case hints.WrongPath:
		label, path := cacheLabel(h, p.Cache)
		if path == "" {
			return template.HTML(fmt.Sprintf("The cache mount is on %s, in root's home, but the step runs as another user: %s writes its %s to that user's home.", code(p.Mount), m, e(label)))
		}
		return template.HTML(fmt.Sprintf("The cache mount is on %s, but %s writes its %s to %s as this step runs.", code(p.Mount), m, e(label), code(path)))
	case hints.ImageNeeds:
		return template.HTML(fmt.Sprintf("The cache mount on %s holds what the image needs (%s): a cache mount is not part of the image, so the image goes without it.", code(p.Mount), e(p.Setting)))
	case hints.NotWritable:
		return template.HTML(fmt.Sprintf("The step runs as a user other than root, and the cache mount on %s says no uid, gid or mode: BuildKit makes it root's (mode 0755), so %s cannot write to it and it stays empty.", code(p.Mount), m))
	case hints.NotLocked:
		return template.HTML(fmt.Sprintf("The cache mount on %s is shared: %s takes one run at a time on its cache, and two builds using it at once fail on its lock. sharing=locked makes the second one wait.", code(p.Mount), m))
	case hints.Disabled:
		switch p.Setting {
		case "docker-clean":
			return template.HTML("The base image deletes downloaded packages after every install (<code>/etc/apt/apt.conf.d/docker-clean</code>, which Debian and Ubuntu images ship), and no step of this build removes it: the cache mount stays empty.")
		case "keepcache":
			return template.HTML(fmt.Sprintf("%s deletes downloaded packages after an install unless keepcache is on, and this build does not turn it on: the cache mount stays empty.", m))
		case "keeppackages":
			return template.HTML("zypper keeps downloaded packages only for repositories with keeppackages on, and this build does not turn it on: the cache mount stays empty.")
		}
		if strings.HasPrefix(p.Setting, "-") || strings.ToUpper(p.Setting) == p.Setting {
			return template.HTML(fmt.Sprintf("%s switches the cache of %s off, so the cache mount is not used.", code(p.Setting), m))
		}
		return template.HTML(fmt.Sprintf("%s empties the cache mount in the same step.", code(p.Setting)))
	}
	return ""
}

// orderVerdict is the line under the verdict of a rebuild that installed
// dependencies again only because of the Dockerfile's order: how much of
// this build's time that took, drawn against the whole.
func orderVerdict(os []store.OrderHint, b *history.Build) template.HTML {
	var took time.Duration
	var steps []string
	var managers []string
	for _, o := range os {
		took += o.Took
		at := ""
		if len(o.Lines) > 0 {
			at = fmt.Sprintf(" (line %d)", o.Lines[0].Start)
		}
		steps = append(steps, code(o.Name)+at)
		for _, m := range o.Managers {
			if !contains(managers, managerName(m)) {
				managers = append(managers, managerName(m))
			}
		}
	}
	o := os[0]
	copyAt := ""
	if len(o.CopyLines) > 0 {
		copyAt = fmt.Sprintf(" (line %d)", o.CopyLines[0].Start)
	}
	var files []string
	for _, x := range os {
		for _, f := range x.Files {
			if !contains(files, f) {
				files = append(files, f)
			}
		}
	}
	total := time.Duration(0)
	if !b.Completed.IsZero() {
		total = b.Completed.Sub(b.Started)
	}
	share := "<b>" + duration(took) + "</b>"
	meter := ""
	if total > took && total > 0 {
		share = fmt.Sprintf("<b>%s of %s</b>", duration(took), duration(total))
		pct := float64(took) / float64(total) * 100
		meter = fmt.Sprintf(`<svg class="ometer" viewBox="0 0 100 8" preserveAspectRatio="none" role="img" aria-label="%s of %s installed the dependencies again"><rect class="r" width="100" height="8" rx="2"/><rect class="o" width="%.1f" height="8" rx="2"/></svg>`,
			duration(took), duration(total), pct)
	}
	others := ""
	if len(o.Others) > 0 {
		others = " (the step also runs " + code(o.Others[0]) + ", which needs the code)"
	}
	return template.HTML(fmt.Sprintf(`%s went to installing the dependencies again%s, and none of the files they come from changed (%s). %s comes after %s%s, so a change to any file of the build context runs %s again.%s<a class="link" href="#spT">Install the dependencies before %s</a>`,
		share, others, filesText(files), strings.Join(steps, ", "), code(o.CopyName), copyAt, e(strings.Join(managers, " and ")), meter, e(o.CopyName)))
}

// fixedVerdict is the line under the verdict of a rebuild whose
// dependencies came from the cache, where an earlier build of the image
// installed them again: what a change cost before and now.
func fixedVerdict(f *store.OrderFixed) template.HTML {
	now := ""
	if f.ThisBuild > 0 {
		now = fmt.Sprintf(" This build took <b>%s</b>;", duration(f.ThisBuild))
	}
	before := fmt.Sprintf(` <a class="link" href="/builds/%d">build %d</a>, the last that installed again, took <b>%s</b>, %s of it installing the dependencies.`,
		f.Before, f.Before, duration(f.Took), duration(f.Wasted))
	var names []string
	for _, m := range f.Managers {
		names = append(names, managerName(m))
	}
	at := ""
	if f.Line > 0 {
		at = fmt.Sprintf(" (line %d)", f.Line)
	}
	return template.HTML(fmt.Sprintf(`<span class="chip passed">order fixed</span> <b>The dependencies came from the cache:</b> %s installs them before the code is copied now%s.%s%s`,
		e(strings.Join(names, " and ")), at, now, before))
}
