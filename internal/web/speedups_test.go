package web

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hoobiboobi/ci-accelerator/internal/hints"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

var speedLink = regexp.MustCompile(`href="/builds/(\d+)/steps/(\d+)#speed" data-step="\d+" data-dtab-open="speed"`)

// The fixtures' deps step runs npm ci under a cache mount on /cache, not
// on npm's cache: the build pages show it as a speed-up, in the card, on
// the step's row, on the Dockerfile tab and in the step's panel.
func TestSpeedups(t *testing.T) {
	ts, _, ids := console(t)
	shown := 0
	for ref, id := range ids {
		p := get(t, ts, fmt.Sprintf("/builds/%d", id))
		if p.status != 200 {
			t.Fatalf("%s: %d", ref, p.status)
		}
		if !strings.Contains(p.body, "Speed-ups for this Dockerfile") {
			continue
		}
		shown++
		for _, want := range []string{
			"Add a cache mount for npm", "<span class=\"chip spk\">missing</span>",
			"No cache mount holds the cache of npm (<code>/root/.npm</code>)",
			// The step's own mount stays; npm's is added (the command is the
			// step's name as BuildKit logged it).
			"RUN --mount=type=cache,target=/cache \\\n    --mount=type=cache,target=/root/.npm \\\n    sleep 3 &amp;&amp; cp deps.txt",
			"in the last 30 days of this image&#39;s builds",
			`class="spmk" title="Speed-up 1: missing"`, `class="spmk" href="#speedup-1"`, ` sphl"`, `aside class="dfnotes"`,
		} {
			if !strings.Contains(p.body, want) {
				t.Errorf("%s: no %q", ref, want)
			}
		}
		m := speedLink.FindStringSubmatch(p.body)
		if m == nil {
			t.Fatalf("%s: no link to the step's speed-up", ref)
		}
		panel := get(t, ts, fmt.Sprintf("/builds/%s/steps/%s?part=panel", m[1], m[2]))
		for _, want := range []string{`data-dtab="speed"`, `data-dpane="speed"`, `data-dpane="out"`, "Keep in mind", "Replace line ",
			"Mount the npm cache, never node_modules", "BuildKit starts them empty for a build with --no-cache"} {
			if !strings.Contains(panel.body, want) {
				t.Errorf("%s panel: no %q", ref, want)
			}
		}
		panel.safe(t)
		p.safe(t)
	}
	if shown == 0 {
		t.Fatal("no build page shows a speed-up")
	}
	// A step of a build without a hint has no tabs in its panel.
	for _, id := range ids {
		p := get(t, ts, fmt.Sprintf("/builds/%d/steps/0?part=panel", id))
		if strings.Contains(p.body, "data-dtab") {
			t.Errorf("build %d: tabs on a step without a speed-up", id)
		}
		break
	}
}

func TestSpeedupTexts(t *testing.T) {
	h := store.StepHints{Step: 3, Key: "[4/6]", Name: "RUN apt-get install -y curl", Lines: []history.LineRange{{Start: 6, End: 8}},
		Hints: []hints.Hint{{Manager: "apt", Problems: []hints.Problem{{Kind: hints.Disabled, Setting: "docker-clean"},
			{Kind: hints.NotLocked, Mount: "/var/cache/apt"}}, Caches: []hints.Cache{{Label: "packages", Path: "/var/cache/apt", Locked: true}}}},
		Fragment: "RUN a\nRUN b \\\n  c \\\n  d"}
	v := speedupOf(2, h)
	if !v.Broken || v.Chip != "does not work" || v.LineText != "lines 6–8" || v.FragLines != 4 || v.Replace != "lines 6–8" {
		t.Errorf("view: %+v", v)
	}
	if string(v.Title) != "The cache mount for apt does not work" || len(v.Why) != 2 || !strings.Contains(string(v.Why[0]), "docker-clean") {
		t.Errorf("texts: %q %q", v.Title, v.Why)
	}
	if v.Figures != "The image's builds did not run this step in the last 30 days: it came from the cache." {
		t.Errorf("figures: %q", v.Figures)
	}
	h.Hints[0].Problems = []hints.Problem{{Kind: hints.NotLocked, Mount: "/var/cache/apt"}}
	if v := speedupOf(1, h); string(v.Title) != "Lock the cache mount for apt" {
		t.Errorf("title: %q", v.Title)
	}
	// Names from the build are escaped.
	x := hints.Hint{Manager: "npm", Problems: []hints.Problem{{Kind: hints.WrongPath, Mount: "/<b>", Cache: "cache"}}, Caches: []hints.Cache{{Label: "cache", Path: "/x&y"}}}
	if got := string(problemText(x, x.Problems[0])); strings.Contains(got, "<b>") || !strings.Contains(got, "/x&amp;y") {
		t.Errorf("not escaped: %s", got)
	}
}
