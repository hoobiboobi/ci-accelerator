package web

import (
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

func TestHighlight(t *testing.T) {
	for _, c := range []struct {
		line      string
		continued bool
		want      string
	}{
		{"FROM golang:1.27 AS build", false, `<span class="syn-i">FROM</span> <span class="syn-img">golang:1.27</span><span class="syn-k"> AS </span><span class="syn-s">build</span>`},
		{"FROM --platform=$BUILDPLATFORM golang:1.27", false, `<span class="syn-i">FROM</span> <span class="syn-f">--platform</span>=$BUILDPLATFORM <span class="syn-img">golang:1.27</span>`},
		{`ARG V="<b>"`, false, `<span class="syn-i">ARG</span> <span class="syn-v">V</span>=&#34;&lt;b&gt;&#34;`},
		{"RUN --mount=type=cache,target=/root/.cache go build", false, `<span class="syn-i">RUN</span> <span class="syn-f">--mount</span>=type=cache,target=/root/.cache go build`},
		{"ENV PATH=$HOME/bin:${PATH}", false, `<span class="syn-i">ENV</span> <span class="syn-v">PATH</span>=<span class="syn-v">$HOME</span>/bin:<span class="syn-v">${PATH}</span>`},
		{"# a <b>comment</b>", false, `<span class="syn-c"># a &lt;b&gt;comment&lt;/b&gt;</span>`},
		{"    && echo '<script>' $X", true, `    &amp;&amp; echo &#39;&lt;script&gt;&#39; <span class="syn-v">$X</span>`},
		{`RUN echo "<img src=x onerror=alert(1)>"`, false, `<span class="syn-i">RUN</span> echo &#34;&lt;img src=x onerror=alert(1)&gt;&#34;`},
		{"COPY --from=<x> a b", false, `<span class="syn-i">COPY</span> <span class="syn-f">--from</span>=&lt;x&gt; a b`},
		{"", false, ``},
		{"<not an instruction>", false, `&lt;not an instruction&gt;`},
	} {
		if got := string(highlight(c.line, c.continued)); got != c.want {
			t.Errorf("%q:\n got %s\nwant %s", c.line, got, c.want)
		}
	}
}

func at(s int) time.Time { return time.Date(2026, 10, 20, 12, 0, s, 0, time.UTC) }

// A build of target "app" from a two-stage Dockerfile with an unused
// stage between them.
func sample() (*history.Build, string) {
	text := strings.Join([]string{
		"FROM alpine AS Base",   // 1
		"RUN apk add \\",        // 2
		"    curl",              // 3
		"",                      // 4
		"FROM base AS tools",    // 5 (not used by app)
		"RUN apk add jq",        // 6
		"ENV A=1",               // 7
		"",                      // 8
		"FROM base AS app",      // 9
		"ENV MODE=prod",         // 10
		"COPY . .",              // 11
		"RUN make",              // 12
		"FROM busybox AS bin",   // 13 (only copied from: its image is a step)
		"FROM app",              // 14
		"COPY --from=bin /x /x", // 15
	}, "\n")
	b := &history.Build{Target: "app", Started: at(0), Completed: at(30), Steps: []history.Step{
		{Key: "[base 1/2]", Name: "FROM docker.io/library/alpine@sha256:abc", Lines: []history.LineRange{{Start: 1, End: 1}}, State: history.Built, Started: at(1), Completed: at(3)},
		{Key: "[base 2/2]", Name: "RUN apk add curl", Lines: []history.LineRange{{Start: 2, End: 3}}, State: history.Cached, Started: at(3), Completed: at(3)},
		{Key: "[app 1/2]", Name: "COPY . .", Lines: []history.LineRange{{Start: 11, End: 11}}, State: history.Built, Started: at(4), Completed: at(5)},
		{Key: "[app 2/2]", Name: "RUN make", Lines: []history.LineRange{{Start: 12, End: 12}}, State: history.Built, Started: at(5), Completed: at(25)},
		{Key: "[bin 1/1]", Name: "FROM docker.io/library/busybox@sha256:def", Lines: []history.LineRange{{Start: 13, End: 13}}, State: history.Cached, Started: at(1), Completed: at(1)},
		{Key: "[stage-4 1/1]", Name: "COPY --from=bin /x /x", Lines: []history.LineRange{{Start: 15, End: 15}}, State: history.Cached, Started: at(25), Completed: at(25)},
	}}
	return b, text
}

func TestDockerfileLines(t *testing.T) {
	b, text := sample()
	r := history.Reason{Kind: history.FilesChanged, Step: b.Steps[2]}
	views := steps(b, r, func(string) string { return "" }, nil)
	lines := dockerfile(text, views, b)
	if len(lines) != 15 {
		t.Fatalf("%d lines", len(lines))
	}
	type want struct {
		step              int
		state             string
		opens, first, dim bool
		meta, head        bool
	}
	for n, w := range map[int]want{
		1:  {step: 0, state: "image", opens: true, head: true},
		2:  {step: 1, state: "cached", opens: true},
		3:  {step: 1, state: "cached"}, // the continuation belongs to the same step
		4:  {step: -1},
		5:  {step: -1, dim: true, head: true},
		6:  {step: -1, dim: true},
		7:  {step: -1, dim: true}, // metadata, but the stage is unused: one tag only
		9:  {step: -1, head: true},
		10: {step: -1, meta: true},
		11: {step: 2, state: "built", opens: true, first: true},
		12: {step: 3, state: "built", opens: true},
		13: {step: 4, state: "image", opens: true, head: true}, // its stage has no other step, yet it ran
		14: {step: -1, head: true},
	} {
		l := lines[n-1]
		got := want{l.Step, l.State, l.Opens, l.First, l.Dim, l.Meta, l.Head}
		if got != w {
			t.Errorf("line %d: got %+v, want %+v", n, got, w)
		}
	}
	if strings.Contains(string(lines[2].HTML), "syn-i") || !strings.Contains(string(lines[1].HTML), "syn-i") {
		t.Errorf("a continuation line is not an instruction: %s / %s", lines[1].HTML, lines[2].HTML)
	}
	if lines[11].Showed != "20\u00a0s" || lines[2].Showed != "" {
		t.Errorf("durations: %q %q", lines[11].Showed, lines[2].Showed)
	}
}

func TestStages(t *testing.T) {
	b, _ := sample()
	b.Steps = append(b.Steps, history.Step{Key: "[stage-4 2/2]", Name: "COPY --from=other /y /y", State: history.Received, ReceivedFrom: "ref-x", Started: at(25), Completed: at(26)})
	views := steps(b, history.Reason{}, func(ref string) string { return "job " + ref }, nil)
	got := stagesOf(views)
	var names, states []string
	for _, g := range got {
		names, states = append(names, g.Name), append(states, g.State)
	}
	if strings.Join(names, ",") != "alpine,base,app,busybox,stage-4" || strings.Join(states, ",") != "image,cached,built,image,received" || got[4].From != "job ref-x" {
		t.Errorf("stages %v %v from %q", names, states, got[4].From)
	}
}

func TestVerdicts(t *testing.T) {
	b, _ := sample()
	step := history.Step{Key: "[app 1/2]", Name: "COPY <x> .", Line: 11}
	for _, c := range []struct {
		r      history.Reason
		by     string
		tone   string
		has    []string
		hasNot []string
	}{
		{history.Reason{Kind: history.FirstBuild}, "", "first", []string{"First build of target <code>app</code>"}, nil},
		{history.Reason{Kind: history.StepChanged, Step: step, Old: "COPY a .", New: "COPY <x> ."}, "", "built", []string{"<code>COPY &lt;x&gt; .</code> (Dockerfile line 11)"}, nil},
		{history.Reason{Kind: history.ArgChanged, Step: step, Arg: "V<", Old: "v-1", New: "v-2"}, "", "built", []string{"<code>V&lt;</code> changed", "<code>v-1</code> → <code>v-2</code>"}, nil},
		{history.Reason{Kind: history.BaseImageChanged, Step: step, Old: "python:3.13-slim", New: "python:3.14-slim"}, "", "built", []string{"base image", "<code>python:3.13-slim</code> → <code>python:3.14-slim</code>"}, nil},
		// Without a GitLab comparison the paths are candidates: no path is
		// named as changed (a witness proves THAT files changed, not which).
		{history.Reason{Kind: history.FilesChanged, Step: step, Paths: []string{"."}}, "lint<b>", "built",
			[]string{"a file it builds on changed", "builds on <code>.</code>", "Confirmed by <b>lint&lt;b&gt;</b>", "COPY . ."},
			[]string{"<code>.</code> changed"}},
		{history.Reason{Kind: history.FilesChanged, Step: step, Paths: []string{"src/"}}, "", "built",
			[]string{"a file it builds on changed", "builds on <code>src/</code>"},
			[]string{"<code>src/</code> changed", "Confirmed", "Tip"}},
		{history.Reason{Kind: history.OtherNode, Step: step, Old: "box-buildkit-4hxsv", New: "box-buildkit-rn6k5"}, "test", "node", []string{"node <b>4hxsv</b>", "ran on <b>rn6k5</b>", "<b>test</b>"}, nil},
		{history.Reason{Kind: history.OtherNode, Step: step, Old: "box-buildkit-4hxsv", New: "box-buildkit-rn6k5"}, "", "node", []string{"reads no files and did not change", "node <b>4hxsv</b>"}, []string{"in this pipeline"}},
		{history.Reason{Kind: history.CacheElsewhere, Step: step, Old: "box-buildkit-4hxsv", New: "box-buildkit-rn6k5"}, "", "unknown", []string{"Either its input changed", "node <b>4hxsv</b>"}, nil},
		// The previous build ran on this daemon: no node may be named.
		{history.Reason{Kind: history.CacheElsewhere, Step: step, Paths: []string{"api"}}, "", "unknown",
			[]string{"Either <code>api</code> changed", "cache no longer had the step"}, []string{"node <b>", "another node"}},
		{history.Reason{Kind: history.NotInCache, Step: step}, "", "unknown", []string{"not found in the cache"}, []string{"another node"}},
		{history.Reason{Kind: history.NotInCache, Step: step}, "test", "unknown",
			[]string{"<b>test</b> in this pipeline got it from cache", "cache no longer had it"}, []string{"another node"}},
		{history.Reason{Kind: history.ArgChanged, Step: step, Arg: "RUN_ID, TOKEN<b>"}, "", "built",
			[]string{"build arguments <code>RUN_ID</code>, <code>TOKEN&lt;b&gt;</code> changed", "did not change"}, []string{"→"}},
	} {
		v := verdictFor(b, c.r, c.by, 3, 0, nil)
		all := string(v.Title) + string(v.Detail) + string(v.Hint)
		if v.Tone != c.tone || v.Kicker == "" {
			t.Errorf("%s: tone %q kicker %q", c.r.Kind, v.Tone, v.Kicker)
		}
		for _, h := range c.has {
			if !strings.Contains(all, h) {
				t.Errorf("%s: %q not in %s", c.r.Kind, h, all)
			}
		}
		for _, h := range c.hasNot {
			if strings.Contains(all, h) {
				t.Errorf("%s: %q in %s", c.r.Kind, h, all)
			}
		}
		if strings.Contains(all, "<x>") || strings.Contains(all, "lint<b>") {
			t.Errorf("%s: unescaped data in %s", c.r.Kind, all)
		}
	}
	if v := verdictFor(b, history.Reason{Kind: history.StepChanged, Step: step, Old: "a", New: "b"}, "", 3, 0, nil); len(v.Diff) != 2 {
		t.Errorf("no diff: %+v", v)
	}
	// Settled by GitLab's comparison: the paths are verified diff hits and
	// may be named as changed.
	verifiedReason := history.Reason{Kind: history.FilesChanged, Step: step, Paths: []string{"deps.lock"}}
	v := withVia(verdictFor(b, verifiedReason, "", 3, 0, nil), verifiedReason, "gitlab 11111111..22222222")
	vall := string(v.Title) + string(v.Detail)
	for _, h := range []string{"<code>deps.lock</code> changed", "11111111..22222222", "(Dockerfile line 11)"} {
		if !strings.Contains(vall, h) {
			t.Errorf("verified files-changed: %q not in %s", h, vall)
		}
	}
	if strings.Contains(vall, "another node") || strings.Contains(vall, "cannot tell") {
		t.Errorf("verified same-daemon files-changed over-claims: %s", vall)
	}
	// An unresolved settling keeps the hedged wording.
	v = withVia(verdictFor(b, verifiedReason, "", 3, 0, nil), verifiedReason, "unresolved: boom")
	if !strings.Contains(string(v.Title), "a file it builds on changed") {
		t.Errorf("unresolved via must stay hedged: %s", v.Title)
	}
	failed := *b
	failed.Error = "exit code: 1 <b>"
	if v := verdictFor(&failed, history.Reason{}, "", 0, 0, nil); v.Tone != "failed" || !strings.Contains(string(v.Detail), "&lt;b&gt;") {
		t.Errorf("failed: %+v", v)
	}
	rec := []Received{{Stages: []string{"deps", "base"}, From: "build-api<i>", Wait: 3 * time.Second}}
	v = verdictFor(b, history.Reason{}, "", 0, 90*time.Second, rec)
	if v.Tone != "cached" || !strings.Contains(string(v.Detail), "1\u00a0min 30\u00a0s") ||
		!strings.Contains(string(v.Detail), "stages <b>deps, base</b> from <b>build-api&lt;i&gt;</b>") {
		t.Errorf("cached: %+v", v)
	}
	// Steps received from a build running at the same time are not counted
	// as cache hits.
	shared := *b
	shared.Steps = append([]history.Step(nil), b.Steps...)
	for i := range shared.Steps {
		shared.Steps[i].State = history.Received
	}
	v = verdictFor(&shared, history.Reason{}, "", 0, 0, rec)
	if t1 := string(v.Title); strings.Contains(t1, "0 steps") || !strings.Contains(t1, "running at the same time") || v.Kicker == "From cache" {
		t.Errorf("shared: %s / %s", v.Kicker, t1)
	}
}

func TestDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0.0 s", 400 * time.Millisecond: "0.4 s", 9500 * time.Millisecond: "9.5 s", 12 * time.Second: "12 s",
		76 * time.Second: "1 min 16 s", 3*time.Hour + 5*time.Minute: "3 h 5 min",
	} {
		if got := strings.ReplaceAll(duration(d), "\u00a0", " "); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

func TestUsageFacts(t *testing.T) {
	got := usageFacts(history.Usage{CPU: 9 * time.Second, Throttled: time.Second, ReadBytes: 2_000_000, WriteBytes: 0}, 3*time.Second)
	if len(got) != 3 || got[0].K != "CPU time" || !strings.Contains(got[0].V, "3.0 cores busy on average") ||
		got[1].K != "Held back by a CPU limit" || got[2].V != "read 2 MiB, wrote 0 B" {
		t.Errorf("%+v", got)
	}
	if got := usageFacts(history.Usage{}, time.Second); len(got) != 0 {
		t.Errorf("a step without usage: %+v", got)
	}
}
