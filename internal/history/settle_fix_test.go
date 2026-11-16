package history

import (
	"bytes"
	"strings"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
	digest "github.com/opencontainers/go-digest"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func settleBuild(ref, daemon string, pipeline uint64, steps ...Step) *Build {
	return &Build{Ref: ref, Daemon: daemon, Context: ".", Steps: steps,
		Attribution: Attribution{State: Attributed, Instance: "abcdef", Pipeline: pipeline, Job: pipeline*10 + uint64(len(ref))}}
}

var settleStep = Step{Key: "[app 1/2]", Name: "COPY common/ ./common", Identity: "id-common"}

func withState(s Step, st StepState) Step { s.State = st; return s }

// Two pipelines of one commit run at once (a merge request's and its
// branch's). The commit changed the files. The merge request's job
// executed the step on node-c a moment before the branch pipeline's
// witness found it in node-c's cache: that hit is the other pipeline's
// product, no proof that the files did not change.
func TestSettleWitnessFedByAnotherPipeline(t *testing.T) {
	elsewhere := Reason{Kind: CacheElsewhere, Step: withState(settleStep, Built), Old: "node-a", New: "node-b", Paths: []string{"common"}}
	cur := settleBuild("cur", "node-b", 7, withState(settleStep, Built))
	witness := settleBuild("witness", "node-c", 7, withState(settleStep, Cached))
	mr := settleBuild("mr", "node-c", 8, withState(settleStep, Built))
	got := Settle([]*Build{cur, witness, mr}, []Reason{elsewhere, {}, elsewhere})
	if got[0].Kind != CacheElsewhere || got[0].By != "" {
		t.Errorf("settled %s by %q, want cache-elsewhere for GitLab's comparison", got[0].Kind, got[0].By)
	}
	// Without the other pipeline's execution the witness proves it.
	if got := Settle([]*Build{cur, witness}, []Reason{elsewhere, {}}); got[0].Kind != OtherNode || got[0].By != "witness" {
		t.Errorf("control: %s by %q", got[0].Kind, got[0].By)
	}
}

// A FilesChanged that Settle made from a sibling is decided again once the
// sibling is decided otherwise (GitLab's comparison found its files
// unchanged: the cache was evicted), instead of standing on a disproved
// witness.
func TestSettleReopensWhenTheWitnessIsDecidedOtherwise(t *testing.T) {
	j1 := settleBuild("j1", "node-a", 9, withState(settleStep, Built))
	j2 := settleBuild("j2", "node-b", 9, withState(settleStep, Built))
	raw1 := Reason{Kind: CacheElsewhere, Step: withState(settleStep, Built), Paths: []string{"common"}}
	raw2 := Reason{Kind: CacheElsewhere, Step: withState(settleStep, Built), Old: "node-a", New: "node-b", Paths: []string{"common"}}
	first := Settle([]*Build{j1, j2}, []Reason{raw1, raw2})
	if first[1].Kind != FilesChanged || first[1].By != "j1" {
		t.Fatalf("control: %s by %q", first[1].Kind, first[1].By)
	}
	decided := first[0]
	decided.Kind = NotInCache
	again := Settle([]*Build{j1, j2}, []Reason{decided, first[1]})
	if again[1].Kind != CacheElsewhere || again[1].By != "" || again[1].Old != "node-a" {
		t.Errorf("after the witness was decided: %s by %q (old %q)", again[1].Kind, again[1].By, again[1].Old)
	}
	// While the witness stands, the verdict stands.
	if same := Settle([]*Build{j1, j2}, first); same[1].Kind != FilesChanged || same[1].By != "j1" {
		t.Errorf("witness unchanged: %s by %q", same[1].Kind, same[1].By)
	}
}

// Two siblings that both rebuilt the step on a daemon other than their
// previous build's settle nothing between them: "the cache was on another
// node" is never read as "the files changed".
func TestSettleCrossDaemonSiblingsProveNothing(t *testing.T) {
	a := settleBuild("a", "node-b", 7, withState(settleStep, Built))
	b := settleBuild("b", "node-c", 7, withState(settleStep, Built))
	ra := Reason{Kind: CacheElsewhere, Step: withState(settleStep, Built), Old: "node-a", New: "node-b"}
	rb := Reason{Kind: CacheElsewhere, Step: withState(settleStep, Built), Old: "node-a", New: "node-c"}
	for i, r := range Settle([]*Build{a, b}, []Reason{ra, rb}) {
		if r.Kind != CacheElsewhere || r.By != "" {
			t.Errorf("build %d: %s by %q", i, r.Kind, r.By)
		}
	}
}

// The value of a variable named like a secret never shows in a reason:
// not in an ENV change, nor in a build argument found in the step's text.
func TestReasonMasksSecretValues(t *testing.T) {
	op := func(env string) []byte {
		return []byte(`{"Op":{"exec":{"meta":{"args":["sh","-c","make"],"env":["` + env + `"]}}}}`)
	}
	b := func(env string) *Build {
		return &Build{prov: &Provenance{Steps: map[string]ProvStep{"s": {ID: "s", Op: op(env)}}, Identity: map[string]string{}}}
	}
	s := Step{Key: "[1/1]", Name: "RUN make", StepID: "s"}
	o, n, ok := envChange(b("DEPLOY_TOKEN=old-value-1"), b("DEPLOY_TOKEN=new-value-2"), s, s)
	if !ok || o != "ENV DEPLOY_TOKEN="+Masked || n != "ENV DEPLOY_TOKEN="+Masked {
		t.Errorf("env change %q -> %q (%v)", o, n, ok)
	}

	// The previous build listed the step without its definition: only the
	// texts compare, and a secret argument found in them is masked.
	prev := &Build{prov: &Provenance{Steps: map[string]ProvStep{}, Identity: map[string]string{}},
		BuildArgs: map[string]string{"API_TOKEN": "old-secret-value"},
		Steps:     []Step{{Key: "[1/1]", Name: "RUN fetch old-secret-value"}}}
	cur := &Build{prov: &Provenance{Steps: map[string]ProvStep{"step0": {ID: "step0", Op: op("A=b")}}, Identity: map[string]string{"step0": "id-new"}},
		BuildArgs: map[string]string{"API_TOKEN": "new-secret-value"},
		Steps:     []Step{{Key: "[1/1]", Name: "RUN fetch new-secret-value", StepID: "step0", Identity: "id-new", State: Built}}}
	r, ok := FirstRebuild(prev, cur)
	if !ok || r.Kind != ArgChanged || r.Arg != "API_TOKEN" || r.Old != Masked || r.New != Masked {
		t.Errorf("%+v", r)
	}
}

// A git context's repository moved (the project was renamed): the steps
// that read the context have other identities, but their operations and
// texts are the same. The reason names what changed, never "this step
// changed" between two equal texts.
func TestContextRepositoryMovedNamesTheSource(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	prev := gitBuild(t, "a", t0, commitA, "daemon-a", "copy", "run", "add")
	raw := gitProvenance(t, commitB, addSum, gitOpts{})
	raw = bytes.ReplaceAll(raw, []byte("gitlab.com/group/big.git"), []byte("gitlab.com/newgroup/big.git"))
	at := func(s float64) *time.Time {
		x := t0.Add(time.Hour).Add(time.Duration(s * float64(time.Second)))
		return &x
	}
	vs := []*client.Vertex{
		{Digest: digest.Digest(vertexDigest(commitB, "from", "")), Name: "[1/4] FROM docker.io/library/alpine:3.22", Started: at(0), Completed: at(0.5), Cached: true},
		{Digest: digest.Digest(vertexDigest(commitB, "copy", "")), Name: "[2/4] COPY . /src", Started: at(1), Completed: at(2)},
		{Digest: digest.Digest(vertexDigest(commitB, "run", "")), Name: "[3/4] RUN du -sh /src", Started: at(2), Completed: at(9)},
		{Digest: digest.Digest(vertexDigest(addSum, "add", "")), Name: "[4/4] ADD https://github.com/other/lib.git#v1 /lib", Started: at(9), Completed: at(10), Cached: true},
	}
	rec := &controlapi.BuildHistoryRecord{Ref: "b", CreatedAt: timestamppb.New(t0.Add(time.Hour)), CompletedAt: timestamppb.New(t0.Add(2 * time.Hour))}
	cur, err := NewBuild(rec, []*client.SolveStatus{{Vertexes: vs}}, [][]byte{raw}, nil, nil, Config{SecretKey: []byte("test")})
	if err != nil {
		t.Fatal(err)
	}
	cur.Daemon = "daemon-b"
	r, ok := FirstRebuild(prev, cur)
	if ok && r.Old == r.New {
		t.Fatalf("%s with the same text before and after: %q", r.Kind, r.Old)
	}
	if ok && (!strings.Contains(r.Old, "group/big.git") || !strings.Contains(r.New, "newgroup/big.git")) {
		t.Errorf("%s: %q -> %q", r.Kind, r.Old, r.New)
	}
}
