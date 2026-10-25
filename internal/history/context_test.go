package history

import (
	"strings"
	"testing"
)

// The build context can feed a step through an ancestor: the step's own
// inputs show no local source, but its cache key still depends on context
// files. These tests pin the transitive view.

func TestContextDependence(t *testing.T) {
	bs := loadFixtures(t)
	b := byJob(t, bs, 1003, "api")
	run := step(t, b, "[api 2/2]") // RUN: reads no files itself
	if b.prov.localInput(run.StepID) {
		t.Fatalf("[api 2/2] reads the context directly; the fixture changed")
	}
	if !b.prov.contextDependent(run.StepID) {
		t.Errorf("[api 2/2] must depend on the context through COPY api/ and COPY deps.txt")
	}
	if got := strings.Join(b.prov.contextSources(run.StepID), ","); got != "api,deps.txt" {
		t.Errorf("contextSources([api 2/2]) = %q, want api,deps.txt", got)
	}
	// A deps step before any COPY: nothing below it reads the context.
	tools := step(t, b, "[deps 3/6]")
	if b.prov.contextDependent(tools.StepID) {
		t.Errorf("[deps 3/6] must not depend on the context")
	}
	if ps := b.prov.contextSources(tools.StepID); len(ps) != 0 {
		t.Errorf("contextSources([deps 3/6]) = %v, want none", ps)
	}
}

// A step whose context-reading ancestor was executed by a concurrent
// sibling build (Received) or found in the cache is rebuilt because of the
// context's content, which the records cannot see: never a confident
// "not in cache" or "other node".
func TestRebuildBehindReceivedOrCachedContextStep(t *testing.T) {
	bs := loadFixtures(t)
	// The same build as previous and current: every identity matches, so
	// only the states and daemons set below decide the reason.
	prev := *byJob(t, bs, 1003, "api")
	prev.Daemon = "daemon-a"

	// The sibling executed COPY api/: this build received it and ran only
	// the RUN after it.
	received := *byJob(t, bs, 1003, "api")
	received.Daemon = "daemon-a"
	received.Steps = append([]Step(nil), received.Steps...)
	for i, s := range received.Steps {
		if s.Key == "[api 1/2]" {
			received.Steps[i].State = Received
		}
	}
	r, ok := FirstRebuild(&prev, &received)
	if !ok || r.Kind != CacheElsewhere || r.Step.Key != "[api 2/2]" {
		t.Fatalf("received context step, same daemon: %+v, want cache-elsewhere at [api 2/2]", r)
	}
	if r.Old != "" || r.New != "" {
		t.Errorf("same daemon must leave the daemons empty: %+v", r)
	}
	if got := strings.Join(r.Paths, ","); got != "api,deps.txt" {
		t.Errorf("paths = %q, want the context sources of its ancestry", got)
	}

	// The previous build ran on another daemon: still unclear (files or
	// node), with the daemons named.
	received.Daemon = "daemon-b"
	if r, _ := FirstRebuild(&prev, &received); r.Kind != CacheElsewhere || r.Old != "daemon-a" || r.New != "daemon-b" {
		t.Errorf("received context step, another daemon: %+v", r)
	}

	// Every ancestor cached (an earlier job of the pipeline warmed them),
	// only the RUN rebuilt: its key still depends on the context.
	cached := *byJob(t, bs, 1003, "api")
	cached.Daemon = "daemon-a"
	cached.Steps = append([]Step(nil), cached.Steps...)
	for i, s := range cached.Steps {
		switch {
		case s.Key == "[api 2/2]":
			cached.Steps[i].State = Built
		case s.State == Built && cached.prov.imageSource(s.StepID) == "":
			cached.Steps[i].State = Cached
		}
	}
	if r, ok := FirstRebuild(&prev, &cached); !ok || r.Kind != CacheElsewhere || r.Step.Key != "[api 2/2]" || r.Old != "" {
		t.Errorf("cached context ancestors, same daemon: %+v, want cache-elsewhere", r)
	}
}
