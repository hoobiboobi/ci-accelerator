package history

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
	digest "github.com/opencontainers/go-digest"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A build whose context the daemon fetched from git (the CI component's
// context-source git) records the repository as a git source pinned to the
// pipeline's commit, as BuildKit v0.34.1 wrote it on gitlab.com:
// "FROM alpine / COPY . /src / RUN du -sh /src", plus an ADD of another
// repository pinned by its own checksum.
const (
	commitA = "8c7c95c5d5dd6adf4182d398fcb87b970fdaf8dd"
	commitB = "2fa0dff0d54226d96776eb27cd5fd2ccc27d2e44"
	addSum  = "1111111111111111111111111111111111111111"
)

// gitOpts vary the provenance: config is the commit the provenance names
// as its config source (default: commit), local adds a named local
// context the RUN step reads.
type gitOpts struct {
	config string
	local  bool
	// localMain: the same build with the job's files as its context (the
	// component's context-source local): no config source, a local source.
	localMain bool
}

func gitProvenance(t *testing.T, commit, add string, o gitOpts) []byte {
	t.Helper()
	if o.config == "" {
		o.config = commit
	}
	repo := "https://gitlab.com/group/big.git"
	src := func(id string, attrs map[string]string) map[string]any {
		return map[string]any{"Op": map[string]any{"source": map[string]any{"identifier": id, "attrs": attrs}}}
	}
	at := func(line int) map[string]any {
		return map[string]any{"locations": []any{map[string]any{"ranges": []any{map[string]any{"start": map[string]int{"line": line}, "end": map[string]int{"line": line}}}}}}
	}
	copyOp := func(dest string) map[string]any {
		return map[string]any{"Op": map[string]any{"file": map[string]any{"actions": []any{map[string]any{
			"input": 0, "secondaryInput": 1, "output": 0,
			"Action": map[string]any{"copy": map[string]any{"src": "/", "dest": dest}}}}}}}
	}
	runInputs := []string{"step2:0"}
	if o.local {
		runInputs = append(runInputs, "step6:0")
	}
	p := map[string]any{
		"buildDefinition": map[string]any{
			"externalParameters": map[string]any{
				"configSource": map[string]any{"uri": repo + "#ci-accelerator", "digest": map[string]string{"sha1": o.config}},
			},
			"internalParameters": map[string]any{"buildConfig": map[string]any{
				"llbDefinition": []any{
					map[string]any{"id": "step0", "op": src("docker-image://docker.io/library/alpine:3.22", nil)},
					map[string]any{"id": "step1", "op": src("git://gitlab.com/group/big.git#ci-accelerator", map[string]string{
						"git.authheadersecret": "GIT_AUTH_HEADER", "git.authtokensecret": "GIT_AUTH_TOKEN",
						"git.checksum": commit, "git.fetchbycommit": "true", "git.fullurl": repo})},
					map[string]any{"id": "step2", "inputs": []string{"step0:0", "step1:0"}, "op": copyOp("/src")},
					map[string]any{"id": "step3", "inputs": runInputs, "op": map[string]any{"Op": map[string]any{"exec": map[string]any{"meta": map[string]any{"args": []string{"/bin/sh", "-c", "du -sh /src"}}}}}},
					map[string]any{"id": "step4", "op": src("git://github.com/other/lib.git#v1", map[string]string{"git.checksum": add, "git.fullurl": "https://github.com/other/lib.git"})},
					map[string]any{"id": "step5", "inputs": []string{"step3:0", "step4:0"}, "op": copyOp("/lib")},
				},
				"digestMapping": map[string]string{
					vertexDigest(commit, "from", ""): "step0",
					vertexDigest(commit, "copy", ""): "step2",
					vertexDigest(commit, "run", ""):  "step3",
					vertexDigest(add, "add", ""):     "step5",
				},
			}},
		},
		"runDetails": map[string]any{"metadata": map[string]any{"buildkit_metadata": map[string]any{"source": map[string]any{
			"locations": map[string]any{"step0": at(1), "step2": at(2), "step3": at(3), "step5": at(4)},
			"infos":     []any{map[string]any{"filename": "Dockerfile", "language": "Dockerfile", "data": []byte("FROM alpine:3.22\nCOPY . /src\nRUN du -sh /src\nADD https://github.com/other/lib.git#v1 /lib\n")}},
		}}}},
	}
	if o.localMain {
		bd := p["buildDefinition"].(map[string]any)
		delete(bd["externalParameters"].(map[string]any), "configSource")
		defs := bd["internalParameters"].(map[string]any)["buildConfig"].(map[string]any)["llbDefinition"].([]any)
		defs[1] = map[string]any{"id": "step1", "op": src("local://context", map[string]string{"local.sharedkeyhint": "context"})}
	}
	if o.local {
		bc := p["buildDefinition"].(map[string]any)["internalParameters"].(map[string]any)["buildConfig"].(map[string]any)
		bc["llbDefinition"] = append(bc["llbDefinition"].([]any),
			map[string]any{"id": "step6", "op": src("local://tools", map[string]string{"local.sharedkeyhint": "tools"})})
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// gitBuild is a build of that Dockerfile from commit on daemon; rebuilt
// lists the steps it executed ("copy", "run", "add"), the rest came from
// the cache.
func gitBuild(t *testing.T, ref string, t0 time.Time, commit, daemon string, rebuilt ...string) *Build {
	t.Helper()
	return gitBuildWith(t, ref, t0, commit, daemon, gitOpts{}, rebuilt...)
}

func gitBuildWith(t *testing.T, ref string, t0 time.Time, commit, daemon string, o gitOpts, rebuilt ...string) *Build {
	t.Helper()
	at := func(s float64) *time.Time { x := t0.Add(time.Duration(s * float64(time.Second))); return &x }
	built := map[string]bool{}
	for _, r := range rebuilt {
		built[r] = true
	}
	vs := []*client.Vertex{
		{Digest: digest.Digest(vertexDigest(commit, "from", "")), Name: "[1/4] FROM docker.io/library/alpine:3.22", Started: at(0), Completed: at(0.5), Cached: true},
		{Digest: digest.Digest(vertexDigest(commit, "copy", "")), Name: "[2/4] COPY . /src", Started: at(1), Completed: at(2), Cached: !built["copy"]},
		{Digest: digest.Digest(vertexDigest(commit, "run", "")), Name: "[3/4] RUN du -sh /src", Started: at(2), Completed: at(9), Cached: !built["run"]},
		{Digest: digest.Digest(vertexDigest(addSum, "add", "")), Name: "[4/4] ADD https://github.com/other/lib.git#v1 /lib", Started: at(9), Completed: at(10), Cached: !built["add"]},
	}
	rec := &controlapi.BuildHistoryRecord{Ref: ref, CreatedAt: timestamppb.New(t0), CompletedAt: timestamppb.New(t0.Add(time.Minute)),
		FrontendAttrs: map[string]string{"context": "https://gitlab.com/group/big.git?ref=ci-accelerator&checksum=" + commit + "&fetch-by-commit=true"}}
	b, err := NewBuild(rec, []*client.SolveStatus{{Vertexes: vs}}, [][]byte{gitProvenance(t, commit, addSum, o)}, nil, nil, Config{SecretKey: []byte("test")})
	if err != nil {
		t.Fatal(err)
	}
	b.Daemon = daemon
	return b
}

// The commit a git context was fetched from is the build context: kept as
// the build's ContextCommit, left out of the steps' identities like a
// local context's files, and what the steps after it depend on.
func TestGitContextIsTheBuildContext(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	a := gitBuild(t, "a", t0, commitA, "daemon-a", "copy", "run", "add")
	b := gitBuild(t, "b", t0.Add(time.Hour), commitB, "daemon-a", "copy", "run")
	if a.ContextCommit != commitA || b.ContextCommit != commitB {
		t.Fatalf("context commits %q, %q", a.ContextCommit, b.ContextCommit)
	}
	for _, id := range []string{"step1", "step2", "step3"} {
		if a.prov.Identity[id] != b.prov.Identity[id] {
			t.Errorf("%s: identity changed with the commit", id)
		}
	}
	if op := string(a.prov.Steps["step1"].Op); strings.Contains(op, commitA) || !strings.Contains(op, gitContextMark) {
		t.Errorf("context source kept as %s", op)
	}
	if !a.prov.contextDependent("step3") {
		t.Error("RUN after COPY . must depend on the git context")
	}
	if got := strings.Join(a.prov.contextSources("step3"), ","); got != "." {
		t.Errorf("contextSources = %q, want . (the whole context)", got)
	}
	if rs := a.prov.remoteSources("step3"); len(rs) != 0 {
		t.Errorf("the pinned context is not a remote source: %v", rs)
	}
	// Another repository's ADD is not the context, even pinned.
	if op := string(a.prov.Steps["step4"].Op); !strings.Contains(op, addSum) {
		t.Errorf("ADD source lost its checksum: %s", op)
	}
	if contextSource(a.prov.Steps["step4"].Op) {
		t.Error("ADD of another repository counted as the context")
	}

	// Rebuilt after a new commit on the same daemon: the context (files)
	// or the cache, never the Dockerfile; comparing the commits settles it.
	r, ok := FirstRebuild(a, b)
	if !ok || r.Step.Key != "[2/4]" || r.Kind != CacheElsewhere || r.Old != "" {
		t.Fatalf("new commit, same daemon: %+v", r)
	}
	// On another daemon: files or node, settled by comparing the commits.
	c := gitBuild(t, "c", t0.Add(2*time.Hour), commitB, "daemon-b", "copy", "run")
	if r, _ := FirstRebuild(a, c); r.Kind != CacheElsewhere || r.Step.Key != "[2/4]" || strings.Join(r.Paths, ",") != "." {
		t.Errorf("new commit, other daemon: %+v", r)
	}
	// The same commit again (a pipeline run twice): the context is the
	// same, so only the cache was missing.
	d := gitBuild(t, "d", t0.Add(3*time.Hour), commitA, "daemon-b", "copy", "run")
	if r, _ := FirstRebuild(a, d); r.Kind != OtherNode || r.Old != "daemon-a" || r.New != "daemon-b" {
		t.Errorf("same commit, other daemon: %+v", r)
	}
	e := gitBuild(t, "e", t0.Add(4*time.Hour), commitA, "daemon-a", "copy", "run")
	if r, _ := FirstRebuild(a, e); r.Kind != NotInCache {
		t.Errorf("same commit, same daemon: %+v", r)
	}
}

// A named local context next to the git one still feeds the step: the
// same commit does not make it the cache's alone.
func TestGitContextWithLocalContext(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	a := gitBuildWith(t, "a", t0, commitA, "daemon-a", gitOpts{local: true}, "copy", "run", "add")
	b := gitBuildWith(t, "b", t0.Add(time.Hour), commitA, "daemon-b", gitOpts{local: true}, "run")
	if r, _ := FirstRebuild(a, b); r.Kind != CacheElsewhere || r.Step.Key != "[3/4]" {
		t.Errorf("same commit, local context too: %+v, want cache-elsewhere at [3/4]", r)
	}
	// The build is not "the repository at a commit": no context commit, so
	// the store keeps the comparison of commits from proving it either.
	if a.ContextCommit != "" || b.ContextCommit != "" {
		t.Errorf("context commits %q, %q with a local context", a.ContextCommit, b.ContextCommit)
	}
	// The git source still stands for the context in the steps' identities.
	if a.prov.Identity["step2"] != gitBuild(t, "c", t0, commitB, "daemon-a").prov.Identity["step2"] {
		t.Error("the COPY's identity changed with the commit")
	}
}

// Only a config source that is a commit and that the build's own git
// source was fetched at makes a git context: a Dockerfile read from one
// repository with the files from the job, or a digest that is no commit,
// leaves every source as it was.
func TestGitContextNeedsItsSource(t *testing.T) {
	for name, o := range map[string]gitOpts{
		"config at another commit": {config: commitB},
	} {
		p, err := ParseProvenance(gitProvenance(t, commitA, addSum, o), nil)
		if err != nil {
			t.Fatal(err)
		}
		if p.ContextCommit != "" || !strings.Contains(string(p.Steps["step1"].Op), commitA) {
			t.Errorf("%s: context commit %q, source %s", name, p.ContextCommit, p.Steps["step1"].Op)
		}
	}
	for _, d := range []map[string]string{{"sha1": "8c7c95c"}, {"sha256": "x"}, {"md5": commitA}, nil} {
		if got := gitContextCommit("https://gitlab.com/g/p.git#main", d); got != "" {
			t.Errorf("gitContextCommit(%v) = %q", d, got)
		}
	}
	if got := gitContextCommit("", map[string]string{"sha1": commitA}); got != "" {
		t.Errorf("no config source: %q", got)
	}
	if got := gitContextCommit("https://gitlab.com/g/p.git#main", map[string]string{"sha1": commitA}); got != commitA {
		t.Errorf("sha1 commit: %q", got)
	}
}

// A failed build has no provenance; its commit comes from the request.
func TestContextURLCommit(t *testing.T) {
	for in, want := range map[string]string{
		"https://gitlab.com/g/p.git?ref=ci-accelerator&checksum=" + commitA + "&fetch-by-commit=true":  commitA,
		"https://gitlab-ci-token:x@gitlab.com/g/p.git?checksum=" + commitA + "&subdir=svc/api#ignored": commitA,
		"git@gitlab.com:g/p.git?checksum=" + commitA:                                                   commitA,
		"https://gitlab.com/g/p.git?ref=main":                                                          "",
		"https://gitlab.com/g/p.git?checksum=8c7c95c":                                                  "",
		"https://gitlab.com/g/p.git#" + commitA:                                                        "",
		"services/api":                                                                                 "",
		"":                                                                                             "",
	} {
		if got := contextURLCommit(in); got != want {
			t.Errorf("contextURLCommit(%q) = %q, want %q", in, got, want)
		}
	}
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	rec := &controlapi.BuildHistoryRecord{Ref: "f", CreatedAt: timestamppb.New(t0), CompletedAt: timestamppb.New(t0.Add(time.Minute)),
		FrontendAttrs: map[string]string{"context": "https://gitlab.com/g/p.git?ref=ci-accelerator&checksum=" + commitB}}
	b, err := NewBuild(rec, nil, nil, nil, nil, Config{SecretKey: []byte("test")})
	if err != nil {
		t.Fatal(err)
	}
	if b.ContextCommit != commitB {
		t.Errorf("failed build: context commit %q", b.ContextCommit)
	}
	rec.FrontendAttrs["context:tools"] = "local:tools"
	if b, err := NewBuild(rec, nil, nil, nil, nil, Config{SecretKey: []byte("test")}); err != nil || b.ContextCommit != "" {
		t.Errorf("failed build with a named local context: context commit %q, %v", b.ContextCommit, err)
	}
}

// normalizeOp is idempotent with and without the context commit: a stored
// (normalised) operation read again keeps its identity.
func FuzzNormalizeOpGitContext(f *testing.F) {
	f.Add(`{"Op":{"source":{"identifier":"git://gitlab.com/g/p.git#ci-accelerator:svc","attrs":{"git.checksum":"`+commitA+`","git.fullurl":"https://gitlab.com/g/p.git"}}}}`, commitA)
	f.Add(`{"Op":{"source":{"identifier":"git://github.com/o/l.git#v1","attrs":{"git.checksum":"`+addSum+`"}}}}`, commitA)
	f.Add(`{"Op":{"source":{"identifier":"local://context","attrs":{"local.sharedkeyhint":"context"}}}}`, "")
	f.Add(`{"Op":{"exec":{"meta":{"env":["API_TOKEN=x","A=b"]}}}}`, commitB)
	f.Fuzz(func(t *testing.T, op, commit string) {
		key := []byte("k")
		gc := gitContext{ID: "git://gitlab.com/g/p.git#ci-accelerator:svc", Commit: commit}
		once := normalizeOp(json.RawMessage(op), key, gc)
		if twice := normalizeOp(once, key, gc); string(twice) != string(once) {
			t.Fatalf("not idempotent:\n%s\n%s", once, twice)
		}
		if again := normalizeOp(once, key, gitContext{}); string(again) != string(once) {
			t.Fatalf("a stored op changes when read without the commit:\n%s\n%s", once, again)
		}
		var v opView
		if commit != "" && json.Unmarshal(once, &v) == nil && v.Op.Source != nil && v.Op.Source.Identifier == gc.ID && v.Op.Source.Attrs["git.checksum"] == commit {
			t.Fatalf("the context commit survived normalisation: %s", once)
		}
	})
}

// Whatever the request's context says, only a full commit id comes out.
func FuzzContextURLCommit(f *testing.F) {
	f.Add("https://gitlab.com/g/p.git?ref=ci-accelerator&checksum=" + commitA + "&fetch-by-commit=true")
	f.Add("git@gitlab.com:g/p.git?checksum=" + commitA)
	f.Add("services/api")
	f.Add("https://u:p@h/p.git?checksum=%zz")
	f.Fuzz(func(t *testing.T, s string) {
		if c := contextURLCommit(s); c != "" && !commitID.MatchString(c) {
			t.Fatalf("contextURLCommit(%q) = %q", s, c)
		}
	})
}

// The config source and the git source of the context name the same
// repository, reference and folder; another folder or reference is not
// the context.
func TestSameGitSource(t *testing.T) {
	for _, c := range []struct {
		id, uri string
		want    bool
	}{
		{"git://gitlab.com/g/p.git#main", "https://gitlab.com/g/p.git#main", true},
		{"git://gitlab.com/g/p.git#ci-accelerator:services/api", "https://gitlab.com/g/p.git#ci-accelerator:services/api", true},
		{"git://gitlab.com/g/p.git#ci-accelerator:./services/api/", "https://gitlab.com/g/p.git#ci-accelerator:services/api", true},
		{"git://h:8443/g/p.git#r:web app", "https://h:8443/g/p.git#r:web%20app", true},
		{"git://h/g/p.git#r:сервис", "http://h/g/p.git#r:%D1%81%D0%B5%D1%80%D0%B2%D0%B8%D1%81", true},
		{"git://h/g/p.git#r:a#b", "http://h/g/p.git#r:a%23b", true},
		{"git://h/g/p.git#r:a%20b", "http://h/g/p.git#r:a%20b", true},
		{"git://h/g/p.git#r:100%41", "http://h/g/p.git#r:100%41", true},
		{"git://h/g/p.git#r:a b", "http://h/g/p.git#r:a%2520b", false},
		{"git://gitlab.com/g/p.git#ci-accelerator:libs/common", "https://gitlab.com/g/p.git#ci-accelerator:services/api", false},
		{"git://gitlab.com/g/p.git#other", "https://gitlab.com/g/p.git#main", false},
		{"git://gitlab.com/g/q.git#main", "https://gitlab.com/g/p.git#main", false},
		{"git://gitlab.com/g/p.git#main", "", false},
	} {
		if got := sameGitSource(c.id, c.uri); got != c.want {
			t.Errorf("sameGitSource(%q, %q) = %v", c.id, c.uri, got)
		}
	}
}

// A named git context of another folder at the pipeline's commit is a
// source of its own: its commit stays in its operation, and it is not
// the build context.
func TestNamedGitContextAtTheSameCommit(t *testing.T) {
	raw := gitProvenance(t, commitA, addSum, gitOpts{})
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	bc := p["buildDefinition"].(map[string]any)["internalParameters"].(map[string]any)["buildConfig"].(map[string]any)
	// First in the definition, so that only the match on repository,
	// reference and folder can tell it from the context.
	bc["llbDefinition"] = append([]any{map[string]any{"id": "step7", "op": map[string]any{"Op": map[string]any{"source": map[string]any{
		"identifier": "git://gitlab.com/group/big.git#ci-accelerator:libs/common",
		"attrs":      map[string]string{"git.checksum": commitA, "git.fullurl": "https://gitlab.com/group/big.git"}}}}}}, bc["llbDefinition"].([]any)...)
	raw, _ = json.Marshal(p)
	prov, err := ParseProvenance(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if prov.ContextCommit != commitA || contextSource(prov.Steps["step7"].Op) || !strings.Contains(string(prov.Steps["step7"].Op), commitA) {
		t.Errorf("context %q; named source %s", prov.ContextCommit, prov.Steps["step7"].Op)
	}
	if !contextSource(prov.Steps["step1"].Op) {
		t.Error("the context's own source is no longer the context")
	}
}

// Switching an image from a local context to a git one: the steps that
// read the context have other identities (another source), so the first
// git build is never "cache elsewhere" against the last local one, and no
// comparison of commits decides it.
func TestSwitchFromLocalToGitContext(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	prev := gitBuildWith(t, "a", t0, commitA, "daemon-a", gitOpts{localMain: true}, "copy", "run", "add")
	if prev.ContextCommit != "" {
		t.Fatalf("a local context has a context commit %q", prev.ContextCommit)
	}
	for _, daemon := range []string{"daemon-a", "daemon-b"} {
		cur := gitBuild(t, "b", t0.Add(time.Hour), commitA, daemon, "copy", "run")
		r, ok := FirstRebuild(prev, cur)
		if !ok || r.Kind == CacheElsewhere || r.Kind == OtherNode || r.Kind == NotInCache {
			t.Errorf("%s: %+v, want the build's own change", daemon, r)
		}
	}
}
