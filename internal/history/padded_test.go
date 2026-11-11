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
