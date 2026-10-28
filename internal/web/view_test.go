package web

import (
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/route"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
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
	fs := history.Step{Key: "[web 2/3]", Name: "RUN make && <false>"}
	if v := verdictFor(&failed, history.Reason{Kind: history.StepChanged, Step: fs, Old: "RUN make", New: fs.Name}, "", 0, 0, nil); v.Tone != "failed" ||
		!strings.Contains(string(v.Title), "a step this build changed") || !strings.Contains(string(v.Title), "&lt;false&gt;") || len(v.Diff) != 2 || v.Diff[0] != "RUN make" {
		t.Errorf("failed at a changed step: %+v", v)
	}
	if v := verdictFor(&failed, history.Reason{Kind: history.NewStep, Step: fs}, "", 0, 0, nil); !strings.Contains(string(v.Title), "a step new in this build") || v.Diff != nil {
		t.Errorf("failed at a new step: %+v", v)
	}
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
		got[1].K != "Held back by a CPU limit" || got[2].K != "Read from disk while it ran" || got[2].V != "1.9 MiB" {
		t.Errorf("%+v", got)
	}
	if got := usageFacts(history.Usage{}, time.Second); len(got) != 0 {
		t.Errorf("a step without usage: %+v", got)
	}
}

// A build away from its image's cache says why the router put it there,
// for every decision the router can make.
func TestNodeHintForEveryRouteDecision(t *testing.T) {
	if h := string(nodeHint(nil)); !strings.Contains(h, "build router") || strings.Contains(h, "planned") {
		t.Errorf("without the router: %q", h)
	}
	for _, r := range []route.Reason{route.LocalWarm, route.ZoneWarm, route.ZoneHash, route.CrossZone, route.UnknownLocal, route.UnknownHash} {
		d := &store.Decision{Reason: r, Node: "node-1"}
		if h := nodeHint(d); h == "" || h == nodeHint(nil) {
			t.Errorf("%s: %q", r, h)
		}
		if txt := routeText(*d); strings.Contains(txt, "("+string(r)+")") {
			t.Errorf("%s shown as a code: %q", r, txt)
		}
	}
}

// The usual time is the median of finished, successful builds of the same
// target and kind; the numbers are the ones a person can recount from the
// builds list (the case seen live: rebuilt api builds of 10.9, 5.5 and
// 6.5 s are usually 6.5 s).
func TestUsualTimes(t *testing.T) {
	at := time.Date(2026, 10, 27, 23, 0, 0, 0, time.UTC)
	row := func(target string, built int, d time.Duration, err string) store.BuildRow {
		return store.BuildRow{Target: target, Built: built, Started: at, Completed: at.Add(d), Error: err}
	}
	bs := []store.BuildRow{
		row("api", 6, 10912*time.Millisecond, ""), row("api", 3, 5507*time.Millisecond, ""), row("api", 5, 6541*time.Millisecond, ""),
		row("api", 0, 984*time.Millisecond, ""), row("api", 0, 336*time.Millisecond, ""),
		row("api", 4, time.Minute, "failed"),   // failed: not usual
		{Target: "api", Built: 2, Started: at}, // running
		row("web", 0, 2*time.Second, ""),
	}
	u := usualTimes(bs)["api"]
	if u.Rebuilt != 6541*time.Millisecond || u.RebuiltN != 3 || u.Cached != 336*time.Millisecond || u.CachedN != 2 || u.RebuiltP95 != 0 {
		t.Errorf("api: %+v", u)
	}
	if w := usualTimes(bs)["web"]; w.Cached != 2*time.Second || w.CachedN != 1 || w.RebuiltN != 0 {
		t.Errorf("web: %+v", w)
	}
}

// The Nodes page sums up the routers' decisions exactly, by why.
func TestRoutedText(t *testing.T) {
	n, warm, text := routed(map[string]int{"local-warm": 5, "zone-warm": 4, "zone-hash": 2, "unknown-local": 1, "cross-zone": 1})
	if n != 13 || warm != 9 || text != "13 — 9 to the project's cache, 2 by hash: no cache held the project, 1 from another zone, 1 job not known" {
		t.Errorf("%d %d %q", n, warm, text)
	}
	if n, _, text := routed(nil); n != 0 || text != "" {
		t.Errorf("no decisions: %d %q", n, text)
	}
}

// A build's parts on the time axis are the time that passed, not the sum
// of its steps: two steps running side by side for 4 s each took 5 s.
func TestSegmentsAreWallTime(t *testing.T) {
	t0 := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	step := func(name string, st history.StepState, from, to float64) history.Step {
		return history.Step{Name: name, State: st, Started: at(from), Completed: at(to)}
	}
	b := &history.Build{Started: at(0), Completed: at(9), Steps: []history.Step{
		step("FROM docker.io/library/busybox:1.37", history.Built, 0.5, 1),
		step("RUN a", history.Built, 1, 5), step("RUN b", history.Built, 2, 6), // side by side
		step("RUN c", history.Built, 7, 8), // after a gap
		step("RUN d", history.Received, 1, 3),
		step("RUN e", history.Cached, 3, 3)}}
	got := map[string]Segment{}
	for _, s := range segments(b) {
		got[s.Class] = s
	}
	want := map[string]time.Duration{"xfer": 500 * time.Millisecond, "image": 500 * time.Millisecond, "received": 2 * time.Second,
		"built": 6 * time.Second, "push": time.Second}
	for class, d := range want {
		if got[class].D != d {
			t.Errorf("%s: %v, want %v", class, got[class].D, d)
		}
	}
	if s := got["built"].Spans; len(s) != 2 || !s[0].From.Equal(at(1)) || !s[0].To.Equal(at(6)) || !s[1].From.Equal(at(7)) {
		t.Errorf("built spans: %+v", s)
	}
	for _, s := range got {
		for _, sp := range s.Spans {
			if sp.From.Before(b.Started) || sp.To.After(b.Completed) {
				t.Errorf("%s outside the build: %+v", s.Class, sp)
			}
		}
	}
}

func TestParallelText(t *testing.T) {
	t0 := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	st := func(name string, from, to float64) history.Step {
		return history.Step{Name: name, State: history.Built, Started: at(from), Completed: at(to)}
	}
	b := &history.Build{Steps: []history.Step{st("FROM x", 0, 3), st("RUN a", 1, 5), st("RUN b", 2, 6), st("RUN c", 7, 8)}}
	if got := parallelText(b); got != "9.0\u00a0s of step time in 6.0\u00a0s: 1.5 steps at a time on average" {
		t.Errorf("%q", got)
	}
	if got := parallelText(&history.Build{Steps: []history.Step{st("RUN a", 1, 5), st("RUN c", 7, 8)}}); got != "" {
		t.Errorf("one after another: %q", got)
	}
}

func TestSignalHint(t *testing.T) {
	b := &history.Build{Error: `process "/bin/sh -c make" did not complete successfully: exit code: 137`}
	if v := verdictFor(b, history.Reason{}, "", 0, 0, nil); !strings.Contains(string(v.Hint), "SIGKILL") {
		t.Errorf("137: %q", v.Hint)
	}
	b.Error = `process "/bin/sh -c make" did not complete successfully: exit code: 1`
	if v := verdictFor(b, history.Reason{}, "", 0, 0, nil); v.Hint != "" {
		t.Errorf("exit code 1 has a hint: %q", v.Hint)
	}
	if signalHint("exit code: 143") == "" || signalHint("exit code: 1370") != "" {
		t.Error("143 not read, or 1370 read as 137")
	}
}

func TestBytesText(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 10 << 10: "10 KiB", 1_572_864: "1.5 MiB",
		10_433_331: "9.9 MiB", 10_438_574: "10 MiB", 300 << 20: "300 MiB", 3 << 30: "3.0 GiB", 2 << 40: "2.00 TiB"} {
		if got := bytesText(n); got != want {
			t.Errorf("%d: %q, want %q", n, got, want)
		}
	}
}

func TestP95FromTwentyRebuilds(t *testing.T) {
	at := time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC)
	var bs []store.BuildRow
	for i := 1; i <= 20; i++ {
		bs = append(bs, store.BuildRow{Target: "api", Built: 1, Started: at, Completed: at.Add(time.Duration(i) * time.Second)})
	}
	if u := usualTimes(bs[:19])["api"]; u.RebuiltP95 != 0 {
		t.Errorf("p95 from 19 rebuilds: %v", u.RebuiltP95)
	}
	if u := usualTimes(bs)["api"]; u.RebuiltP95 != 19*time.Second || u.Rebuilt != 10*time.Second {
		t.Errorf("20 rebuilds: %+v", u)
	}
}

// The time before a job's build, split by its pod's times, adds up to it
// and stays inside it although Kubernetes counts whole seconds.
func TestStartParts(t *testing.T) {
	start := time.Date(2026, 10, 28, 1, 3, 35, 400_000_000, time.UTC)
	build := start.Add(20 * time.Second)
	jt := jobTimes{Start: start, PodCreated: start.Truncate(time.Second), PodRunning: start.Truncate(time.Second).Add(14 * time.Second)}
	ps := startParts(jt, build)
	if len(ps) != 2 || ps[0].Class != "podwait" || !ps[0].From.Equal(start) || ps[len(ps)-1].To != build {
		t.Fatalf("parts %+v", ps)
	}
	var sum time.Duration
	for _, p := range ps {
		sum += p.To.Sub(p.From)
	}
	if sum != 20*time.Second {
		t.Errorf("parts add up to %v", sum)
	}
	if ps := startParts(jobTimes{Start: start}, build); len(ps) != 1 || ps[0].Label != "Runner & checkout" {
		t.Errorf("without pod times: %+v", ps)
	}
	if ps := startParts(jobTimes{Start: start}, start); ps != nil {
		t.Errorf("no time before the build: %+v", ps)
	}
}

func TestReadsAny(t *testing.T) {
	for _, c := range []struct {
		step string
		ps   []string
		want bool
	}{
		{"COPY services/api/ /src/api/", []string{"services/api/main.txt"}, true},
		{"COPY services/auth/ /src/auth/", []string{"deps.lock"}, false},
		{"COPY deps.lock /src/deps.lock", []string{"deps.lock"}, true},
		{"COPY . .", []string{"anything/at/all"}, true},
		{"COPY ./requirements.txt .", []string{"requirements.txt"}, true},
		{"COPY --chown=app:app api/ ./api", []string{"api/app.py"}, true},
		{"COPY --from=deps /a /a", []string{"a"}, false},
		{"ADD *.lock /src/", []string{"b.lock"}, true},
		{"RUN make", []string{"Makefile"}, false},
		{"COPY a.txt b.txt /dst/", []string{"b.txt"}, true},
	} {
		if got := readsAny(c.step, c.ps); got != c.want {
			t.Errorf("%q reads %v: %v", c.step, c.ps, got)
		}
	}
	steps := []history.Step{{Key: "[deps 1/2]", Name: "COPY deps.lock /src/deps.lock"}, {Key: "[deps 2/2]", Name: "RUN cp"}, {Key: "[auth 1/3]", Name: "COPY services/auth/ /src/auth/"}}
	df := "FROM busybox AS base\nFROM base AS deps\nCOPY deps.lock /src/deps.lock\nFROM deps AS auth\nCOPY services/auth/ /src/auth/\nFROM base AS other\nCOPY . .\n"
	parents := stageParents(df)
	if parents["auth"] != "deps" || parents["deps"] != "base" || parents["base"] != "" {
		t.Fatalf("stages %v", parents)
	}
	if r := firstReader(steps, "[auth 1/3]", []string{"deps.lock"}, parents); r == nil || r.Key != "[deps 1/2]" {
		t.Errorf("first reader %+v", r)
	}
	if r := firstReader(steps, "[deps 1/2]", []string{"deps.lock"}, parents); r != nil {
		t.Errorf("a reader after the step: %+v", r)
	}
	// A stage the step does not build on copies it too: not named.
	unrelated := append([]history.Step{{Key: "[other 1/1]", Name: "COPY . ."}}, steps[2])
	if r := firstReader(unrelated, "[auth 1/3]", []string{"deps.lock"}, parents); r != nil {
		t.Errorf("a step of an unrelated stage named: %+v", r)
	}
	if r := firstReader(steps, "[auth 1/3]", []string{"deps.lock"}, nil); r != nil {
		t.Errorf("named without the Dockerfile: %+v", r)
	}
}

// A rebuild on the same node under a replaced daemon says so, not that
// the job ran on another node.
func TestReplacedDaemonVerdict(t *testing.T) {
	r := history.Reason{Kind: history.OtherNode, Step: history.Step{Name: "COPY services/auth/ /src/auth/", Line: 20}, Old: "glab-buildkit-nqspg", New: "glab-buildkit-qbjt5"}
	v := replacedDaemon(Verdict{Title: "Rebuilt because this job ran on another build node."}, r, "same commit 0f0da36d", func(s string) string { return s }, "buildkit-worker", "glab-buildkit-qbjt5", false)
	if strings.Contains(string(v.Title), "another build node") || !strings.Contains(string(v.Title), "daemon on this node was replaced") ||
		!strings.Contains(string(v.Detail), "same commit") || !strings.Contains(string(v.Detail), "<b>buildkit-worker</b>") || !strings.Contains(string(v.Hint), "hostPath") {
		t.Errorf("%+v", v)
	}
}

// With the cache on the node's disk the new daemon has the old one's
// cache: the page must not say it went with the pod.
func TestReplacedDaemonWithAPersistentCache(t *testing.T) {
	r := history.Reason{Kind: history.OtherNode, Step: history.Step{Name: "RUN make"}, Old: "bk-a", New: "bk-b"}
	v := replacedDaemon(Verdict{}, r, "same commit 0f0da36d", func(s string) string { return s }, "node-1", "bk-b", true)
	if strings.Contains(string(v.Title), "cache with it") || strings.Contains(string(v.Detail), "starts empty") || strings.Contains(string(v.Hint), "hostPath") ||
		!strings.Contains(string(v.Detail), "same cache") {
		t.Errorf("%+v", v)
	}
}
