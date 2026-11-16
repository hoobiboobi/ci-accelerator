package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// cssRules maps each selector of console.css (rules inside @media too) to
// its declarations; a selector written twice gets both bodies.
func cssRules(t *testing.T) (map[string]string, string) {
	t.Helper()
	css, err := fs.ReadFile(files, "static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	s := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(css), "")
	rules := map[string]string{}
	for _, m := range regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`).FindAllStringSubmatch(s, -1) {
		for _, sel := range strings.Split(m[1], ",") {
			sel = strings.Join(strings.Fields(sel), " ")
			rules[sel] += m[2] + ";"
		}
	}
	return rules, s
}

// A failed build has a second cue beside its colour in every compact
// chart: its colour is close to "change" and "farm" in both themes.
func TestFailedHasShapeCue(t *testing.T) {
	rules, _ := cssRules(t)
	for sel, want := range map[string]string{
		".strip .sq.st-failed":     "linear-gradient(to top right",
		".chart .dot.st-failed":    "box-shadow: 0 0 0 1.5px var(--panel), 0 0 0 3px var(--sc)",
		".weeks .wcol i.st-failed": "repeating-linear-gradient(45deg",
		".pg-tr .j.jw-failed":      "repeating-linear-gradient(90deg",
		".pg-tr .j.st-failed":      "repeating-linear-gradient(90deg",
		".pg-key i.jw-failed":      "repeating-linear-gradient(90deg",
	} {
		if !strings.Contains(rules[sel], want) {
			t.Errorf("%s: no %q in %q", sel, want, rules[sel])
		}
	}
	// The cross is drawn both ways, the weekly hatching crosses.
	if !strings.Contains(rules[".strip .sq.st-failed"], "linear-gradient(to bottom right") {
		t.Error(".strip .sq.st-failed: one stroke of the cross only")
	}
	if !strings.Contains(rules[".weeks .wcol i.st-failed"], "repeating-linear-gradient(135deg") {
		t.Error(".weeks .wcol i.st-failed: hatching one way only, as unclear")
	}
}

// Faint graphics and the current page or tab are marked by a shape in a
// colour with at least 3:1 against what is behind it (WCAG 1.4.11), in
// both themes.
func TestFaintGraphicsAndCurrentMarkers(t *testing.T) {
	rules, s := cssRules(t)
	for sel, want := range map[string]string{
		".c-runner":                                "border: 1px dashed var(--ink-3)",
		".pg-tr .j.jw-canceled":                    "box-shadow: inset 0 0 0 1px var(--ink-3)",
		".nav a[aria-current=\"page\"]":            "box-shadow: inset 3px 0 0 var(--accent)",
		".mclu-menu a.sbtn[aria-current=\"true\"]": "box-shadow: inset 2px 0 0 var(--accent)",
		".seg a[aria-current=\"true\"]":            "box-shadow: inset 0 -2px 0 var(--accent)",
		".seg a[aria-selected=\"true\"]":           "box-shadow: inset 0 -2px 0 var(--accent)",
		".seg button[aria-pressed=\"true\"]":       "box-shadow: inset 0 -2px 0 var(--accent)",
		".drtabs button[aria-selected=\"true\"]":   "box-shadow: inset 0 -2px 0 var(--accent)",
		".bbar a[aria-current=\"page\"]":           "box-shadow: inset 0 -2px 0 var(--accent)",
	} {
		if !strings.Contains(rules[sel], want) {
			t.Errorf("%s: no %q in %q", sel, want, rules[sel])
		}
	}
	if !strings.Contains(rules[".pg-tr .j.jw-canceled"], "background: transparent") {
		t.Error(".pg-tr .j.jw-canceled: not hollow")
	}
	tok := func(block, name string) string {
		m := regexp.MustCompile(`--` + name + `: (#[0-9a-fA-F]{6})`).FindStringSubmatch(block)
		if m == nil {
			t.Fatalf("no --%s in %q", name, block[:40])
		}
		return m[1]
	}
	for _, sel := range []string{":root {", `:root[data-theme="light"] {`} {
		i := strings.Index(s, sel)
		if i < 0 {
			t.Fatalf("no %s", sel)
		}
		block := s[i : i+strings.Index(s[i:], "}")]
		for _, p := range [][2]string{{"ink-3", "panel"}, {"accent", "side"}, {"accent", "panel"}, {"accent", "sel"}} {
			if r := contrast(tok(block, p[0]), tok(block, p[1])); r < 3 {
				t.Errorf("%s --%s on --%s: %.2f:1", sel, p[0], p[1], r)
			}
		}
	}
}
