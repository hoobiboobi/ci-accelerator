package history

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/solver/pb"
	digest "github.com/opencontainers/go-digest"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The bounds a build's record is read with, as the constants say; written
// out so that a change of a bound is a change of this test too.
const (
	wantMaxSteps        = 2000
	wantMaxStepName     = 4 << 10
	wantMaxStepError    = 16 << 10
	wantMaxDockerfile   = 1 << 20
	wantMaxVertices     = 50000
	wantMaxWarnings     = 1000
	wantMaxLiveVertices = 4 * wantMaxSteps
)

// A frontend may send a warning whose range has no start or no end
// (BuildKit passes it on as it is): it is read, not a panic that stops the
// controller at every start.
func TestWarningRangeWithoutEnds(t *testing.T) {
	short := []byte("JSONArgsRecommended: JSON arguments recommended (line 3)")
	w := func(r ...*pb.Range) *controlapi.VertexWarning {
		return &controlapi.VertexWarning{Vertex: digest.FromString("v").String(), Short: short, Ranges: r}
	}
	pos := func(l int32) *pb.Position { return &pb.Position{Line: l} }
	blob := progressBlob(t, []*controlapi.StatusResponse{
		{Vertexes: []*controlapi.Vertex{{Digest: digest.FromString("v").String(), Name: "[1/1] RUN true"}}},
		{Warnings: []*controlapi.VertexWarning{w(&pb.Range{})}},
		{Warnings: []*controlapi.VertexWarning{w(&pb.Range{Start: pos(4)})}},
		{Warnings: []*controlapi.VertexWarning{w(&pb.Range{End: pos(9)}, &pb.Range{Start: pos(6), End: pos(7)})}},
	})
	progress, err := DecodeProgress(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	rec := &controlapi.BuildHistoryRecord{Ref: "w", CreatedAt: timestamppb.New(t0)}
	b, err := NewBuild(rec, progress, nil, nil, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Warnings) != 1 {
		t.Fatalf("warnings %+v", b.Warnings)
	}
	// Directly: each shape alone.
	for _, tc := range []struct {
		r          []*pb.Range
		line, last int
	}{
		{[]*pb.Range{{}}, 0, 0},
		{[]*pb.Range{{Start: pos(4)}}, 4, 4},
		{[]*pb.Range{{End: pos(9)}, {Start: pos(6), End: pos(7)}}, 6, 7},
	} {
		ws := warnings([]*client.SolveStatus{{Warnings: []*client.VertexWarning{{Short: short, Range: tc.r}}}})
		if len(ws) != 1 || ws[0].Line != tc.line || ws[0].EndLine != tc.last {
			t.Errorf("ranges %v: %+v, want lines %d-%d", tc.r, ws, tc.line, tc.last)
		}
	}
}

// A provenance without a build definition (no digest mapping) followed by
// one of another platform that has one merges, without a panic.
func TestMergeProvenanceFirstWithoutMapping(t *testing.T) {
	empty, err := ParseProvenance([]byte(`{}`), []byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	full, err := ParseProvenance(gitProvenance(t, commitA, addSum, gitOpts{}), []byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	m := mergeProvenance([]*Provenance{empty, full})
	if len(m.DigestMapping) != len(full.DigestMapping) {
		t.Errorf("merged mapping %v", m.DigestMapping)
	}
}

// A build's steps, names and errors are bounded: a client can name and
// count them at will, and the store keeps what a build keeps.
func TestBuildIsBounded(t *testing.T) {
	var vs []*client.Vertex
	at := t0.Add(time.Second)
	for i := range wantMaxSteps + 500 {
		vs = append(vs, &client.Vertex{Digest: digest.FromString(fmt.Sprint(i)), Name: fmt.Sprintf("[%d/%d] RUN step %d", i+1, wantMaxSteps+500, i), Started: &at, Completed: &at})
	}
	long := &client.Vertex{Digest: digest.FromString("long"), Name: "[x 1/1] RUN " + strings.Repeat("echo a; ", 4<<10), Started: &at, Completed: &at,
		Error: strings.Repeat("error text ", 20<<10)}
	vs = append([]*client.Vertex{long}, vs...)
	docker := strings.Repeat("RUN echo a line of a long Dockerfile\n", (wantMaxDockerfile/37)+1000)
	rec := &controlapi.BuildHistoryRecord{Ref: "big", CreatedAt: timestamppb.New(t0), CompletedAt: timestamppb.New(t0.Add(time.Minute)),
		Error:         nil,
		FrontendAttrs: map[string]string{"target": strings.Repeat("t", 64<<10)}}
	b, err := NewBuild(rec, []*client.SolveStatus{{Vertexes: vs}}, nil, nil, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Steps) > wantMaxSteps || !b.Truncated {
		t.Errorf("%d steps kept of %d, truncated %v", len(b.Steps), len(vs), b.Truncated)
	}
	var s Step
	for _, x := range b.Steps {
		if x.Key == "[x 1/1]" {
			s = x
		}
	}
	if s.Key == "" || len(s.Name) > wantMaxStepName+len("…") || len(s.Error) > wantMaxStepError+len("…") {
		t.Errorf("step %q: name %d bytes, error %d bytes", s.Key, len(s.Name), len(s.Error))
	}
	if len(b.Target) > 4<<10+len("…") {
		t.Errorf("target of %d bytes", len(b.Target))
	}
	b.errSrc = &ErrorSource{Name: "Dockerfile", Text: docker}
	if _, text := b.Dockerfile(); len(text) > wantMaxDockerfile || !strings.HasSuffix(text, "\n") {
		t.Errorf("Dockerfile of %d bytes", len(text))
	}
}

// What DecodeProgress keeps of a blob besides its logs is bounded: each
// vertex once, a bounded number of them and of warnings, each warning
// without the Dockerfile BuildKit sends with it.
func TestDecodedProgressIsBounded(t *testing.T) {
	var msgs []*controlapi.StatusResponse
	ts := timestamppb.New(t0)
	const n = wantMaxVertices + 5000
	for i := 0; i < n; i += 1000 {
		var vs []*controlapi.Vertex
		for j := i; j < min(i+1000, n); j++ {
			vs = append(vs, &controlapi.Vertex{Digest: digest.FromString(fmt.Sprint(j)).String(), Name: fmt.Sprintf("[%d/%d] RUN x", j+1, n), Started: ts, Completed: ts})
		}
		msgs = append(msgs, &controlapi.StatusResponse{Vertexes: vs})
	}
	// One vertex reported in many frames is kept once.
	for range 100 {
		msgs = append(msgs, &controlapi.StatusResponse{Vertexes: []*controlapi.Vertex{{Digest: digest.FromString("0").String(), Name: "[1/1] RUN x", Started: ts}}},
			&controlapi.StatusResponse{Statuses: []*controlapi.VertexStatus{{ID: "transferring context:", Current: 5}}})
	}
	var ws []*controlapi.VertexWarning
	for i := range wantMaxWarnings * 3 {
		ws = append(ws, &controlapi.VertexWarning{Short: fmt.Appendf(nil, "RuleName: message %d (line 1)", i),
			Info: &pb.SourceInfo{Filename: "Dockerfile", Data: bytes.Repeat([]byte("FROM x\n"), 1000)}})
	}
	msgs = append(msgs, &controlapi.StatusResponse{Warnings: ws})
	progress, err := DecodeProgress(bytes.NewReader(progressBlob(t, msgs)))
	if err != nil {
		t.Fatal(err)
	}
	entries, distinct, warns, statuses := 0, map[digest.Digest]bool{}, 0, 0
	for _, st := range progress {
		entries += len(st.Vertexes)
		statuses += len(st.Statuses)
		for _, v := range st.Vertexes {
			distinct[v.Digest] = true
		}
		for _, w := range st.Warnings {
			warns++
			if w.SourceInfo != nil && len(w.SourceInfo.Data) > 0 {
				t.Fatal("a warning keeps the Dockerfile")
			}
		}
	}
	if len(distinct) > wantMaxVertices+1 || entries != len(distinct) || warns > wantMaxWarnings || statuses > 1 {
		t.Errorf("kept %d vertex entries of %d vertices, %d warnings, %d statuses", entries, len(distinct), warns, statuses)
	}
	rec := &controlapi.BuildHistoryRecord{Ref: "many", CreatedAt: timestamppb.New(t0)}
	b, err := NewBuild(rec, progress, nil, nil, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !b.Truncated || b.ContextBytes != 5 {
		t.Errorf("truncated %v, context %d bytes", b.Truncated, b.ContextBytes)
	}
}

// A running build keeps a bounded number of steps, each name bounded, and
// its steps are made once per update, not at every poll of its page.
func TestLiveIsBounded(t *testing.T) {
	l := NewLive(nil)
	var vs []*client.Vertex
	for i := range wantMaxLiveVertices + 1000 {
		vs = append(vs, vtx(fmt.Sprintf("[%d/%d] RUN x", i+1, wantMaxLiveVertices+1000), lat(1), lat(2), false, ""))
	}
	vs = append(vs, vtx("[long 1/1] RUN "+strings.Repeat("y", 64<<10), lat(1), nil, false, ""))
	l.Add(&client.SolveStatus{Vertexes: vs})
	a := l.Steps()
	if len(a) > wantMaxLiveVertices {
		t.Errorf("%d steps kept", len(a))
	}
	for _, s := range a {
		if len(s.Name) > wantMaxStepName+len("…") {
			t.Fatalf("a name of %d bytes", len(s.Name))
		}
	}
	if b := l.Steps(); len(b) == 0 || &a[0] != &b[0] {
		t.Error("the steps were made again without an update")
	}
	l.Add(&client.SolveStatus{Logs: []*client.VertexLog{logOf("[1/9000] RUN x", 1.5, "line\n")}})
	if c := l.Steps(); &a[0] == &c[0] {
		t.Error("an update kept the old steps")
	}
}

// A long log that prints the same token on every line shrinks under
// masking by more than the margin: no part of a token the cut went through
// is shown, at the tail's start nor at the head's end.
func TestCutLogShowsNoPartOfASecretWhenMaskingShrinksIt(t *testing.T) {
	tok := "glpat-AbCdEfGhIjKlMnOpQrSt"
	secret := "s3cr3tVALUEabcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJ"
	args := map[string]string{"GIT_TOKEN": secret}
	// Any six characters of the secret after the prefix that shows by
	// itself.
	leaks := func(out, s string, from int) string {
		for i := from; i+6 <= len(s); i++ {
			if strings.Contains(out, s[i:i+6]) {
				return s[i : i+6]
			}
		}
		return ""
	}
	for off := 0; off < 46; off += 3 {
		for _, c := range []struct {
			name, line, secret string
			args               map[string]string
			skip               int // the prefix that shows by itself
			last               string
		}{
			{"token", "PRIVATE-TOKEN: " + tok + " ok\n", tok, nil, len("glpat-"), "PRIVATE-TOKEN: [masked] ok\n"},
			{"build argument", "fetch https://oauth2:" + secret + "@git.example/x.git\n", secret, args, 0, "fetch https://oauth2:[masked]@git.example/x.git\n"},
			{"head", "glpat-" + strings.Repeat("Z", 60) + "\n", "glpat-" + strings.Repeat("Z", 60), nil, len("glpat-"), "[masked]\n"},
		} {
			var l logBuffer
			l.Write([]byte(strings.Repeat("x", off) + "\n"))
			for range 9000 {
				l.Write([]byte(c.line))
			}
			l.trim()
			out := l.output(c.args)
			if s := leaks(out, c.secret, c.skip); s != "" {
				t.Fatalf("%s, offset %d: %q shown", c.name, off, s)
			}
			if !strings.Contains(out, "bytes cut") || !strings.HasSuffix(out, c.last) {
				t.Fatalf("%s, offset %d: output %q … %q", c.name, off, out[:min(len(out), 60)], out[max(0, len(out)-60):])
			}
		}
	}
}

// A line longer than the tail's bound, with a masked token before a secret
// the bound cuts: the part of the secret left by the cut is not shown.
func TestLiveTailCutSecretAfterMaskedText(t *testing.T) {
	secret := "s3cr3tVALUEabcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJ"
	args := map[string]string{"GIT_TOKEN": secret}
	for pad := 1500; pad < 1600; pad += 7 {
		for _, c := range []struct {
			line, secret string
			args         map[string]string
		}{
			{"glpat-" + strings.Repeat("A", pad-7) + " " + secret + " end\n", secret, args},
			{"glpat-" + strings.Repeat("A", pad) + " https://deploy:Sup3rS3cretPassw0rd-0123456789-abcdefghij@registry.example/v2/ end\n", "Sup3rS3cret", nil},
		} {
			tl := &tail{}
			tl.write([]byte(c.line), t0, c.args)
			got := tl.snapshot(time.Time{}, c.args)[0].Text
			if strings.Contains(got, c.secret[:8]) {
				t.Fatalf("pad %d: %q", pad, got)
			}
		}
	}
	// A long line without a secret still shows its start.
	tl := &tail{}
	tl.write([]byte("word "+strings.Repeat("abc ", 1000)+"\n"), t0, nil)
	if got := tl.snapshot(time.Time{}, nil)[0].Text; len(got) < maxTailLine || !strings.HasPrefix(got, "word abc") {
		t.Errorf("long line: %q", got)
	}
}

// Terminal colours are not shown as text, in a running step's tail or in
// a step's output; a token with colours inside it is masked.
func TestColoursAreNotShown(t *testing.T) {
	line := "\x1b[1m\x1b[32m   Compiling\x1b[0m serde v1.0 \x1b]8;;https://x.example\x07link\x1b]8;;\x07"
	tl := &tail{}
	tl.write([]byte(line+"\n"), t0, nil)
	if got := tl.snapshot(time.Time{}, nil)[0].Text; got != "   Compiling serde v1.0 link" {
		t.Errorf("tail %q", got)
	}
	var l logBuffer
	l.Write([]byte(line + "\nglpat-\x1b[01;31mAbCdEfGhIj\x1b[mKlMnOpQrSt\n"))
	if got := l.output(nil); got != "   Compiling serde v1.0 link\n[masked]\n" {
		t.Errorf("output %q", got)
	}
}

// Credential shapes that curl -v, docker login, npm and pip print are
// masked, and a secret build argument's value in its encoded forms.
func TestMaskCommonCredentialShapes(t *testing.T) {
	for in, secret := range map[string]string{
		"token eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4ifQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c": "SflKxwRJSMeK",
		"> Authorization: Basic ZGVwbG95OlN1cDNyUzNjcmV0UGFzc3cwcmQ=":                                                                           "ZGVwbG95",
		"> PRIVATE-TOKEN: 3xAmpleOldStyleTok3n":                                                                                                 "3xAmple",
		"> JOB-TOKEN: 3xAmpleOldStyleTok3n64chars":                                                                                              "3xAmple",
		"registration token GR1348941AbCdEfGhIjKlMnOpQrSt":                                                                                      "AbCdEfGhIj",
		"AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY":                                                                        "wJalrXUtn",
		"//registry.npmjs.org/:_authToken=npm_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789":                                                             "AbCdEfGhIj",
		"pypi-AgEIcHlwaS5vcmcCJGFiY2RlZmdoLWFiY2QtYWJjZC1hYmNkLWFiY2RlZmdoaWprbAACKlszLCJh":                                                     "CJGFiY2Rl",
		`{"auths":{"registry.example":{"auth":"ZGVwbG95OlN1cDNyUzNjcmV0UGFzc3cwcmQ="}}}`:                                                        "ZGVwbG95",
		"git clone https://Sup3rS3cretPassw0rdToken@gitlab.example/g/p.git":                                                                     "Sup3rS3cret",
		"curl https://api.example/x?private_token=3xAmpleOldStyleTok3n&a=1":                                                                     "3xAmple",
		"mysql --password=Sup3rS3cretPassw0rd -u root":                                                                                          "Sup3rS3cret",
		"key sk_live_" + "AbCdEfGhIjKlMnOpQrStUvWx":                                                                                             "AbCdEfGhIj",
		"key AIzaSyAbCdEfGhIjKlMnOpQrStUvWxYz0123456":                                                                                           "AbCdEfGhIj",
		"vault hvs.AbCdEfGhIjKlMnOpQrStUvWxYz012345":                                                                                            "AbCdEfGhIj",
		"hook https://hooks.slack.com/services/T000/B000/AbCdEfGhIjKlMnOp":                                                                      "AbCdEfGhIj",
	} {
		if got := MaskSecrets(in); strings.Contains(got, secret) {
			t.Errorf("MaskSecrets(%q) = %q", in, got)
		}
	}
	args := map[string]string{"DEPLOY_PASSWORD": "pa ss/wo+rd&x=\"1\""}
	for _, form := range []string{"pa+ss%2Fwo%2Brd%26x%3D%221%22", "pa%20ss%2Fwo+rd&x=%221%22", "cGEgc3Mvd28rcmQmeD0iMSI=", `pa ss/wo+rd&x=\"1\"`} {
		if got := mask("sent "+form+" end", args); strings.Contains(got, form) {
			t.Errorf("encoded form %q shown: %q", form, got)
		}
	}
	// Not secrets: a variable, a Dockerfile's secret mount, plain words.
	for _, in := range []string{
		"ENV NODE_AUTH_TOKEN=$NPM_TOKEN",
		"RUN --mount=type=secret,id=npm_token,target=/root/.npmrc npm ci",
		"ARG GITHUB_TOKEN",
		"the token is read from a file",
	} {
		if got := MaskSecrets(in); got != in {
			t.Errorf("MaskSecrets(%q) = %q", in, got)
		}
	}
}

// The paths of a "fetched from the network" reason hide a secret build
// argument's value in the address's path, as the step's name does; an
// address that does not parse keeps no user information.
func TestRemoteSourcePathsAreMasked(t *testing.T) {
	prov := `{"buildDefinition":{"internalParameters":{"buildConfig":{
		"llbDefinition":[
		 {"id":"step0","op":{"Op":{"source":{"identifier":"https://deploy:PWsecret@dl.example.com/get/PATHsecret123/pkg.tgz?sig=SIGsecret"}}}},
		 {"id":"step1","inputs":["step0:0"],"op":{"Op":{"exec":{"meta":{"args":["sh","-c","tar xf /pkg.tgz"]}}}}}],
		"digestMapping":{"sha256:aa":"step0","sha256:bb":"step1"}}}}}`
	p, err := ParseProvenance([]byte(prov), []byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	paths := strings.Join(p.remoteSources("step1", map[string]string{"DOWNLOAD_TOKEN": "PATHsecret123"}), " ")
	if strings.Contains(paths, "secret") || !strings.HasPrefix(paths, "https://dl.example.com/get/") {
		t.Errorf("paths %q", paths)
	}
	for _, s := range []string{"https://user:FALLBACKsecret@host.example.com/a%zzb", "https://user:FALLBACKsecret@host.example.com:port/x?q=1"} {
		if got := safeURL(s); strings.Contains(got, "secret") || !strings.HasPrefix(got, "https://host.example.com") {
			t.Errorf("safeURL(%q) = %q", s, got)
		}
	}
}

// What the store keeps of a build's operations holds no secret value: not
// a secret argument's value in a derived variable, nor a token in the
// command line, nor a password in a source's address; the context option
// is masked too. The identities stay those of the normalised operations.
func TestStoredOperationsKeepNoSecret(t *testing.T) {
	ops := []string{
		`{"Op":{"source":{"identifier":"https://deploy:SRCsecretPW@dl.example.com/x.tgz","attrs":{"http.filename":"x.tgz"}}}}`,
		`{"Op":{"exec":{"meta":{"args":["sh","-c","echo glpat-AbCdEfGhIjKlMnOpQrSt; npm ci"],"env":["NPM_TOKEN=ARGsecretVALUE","DERIVED_URL=https://x:ARGsecretVALUE@npm.example.com/","PATH=/usr/bin"]}}}}`,
	}
	prov := fmt.Sprintf(`{"buildDefinition":{"externalParameters":{"request":{"args":{"build-arg:NPM_TOKEN":"ARGsecretVALUE"}}},"internalParameters":{"buildConfig":{
		"llbDefinition":[{"id":"step0","op":%s},{"id":"step1","inputs":["step0:0"],"op":%s}],
		"digestMapping":{"sha256:aa":"step0","sha256:bb":"step1"}}}}}`, ops[0], ops[1])
	key := []byte("k")
	p, err := ParseProvenance([]byte(prov), key)
	if err != nil {
		t.Fatal(err)
	}
	for id, s := range p.Steps {
		if op := string(s.Op); strings.Contains(op, "secret") || strings.Contains(op, "AbCdEfGh") {
			t.Errorf("%s kept as %s", id, op)
		}
	}
	if !strings.Contains(string(p.Steps["step1"].Op), "PATH=/usr/bin") || !strings.Contains(string(p.Steps["step1"].Op), "npm ci") {
		t.Errorf("the rest of the operation is lost: %s", p.Steps["step1"].Op)
	}
	q := &Provenance{Steps: map[string]ProvStep{
		"step0": {ID: "step0", Op: normalizeOp(json.RawMessage(ops[0]), key, gitContext{})},
		"step1": {ID: "step1", Inputs: []string{"step0"}, Op: normalizeOp(json.RawMessage(ops[1]), key, gitContext{})},
	}, Identity: map[string]string{}}
	for id := range q.Steps {
		if v, _ := q.identity(id, map[string]bool{}); v != p.Identity[id] {
			t.Errorf("%s: identity %s, want %s", id, p.Identity[id], v)
		}
	}
	rec := &controlapi.BuildHistoryRecord{Ref: "ctx", FrontendAttrs: map[string]string{ContextOpt: "https://oauth2:CTXsecretTOKEN@gitlab.example.com/x/y"}}
	b, err := NewBuild(rec, nil, nil, nil, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.Context, "secret") {
		t.Errorf("context %q", b.Context)
	}
}

// The structure of a progress blob is the client's: no frame it can send
// makes reading the record panic.
func FuzzProgressRecord(f *testing.F) {
	f.Add("[1/1] RUN true", "JSONArgsRecommended: x (line 1)", int32(1), int32(2), true, true, "boom")
	f.Add("[a 1/2] COPY . .", "Rule: y", int32(0), int32(0), false, true, "")
	f.Add("[internal] load", "Rule: z (line 3)", int32(5), int32(1), true, false, "")
	f.Fuzz(func(t *testing.T, name, short string, start, end int32, hasStart, hasEnd bool, errText string) {
		r := &pb.Range{}
		if hasStart {
			r.Start = &pb.Position{Line: start}
		}
		if hasEnd {
			r.End = &pb.Position{Line: end}
		}
		d := digest.FromString(name).String()
		ts := timestamppb.New(t0)
		blob := progressBlob(t, []*controlapi.StatusResponse{
			{Vertexes: []*controlapi.Vertex{{Digest: d, Name: name, Started: ts, Completed: ts, Error: errText}}},
			{Warnings: []*controlapi.VertexWarning{{Vertex: d, Short: []byte(short), Ranges: []*pb.Range{r}, Info: &pb.SourceInfo{Filename: name}}}},
			{Logs: []*controlapi.VertexLog{{Vertex: d, Stream: 1, Msg: []byte(errText + "\n" + name)}}},
		})
		progress, err := DecodeProgress(bytes.NewReader(blob))
		if err != nil {
			return // not valid protobuf (a name that is not UTF-8): refused, which is fine
		}
		rec := &controlapi.BuildHistoryRecord{Ref: "f", CreatedAt: ts, FrontendAttrs: map[string]string{"build-arg:A_TOKEN": name, ContextOpt: short}}
		if _, err := NewBuild(rec, progress, nil, nil, nil, Config{}); err != nil {
			t.Fatal(err)
		}
		l := NewLive(rec.FrontendAttrs)
		l.Add(progress[0])
		l.Steps()
	})
}
