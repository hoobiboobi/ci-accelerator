package history

import (
	"regexp"
	"sort"
	"strings"

	"github.com/moby/buildkit/client"
)

// Warning is one of the Dockerfile's build checks that failed, as
// BuildKit's Dockerfile frontend reports it on every build (the official
// rules: https://docs.docker.com/reference/build-checks/). Nothing here is
// the console's own judgement.
type Warning struct {
	Rule    string `json:"rule"`              // "JSONArgsRecommended"
	Message string `json:"message"`           // what the check says about this line
	Detail  string `json:"detail,omitempty"`  // the rule's description
	URL     string `json:"url,omitempty"`     // the rule's documentation
	File    string `json:"file,omitempty"`    // the Dockerfile's name in the build
	Line    int    `json:"line,omitempty"`    // first line, 1-based; 0 unknown
	EndLine int    `json:"endLine,omitempty"` // last line
}

// ruleLine splits "Rule: message (line N)".
var ruleLine = regexp.MustCompile(`^([A-Za-z]+): (.*?)(?: \(line \d+\))?$`)

// warnings collects the build checks' warnings of a progress stream, once
// each, in line order.
func warnings(progress []*client.SolveStatus) []Warning {
	seen := map[string]bool{}
	var out []Warning
	for _, st := range progress {
		for _, w := range st.Warnings {
			short := strings.TrimSpace(string(w.Short))
			m := ruleLine.FindStringSubmatch(short)
			if m == nil {
				continue
			}
			x := Warning{Rule: m[1], Message: m[2], URL: w.URL}
			var detail []string
			for _, d := range w.Detail {
				detail = append(detail, strings.TrimSpace(string(d)))
			}
			x.Detail = strings.Join(detail, " ")
			if w.SourceInfo != nil {
				x.File = w.SourceInfo.Filename
			}
			for i, r := range w.Range {
				if r == nil {
					continue
				}
				if i == 0 || int(r.Start.Line) < x.Line {
					x.Line = int(r.Start.Line)
				}
				if int(r.End.Line) > x.EndLine {
					x.EndLine = int(r.End.Line)
				}
			}
			key := x.Rule + "\x00" + x.Message + "\x00" + x.File
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, x)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}
