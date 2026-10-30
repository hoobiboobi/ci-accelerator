package history

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/moby/buildkit/client"
	digest "github.com/opencontainers/go-digest"
)

var liveT0 = time.Date(2026, 10, 30, 11, 37, 0, 0, time.UTC)

func lat(s float64) *time.Time {
	t := liveT0.Add(time.Duration(s * float64(time.Second)))
	return &t
}

func vtx(name string, started, completed *time.Time, cached bool, err string) *client.Vertex {
	return &client.Vertex{Digest: digest.FromString(name), Name: name, Started: started, Completed: completed, Cached: cached, Error: err}
}

func logOf(name string, at float64, text string) *client.VertexLog {
	return &client.VertexLog{Vertex: digest.FromString(name), Stream: 1, Data: []byte(text), Timestamp: *lat(at)}
}

func TestLiveStepStates(t *testing.T) {
	l := NewLive(nil)
	l.Add(&client.SolveStatus{Vertexes: []*client.Vertex{
		vtx("[internal] load build definition from Dockerfile", lat(0), lat(0.1), false, ""),
		vtx("[toolchain 1/2] FROM docker.io/library/debian:bookworm", lat(0.2), lat(4.4), false, ""),
		vtx("[toolchain 2/2] RUN apt-get update", lat(4.4), lat(22), true, ""),
		vtx("[vmlinux 1/1] RUN make vmlinux", lat(39), lat(671), false, ""),
		vtx("[modules 1/1] RUN make modules", lat(671), nil, false, ""),
		vtx("[install 1/1] RUN make modules_install", nil, nil, false, ""),
	}})
	steps := l.Steps()
	got := map[string]string{}
	var order []string
	for _, s := range steps {
		got[s.Key] = s.State
		order = append(order, s.Key)
	}
	want := map[string]string{"[toolchain 1/2]": LiveBuilt, "[toolchain 2/2]": LiveCached, "[vmlinux 1/1]": LiveBuilt,
		"[modules 1/1]": LiveRunning, "[install 1/1]": LiveWaiting}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: state %q, want %q", k, got[k], w)
		}
	}
	if len(steps) != 5 {
		t.Fatalf("internal vertices are not steps: %v", order)
	}
	if order[len(order)-1] != "[install 1/1]" {
		t.Errorf("a step that has not started comes last: %v", order)
	}
	// A later update of the same vertex finishes it; another fails.
	l.Add(&client.SolveStatus{Vertexes: []*client.Vertex{
		vtx("[modules 1/1] RUN make modules", lat(671), lat(940), false, ""),
		vtx("[install 1/1] RUN make modules_install", lat(940), lat(941), false, "process \"/bin/sh -c make modules_install\" did not complete successfully: exit code: 2"),
	}})
	for _, s := range l.Steps() {
		switch s.Key {
		case "[modules 1/1]":
			if s.State != LiveBuilt || !s.Completed.Equal(*lat(940)) {
				t.Errorf("modules: %s until %v", s.State, s.Completed)
			}
		case "[install 1/1]":
			if s.State != LiveFailed || s.Error == "" {
				t.Errorf("install: %s %q", s.State, s.Error)
			}
		}
	}
	l.Add(&client.SolveStatus{Vertexes: []*client.Vertex{vtx("[pack 1/1] RUN tar", lat(941), lat(942), false, "context canceled")}})
	for _, s := range l.Steps() {
		if s.Key == "[pack 1/1]" && s.State != LiveCanceled {
			t.Errorf("a canceled step: %s", s.State)
		}
	}
}

// A step whose vertex is still running is running, even when another of
// its vertices (BuildKit splits a few steps in two) already completed.
func TestLiveStepWithTwoVertices(t *testing.T) {
	l := NewLive(nil)
	l.Add(&client.SolveStatus{Vertexes: []*client.Vertex{
		vtx("[app 2/3] COPY . .", lat(1), lat(2), false, ""),
		{Digest: digest.FromString("second"), Name: "[app 2/3] COPY . .", Started: lat(2)},
	}})
	s := l.Steps()
	if len(s) != 1 || s[0].State != LiveRunning || !s[0].Completed.IsZero() {
		t.Fatalf("steps %+v", s)
	}
}

func TestLiveTail(t *testing.T) {
	l := NewLive(map[string]string{"API_TOKEN": "s3cr3t-value", "JOBS": "32"})
	run := "[modules 1/1] RUN make -j32 modules"
	l.Add(&client.SolveStatus{Vertexes: []*client.Vertex{vtx(run, lat(10), nil, false, "")}})
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&b, "  CC [M]  drivers/x%02d.o\n", i)
	}
	// Written in pieces that cut lines.
	text := b.String()
	l.Add(&client.SolveStatus{Logs: []*client.VertexLog{logOf(run, 100, text[:37])}})
	l.Add(&client.SolveStatus{Logs: []*client.VertexLog{logOf(run, 110.6, text[37:])}})
	l.Add(&client.SolveStatus{Logs: []*client.VertexLog{logOf(run, 111, "token s3cr3t-value and glpat-abcdefghij0123456789xyz\nhalf a li")}})
	s := l.Steps()[0]
	if s.Lines != 21 {
		t.Errorf("lines %d, want 21", s.Lines)
	}
	if len(s.Tail) != TailLines {
		t.Fatalf("tail of %d lines, want %d", len(s.Tail), TailLines)
	}
	last := s.Tail[len(s.Tail)-1]
	if strings.Contains(last.Text, "s3cr3t") || strings.Contains(last.Text, "glpat-abc") || strings.Count(last.Text, Masked) != 2 {
		t.Errorf("not masked: %q", last.Text)
	}
	if s.Tail[0].Text != "  CC [M]  drivers/x14.o" {
		t.Errorf("first line of the tail %q", s.Tail[0].Text)
	}
	if got := s.Tail[len(s.Tail)-2]; got.Text != "  CC [M]  drivers/x20.o" || got.At != 100600*time.Millisecond {
		t.Errorf("line %q at %v (the piece that ended it came 100.6 s into the step)", got.Text, got.At)
	}
	for _, tl := range s.Tail {
		if strings.Contains(tl.Text, "half a li") {
			t.Errorf("a line still being printed shows: %q", tl.Text)
		}
	}
}

func TestLiveTailPrivateKey(t *testing.T) {
	l := NewLive(nil)
	run := "[deploy 1/1] RUN cat key"
	l.Add(&client.SolveStatus{Vertexes: []*client.Vertex{vtx(run, lat(0), nil, false, "")}})
	lines := "before\n-----BEGIN OPENSSH PRIVATE KEY-----\n"
	for i := 0; i < 12; i++ {
		lines += "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtz" + fmt.Sprint(i) + "\n"
	}
	lines += "-----END OPENSSH PRIVATE KEY-----\nafter\n"
	l.Add(&client.SolveStatus{Logs: []*client.VertexLog{logOf(run, 1, lines)}})
	s := l.Steps()[0]
	for i, tl := range s.Tail[:len(s.Tail)-1] {
		if tl.Text != Masked {
			t.Errorf("line %d of the key shows: %q", i, tl.Text)
		}
	}
	if s.Tail[len(s.Tail)-1].Text != "after" {
		t.Errorf("the line after the key: %q", s.Tail[len(s.Tail)-1].Text)
	}
}

// An armored PGP key ("PRIVATE KEY BLOCK") is a private key too.
func TestLiveTailPGPKey(t *testing.T) {
	l := NewLive(nil)
	run := "[deploy 1/1] RUN gpg --export-secret-keys --armor"
	l.Add(&client.SolveStatus{Vertexes: []*client.Vertex{vtx(run, lat(0), nil, false, "")}})
	l.Add(&client.SolveStatus{Logs: []*client.VertexLog{logOf(run, 1,
		"-----BEGIN PGP PRIVATE KEY BLOCK-----\n\nlQOYBGVjZXJ0aWZpY2F0ZQ\n=Xq4t\n-----END PGP PRIVATE KEY BLOCK-----\nafter\n")}})
	s := l.Steps()[0]
	for _, tl := range s.Tail {
		if tl.Text != Masked && tl.Text != "after" && tl.Text != "" {
			t.Errorf("a line of the key shows: %q", tl.Text)
		}
	}
}

func TestLiveTailProgressBar(t *testing.T) {
	l := NewLive(nil)
	run := "[source 2/2] RUN curl -o linux.tar.xz https://example.com/linux.tar.xz"
	l.Add(&client.SolveStatus{Vertexes: []*client.Vertex{vtx(run, lat(0), nil, false, "")}})
	l.Add(&client.SolveStatus{Logs: []*client.VertexLog{logOf(run, 1, "start\n 10%\r 20%\r 3")}})
	s := l.Steps()[0]
	if len(s.Tail) != 2 || s.Tail[1].Text != " 20%" {
		t.Fatalf("a progress bar shows its last whole drawing: %+v", s.Tail)
	}
	l.Add(&client.SolveStatus{Logs: []*client.VertexLog{logOf(run, 2, "0%\r100%\n")}})
	s = l.Steps()[0]
	if len(s.Tail) != 2 || s.Tail[1].Text != "100%" || s.Lines != 2 {
		t.Fatalf("the finished bar is one line: %+v (%d lines)", s.Tail, s.Lines)
	}
}

func TestLiveTailLongLine(t *testing.T) {
	l := NewLive(nil)
	run := "[a 1/1] RUN x"
	l.Add(&client.SolveStatus{Vertexes: []*client.Vertex{vtx(run, lat(0), nil, false, "")}})
	l.Add(&client.SolveStatus{Logs: []*client.VertexLog{logOf(run, 1, strings.Repeat("é", 5000)+"\n")}})
	got := l.Steps()[0].Tail[0].Text
	if len(got) > maxTailLine+len("…") || !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, "é") {
		t.Errorf("long line kept as %d bytes", len(got))
	}
}

func TestLiveMasksStepNames(t *testing.T) {
	l := NewLive(map[string]string{"NPM_TOKEN": "abcd1234efgh"})
	l.Add(&client.SolveStatus{Vertexes: []*client.Vertex{vtx("[deps 1/1] RUN npm ci --token abcd1234efgh", lat(0), nil, false, "")}})
	if n := l.Steps()[0].Name; strings.Contains(n, "abcd1234") {
		t.Errorf("name %q", n)
	}
}

func TestTraceParent(t *testing.T) {
	prefix := InstancePrefix("https://gitlab.com")
	known := map[string]bool{prefix: true}
	id := fmt.Sprintf("01%s%012x%012x", prefix, 3007867305, 17603024205)
	a := TraceParent(map[string]string{TraceOpt: "00-" + id + "-0000000419350c4d-01"}, known)
	if a.State != Attributed || a.Pipeline != 3007867305 || a.Job != 17603024205 || a.Instance != prefix {
		t.Fatalf("attribution %+v", a)
	}
	for name, v := range map[string]string{
		"none":             "",
		"short":            "00-" + id[:30] + "-0000000419350c4d-01",
		"no parts":         id,
		"unknown instance": "00-01abcdef" + id[8:] + "-0000000419350c4d-01",
	} {
		if a := TraceParent(map[string]string{TraceOpt: v}, known); a.State == Attributed {
			t.Errorf("%s: attributed %+v", name, a)
		}
	}
}

// A line redrawn inside a private key being printed stays hidden.
func TestLiveTailRedrawInKey(t *testing.T) {
	l := NewLive(nil)
	run := "[deploy 1/1] RUN cat key"
	l.Add(&client.SolveStatus{Vertexes: []*client.Vertex{vtx(run, lat(0), nil, false, "")}})
	l.Add(&client.SolveStatus{Logs: []*client.VertexLog{logOf(run, 1, "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\rMIIEpAIBAAKCAQEA\rMII")}})
	for _, tl := range l.Steps()[0].Tail {
		if tl.Text != Masked {
			t.Errorf("shows %q", tl.Text)
		}
	}
}

// A progress bar that redraws for minutes keeps showing its latest state
// (the kept text used to fill up and freeze at an early percentage).
func TestLiveTailLongRedraw(t *testing.T) {
	l := NewLive(nil)
	run := "[src 1/1] RUN curl -o big.tar https://example.com/big.tar"
	l.Add(&client.SolveStatus{Vertexes: []*client.Vertex{vtx(run, lat(0), nil, false, "")}})
	for p := 0; p <= 100; p++ {
		l.Add(&client.SolveStatus{Logs: []*client.VertexLog{logOf(run, 1, fmt.Sprintf("\rDownloading 123456789 bytes ....... %3d%%", p))}})
		if p == 99 {
			if tl := l.Steps()[0].Tail; len(tl) == 0 || !strings.HasSuffix(tl[len(tl)-1].Text, " 98%") {
				t.Errorf("while drawing: %q", tl)
			}
		}
	}
	l.Add(&client.SolveStatus{Logs: []*client.VertexLog{logOf(run, 1, "\n")}})
	if tl := l.Steps()[0].Tail; len(tl) != 1 || !strings.HasSuffix(tl[0].Text, "100%") {
		t.Errorf("after the line ended: %q", tl)
	}
}

// A secret in a redrawn line is masked whole in every drawing, however
// long the line has been redrawing.
func TestLiveTailSecretInRedraw(t *testing.T) {
	sec := "Zq8vK2mN7pL4xR9tW3yB6cD1"
	l := NewLive(map[string]string{"DEPLOY_TOKEN": sec})
	run := "[deploy 1/1] RUN curl"
	l.Add(&client.SolveStatus{Vertexes: []*client.Vertex{vtx(run, lat(0), nil, false, "")}})
	for _, c := range []string{"\r" + strings.Repeat("#", 1560), "\rcurl -H 'PRIVATE-TOKEN: " + sec + "' https://x 50%",
		"\rcurl -H 'PRIVATE-TOKEN: " + sec + "' https://x 100%"} {
		l.Add(&client.SolveStatus{Logs: []*client.VertexLog{logOf(run, 1, c)}})
		for _, tl := range l.Steps()[0].Tail {
			if strings.Contains(tl.Text, sec[:6]) {
				t.Fatalf("part of the secret shows: %q", tl.Text)
			}
		}
	}
	l.Add(&client.SolveStatus{Logs: []*client.VertexLog{logOf(run, 1, "\n")}})
	if tl := l.Steps()[0].Tail; len(tl) != 1 || !strings.Contains(tl[0].Text, "[masked]") || !strings.HasSuffix(tl[0].Text, "100%") {
		t.Errorf("after the line ended: %q", tl)
	}
}

// A secret build argument of several lines is masked in a tail that sees
// one line at a time.
func TestLiveTailMultiLineSecret(t *testing.T) {
	l := NewLive(map[string]string{"SECRET_ENV": "user=admin\npassword=hunter2hunter2\n"})
	run := "[app 1/1] RUN cat .env"
	l.Add(&client.SolveStatus{Vertexes: []*client.Vertex{vtx(run, lat(0), nil, false, "")}})
	l.Add(&client.SolveStatus{Logs: []*client.VertexLog{logOf(run, 1, "user=admin\npassword=hunter2hunter2\ndone\n")}})
	for _, tl := range l.Steps()[0].Tail {
		if strings.Contains(tl.Text, "hunter2") || strings.Contains(tl.Text, "user=admin") {
			t.Errorf("a line of the secret shows: %q", tl.Text)
		}
	}
}

// A drawing longer than the bound is cut; once whole it shows as an
// ellipsis, never as the piece that was kept (it may end in half a secret).
func TestLiveTailCutDrawing(t *testing.T) {
	sec := "Zq8vK2mN7pL4xR9tW3yB6cD1"
	l := NewLive(map[string]string{"DEPLOY_TOKEN": sec})
	run := "[deploy 1/1] RUN curl"
	l.Add(&client.SolveStatus{Vertexes: []*client.Vertex{vtx(run, lat(0), nil, false, "")}})
	long := strings.Repeat("#", 4*maxTailLine-10) + sec + " 1%"
	l.Add(&client.SolveStatus{Logs: []*client.VertexLog{logOf(run, 1, "\r"+long+"\rnext 2%")}})
	tl := l.Steps()[0].Tail
	if len(tl) != 1 || tl[0].Text != "…" {
		t.Errorf("the cut drawing shows as %q", tl)
	}
}

// A step received from another job waits for the steps of its own stage,
// not for a stage that runs beside it.
func TestWaitsByStage(t *testing.T) {
	t0 := time.Unix(1000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	steps := []Step{
		{Key: "[test 2/2]", Line: 3, State: Built, Started: at(1), Completed: at(300)},
		{Key: "[app 2/2]", Line: 7, State: Received, foreignStarted: at(-20), Completed: at(60)},
	}
	waits(t0, steps)
	if s := steps[1]; !s.Started.Equal(t0) || !s.Completed.Equal(at(60)) {
		t.Errorf("received step: +%s to +%s, want +0s to +60s", s.Started.Sub(t0), s.Completed.Sub(t0))
	}
	steps = []Step{
		{Key: "[app 1/2]", Line: 6, State: Built, Started: at(0), Completed: at(30)},
		{Key: "[app 2/2]", Line: 7, State: Received, foreignStarted: at(10), Completed: at(60)},
	}
	waits(t0, steps)
	if s := steps[1]; !s.Started.Equal(at(30)) {
		t.Errorf("within its stage: starts +%s, want +30s", s.Started.Sub(t0))
	}
}
