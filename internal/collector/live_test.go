package collector

import (
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestClassify(t *testing.T) {
	now := time.Now()
	u := Classify([]Record{
		{Type: "regular", Description: "pulled from docker.io/library/python:3.13-slim@sha256:abc", Size: 100},
		{Type: "regular", Description: "pulled from docker.io/library/python:3.13-slim@sha256:abc", Size: 50},
		{Type: "regular", Description: "pulled from ghcr.io/org/base:1", Size: 7},
		{Type: "regular", Description: "mount / from exec /bin/sh -c pip install", Size: 30},
		{Type: "regular", Description: "[api 1/2] COPY api/ ./api", Size: 3},
		{Type: "exec.cachemount", Description: "cached mount /root/.cache/pip", Size: 20, LastUsed: &now},
		{Type: "exec.cachemount", Description: "cached mount /go", Size: 40},
		{Type: "source.local", Description: "local source for context", Size: 5},
		{Type: "frontend", Description: "x", Size: 1},
	})
	if u.Total != 256 || u.Images != 157 || u.Layers != 33 || u.Mounts != 60 || u.Contexts != 5 || u.Other != 1 {
		t.Errorf("%+v", u)
	}
	if u.ImageSizes["python:3.13-slim"] != 150 || u.ImageSizes["ghcr.io/org/base:1"] != 7 || len(u.ImageSizes) != 2 {
		t.Errorf("images %v", u.ImageSizes)
	}
	if len(u.MountList) != 2 || u.MountList[0].Size != 40 || !u.MountList[1].LastUsed.Equal(now) {
		t.Errorf("mounts %+v", u.MountList)
	}
}

func TestLiveState(t *testing.T) {
	var c Collector
	calls := 0
	c.OnLiveChange(func() { calls++ })
	c.live.connected("d1", true)
	rec := func(ref, target string) *controlapi.BuildHistoryRecord {
		return &controlapi.BuildHistoryRecord{Ref: ref, FrontendAttrs: map[string]string{"target": target},
			CreatedAt: timestamppb.New(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))}
	}
	c.live.event("d1", controlapi.BuildHistoryEventType_STARTED, rec("r1", "api"))
	c.live.event("d1", controlapi.BuildHistoryEventType_STARTED, rec("r2", "worker"))
	c.live.event("d1", controlapi.BuildHistoryEventType_COMPLETE, rec("r1", "api"))
	// Records completed before the stream began do not announce anything.
	c.live.event("d1", controlapi.BuildHistoryEventType_COMPLETE, rec("old", "api"))
	ds := c.Daemons()
	if len(ds) != 1 || !ds[0].Connected || len(ds[0].Active) != 1 || ds[0].Active[0].Target != "worker" || ds[0].Active[0].Started.IsZero() {
		t.Fatalf("%+v", ds)
	}
	if calls != 4 {
		t.Errorf("%d announcements, want 4", calls)
	}
	// An inspection is announced when it finds something new, not every
	// minute.
	c.live.inspected("d1", Inspection{Node: "n1"}, nil)
	c.live.inspected("d1", Inspection{Node: "n1"}, nil)
	if calls != 5 {
		t.Errorf("%d announcements after two equal inspections, want 5", calls)
	}
	c.live.inspected("d1", Inspection{Node: "n1", Usage: Usage{Total: 1}}, nil)
	if calls != 6 {
		t.Errorf("a changed inspection was not announced")
	}
	c.live.connected("d1", false)
	ds = c.Daemons()
	if ds[0].Connected || len(ds[0].Active) != 0 || ds[0].Inspection.Node != "n1" || ds[0].InspectedAt.IsZero() {
		t.Errorf("after the stream ended: %+v", ds[0])
	}
}

func TestForgetGoneDaemon(t *testing.T) {
	var c Collector
	c.live.connected("old", true)
	c.live.connected("new", true)
	c.live.forget("old")
	if ds := c.Daemons(); len(ds) != 1 || ds[0].Name != "new" {
		t.Errorf("%+v", ds)
	}
}
