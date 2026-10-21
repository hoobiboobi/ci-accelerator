package history

import (
	"strings"
	"testing"
	"time"

	"github.com/moby/buildkit/client"
	digest "github.com/opencontainers/go-digest"
)

// Two builds asking for the same cached step at once: BuildKit may show the
// waiting build the other build's vertex, marked cached. That is a cache
// hit, not work received from another build, whatever the order of the
// vertices in the stream.
func TestCachedForeignVertexIsACacheHit(t *testing.T) {
	t0 := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	at := func(s float64) *time.Time { x := t0.Add(time.Duration(s * float64(time.Second))); return &x }
	own := &client.Vertex{Digest: digest.FromString("own"), Name: "[deps 2/2] RUN pip install", Started: at(1), Completed: at(1)}
	foreign := &client.Vertex{Digest: digest.FromString("foreign"), Name: "[deps 2/2] RUN pip install", Started: at(0.5), Completed: at(0.5), Cached: true}
	for name, order := range map[string][]*client.Vertex{"own first": {own, foreign}, "foreign first": {foreign, own}} {
		b := &Build{
			Started:     t0,
			Vertices:    map[string]bool{own.Digest.String(): true},
			vertexStart: map[string]time.Time{},
			prov: &Provenance{
				Steps:         map[string]ProvStep{"step1": {ID: "step1"}},
				DigestMapping: map[string]string{own.Digest.String(): "step1"},
				Lines:         map[string]int{"step1": 4},
				Identity:      map[string]string{"step1": "id1"},
			},
		}
		steps, _ := b.steps([]*client.SolveStatus{{Vertexes: order}})
		if len(steps) != 1 || steps[0].State != Cached || steps[0].ReceivedFrom != "" {
			t.Errorf("%s: %+v, want one cached step", name, steps)
		}
	}
}

// A built step's output reaches the model masked and clipped; a received
// step's output belongs to the build that executed it.
func TestStepOutputIsMaskedAndClipped(t *testing.T) {
	t0 := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	at := func(s float64) *time.Time { x := t0.Add(time.Duration(s * float64(time.Second))); return &x }
	// BuildKit writes build arguments into a step's name, expanded.
	own := &client.Vertex{Digest: digest.FromString("own"), Name: "[app 1/2] RUN make TOKEN=s3cr3t-npm", Started: at(1), Completed: at(2), Error: "make: login with s3cr3t-npm failed"}
	foreign := &client.Vertex{Digest: digest.FromString("foreign"), Name: "[deps 1/1] RUN pip install", Started: at(0.5), Completed: at(1)}
	b := &Build{
		Started:     t0,
		BuildArgs:   map[string]string{"NPM_TOKEN": "s3cr3t-npm"},
		Vertices:    map[string]bool{own.Digest.String(): true},
		vertexStart: map[string]time.Time{},
	}
	b.prov = &Provenance{
		Steps:         map[string]ProvStep{"step1": {ID: "step1"}},
		DigestMapping: map[string]string{own.Digest.String(): "step1"},
		Identity:      map[string]string{"step1": "id1"},
	}
	long := strings.Repeat("x", 2*MaxStepOutput)
	steps, _ := b.steps([]*client.SolveStatus{{
		Vertexes: []*client.Vertex{foreign, own},
		Logs: []*client.VertexLog{
			{Vertex: own.Digest, Data: []byte("using s3cr3t-npm\n")},
			{Vertex: own.Digest, Data: []byte(long + "END\n")},
			{Vertex: foreign.Digest, Data: []byte("Collecting flask\n")},
		},
	}})
	for _, s := range steps {
		switch s.Key {
		case "[app 1/2]":
			if s.Name != "RUN make TOKEN=[masked]" {
				t.Errorf("step name not masked: %q", s.Name)
			}
			if s.State != Built || s.Error != "make: login with [masked] failed" || !strings.HasPrefix(s.Output, "using [masked]\n") || !strings.HasSuffix(s.Output, "END\n") || len(s.Output) > MaxStepOutput+64 {
				t.Errorf("built step: %s, output %d bytes, starts %q", s.State, len(s.Output), s.Output[:20])
			}
		case "[deps 1/1]":
			if s.State != Received || s.Output != "" {
				t.Errorf("received step: %s, output %q", s.State, s.Output)
			}
		}
	}
}

// A Dockerfile whose only stage has no name: BuildKit names its steps
// "[1/3] ...", without a stage. They are steps too; BuildKit's own
// vertices ("[internal] ...", "[auth] ...") are not.
func TestStepsOfAnUnnamedStage(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	at := func(s float64) *time.Time { x := t0.Add(time.Duration(s * float64(time.Second))); return &x }
	vs := []*client.Vertex{
		{Digest: digest.FromString("ctx"), Name: "[internal] load build context", Started: at(0), Completed: at(0.1)},
		{Digest: digest.FromString("auth"), Name: "[auth] library/alpine:pull token for registry-1.docker.io", Started: at(0), Completed: at(0.1)},
		{Digest: digest.FromString("from"), Name: "[1/3] FROM docker.io/library/alpine:3.22", Started: at(0.1), Completed: at(0.5)},
		{Digest: digest.FromString("run"), Name: "[2/3] RUN sleep 2 && echo deps > /deps", Started: at(0.5), Completed: at(2.5)},
		{Digest: digest.FromString("bust"), Name: "[3/3] RUN echo bust=$BUST", Started: at(2.5), Completed: at(2.6), Cached: true},
	}
	b := &Build{Started: t0, Vertices: map[string]bool{}, vertexStart: map[string]time.Time{}}
	steps, _ := b.steps([]*client.SolveStatus{{Vertexes: vs}})
	var keys []string
	for _, s := range steps {
		keys = append(keys, s.Key+" "+string(s.State))
	}
	if got := strings.Join(keys, ", "); got != "[1/3] built, [2/3] built, [3/3] cached" {
		t.Errorf("steps: %s", got)
	}
}
