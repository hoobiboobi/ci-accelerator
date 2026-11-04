package web

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/hints"
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

// speedups turns a build's stored hints into what the pages show.
func speedups(hs []store.StepHints) []Speedup {
	out := make([]Speedup, 0, len(hs))
	for i, h := range hs {
		out = append(out, speedupOf(i+1, h))
	}
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
