package history

import (
	"fmt"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
	digest "github.com/opencontainers/go-digest"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// BuildKit pads a step's number to the width of the count: a Dockerfile
// of ten or more steps names them "[ 1/10]" to "[ 9/10]", then "[10/10]"
// (rustdesk's in the lab run, whose build showed no step at all).
func TestPaddedStepKeys(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	at := func(s int) *time.Time { x := t0.Add(time.Duration(s) * time.Second); return &x }
	var vs []*client.Vertex
	for i := 1; i <= 11; i++ {
		vs = append(vs, &client.Vertex{Digest: digest.FromString(fmt.Sprint(i)), Name: fmt.Sprintf("[%2d/11] RUN step %d", i, i),
			Started: at(i), Completed: at(i + 1)})
	}
	vs = append(vs, &client.Vertex{Digest: digest.FromString("named"), Name: "[build  3/12] RUN make", Started: at(20), Completed: at(21)})
	rec := &controlapi.BuildHistoryRecord{Ref: "padded", CreatedAt: timestamppb.New(t0), CompletedAt: timestamppb.New(t0.Add(time.Minute))}
	b, err := NewBuild(rec, []*client.SolveStatus{{Vertexes: vs}}, nil, nil, nil, Config{SecretKey: []byte("test")})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	for _, s := range b.Steps {
		keys[s.Key] = s.Name
	}
	if len(b.Steps) != 12 || keys["[ 1/11]"] != "RUN step 1" || keys["[11/11]"] != "RUN step 11" || keys["[build  3/12]"] != "RUN make" {
		t.Errorf("%d steps: %v", len(b.Steps), keys)
	}
}

func TestStageOfKey(t *testing.T) {
	for key, want := range map[string]string{
		"[ 3/10]":                     "",
		"[3/4]":                       "",
		"[build 3/4]":                 "build",
		"[build-env  9/11]":           "build-env", // no trailing space of the padding
		"[linux/arm64 builder 2/4]":   "linux/arm64 builder",
		"[linux/amd64 builder  2/14]": "linux/amd64 builder",
	} {
		if got := stageOfKey(key); got != want {
			t.Errorf("stageOfKey(%q) = %q, want %q", key, got, want)
		}
	}
}

// A provenance whose mapping speaks of other digests than the progress
// (the docker/dockerfile:1.7-labs frontend image in the lab run: grafana's
// cold build showed "nothing was rebuilt, 56 steps came from a build
// running at the same time") leaves every step the build's own. A mapping
// that knows one of the build's vertices still marks a foreign one as
// received.
func TestForeignDigestMapping(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	t0 := time.Date(2026, 11, 11, 0, 43, 33, 0, time.UTC)
	at := func(s float64) *time.Time { x := t0.Add(time.Duration(s * float64(time.Second))); return &x }
	build := func(ds ...string) *Build {
		t.Helper()
		vs := []*client.Vertex{
			{Digest: digest.Digest(ds[0]), Name: "[1/4] FROM docker.io/library/alpine:3.22", Started: at(1), Completed: at(2)},
			{Digest: digest.Digest(ds[1]), Name: "[2/4] COPY . /src", Started: at(2), Completed: at(3)},
			{Digest: digest.Digest(ds[2]), Name: "[3/4] RUN du -sh /src", Started: at(3), Completed: at(60)},
		}
		rec := &controlapi.BuildHistoryRecord{Ref: "r", CreatedAt: timestamppb.New(t0), CompletedAt: timestamppb.New(t0.Add(time.Minute)),
			FrontendAttrs: map[string]string{"context": "https://gitlab.com/group/big.git?ref=ci-accelerator&checksum=" + commit + "&fetch-by-commit=true"}}
		b, err := NewBuild(rec, []*client.SolveStatus{{Vertexes: vs}}, [][]byte{gitProvenance(t, commit, commit, gitOpts{})}, nil, nil, Config{SecretKey: []byte("test")})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	states := func(b *Build) map[string]StepState {
		out := map[string]StepState{}
		for _, s := range b.Steps {
			out[s.Key] = s.State
		}
		return out
	}
	other := func(s string) string { return digest.FromString(s).String() }
	got := states(build(other("a"), other("b"), other("c")))
	for k, st := range got {
		if st == Received {
			t.Errorf("foreign mapping: %s received from nobody", k)
		}
	}
	if got["[3/4]"] != Built {
		t.Errorf("foreign mapping: the RUN step is %q, want built", got["[3/4]"])
	}
	// The image and the copy are the build's own, the RUN another build's.
	got = states(build(vertexDigest(commit, "from", ""), vertexDigest(commit, "copy", ""), other("c")))
	if got["[3/4]"] != Received {
		t.Errorf("known mapping: a foreign vertex is %q, want received", got["[3/4]"])
	}
}
