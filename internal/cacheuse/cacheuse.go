// Package cacheuse says whose a build daemon's cache records are.
//
// BuildKit's disk usage lists records (id, parents, description, type,
// size, creation time) but no build, session or project. A layer is
// attributed to the build that executed the step that made it: a record
// created while a build on the same node executed a step with the
// record's description (COPY, ADD, WORKDIR by the step's name, RUN by its
// command). Several projects executing the same step at that moment make
// it shared; nothing matching leaves it unattributed. Base images, build
// contexts and cache mounts are pools of their own: every project uses
// them.
package cacheuse

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kind is what a record holds.
type Kind string

const (
	Layer   Kind = "layer"   // a step's result
	Base    Kind = "base"    // a base image's layer, pulled from a registry
	Context Kind = "context" // a build context or Dockerfile sent by a client
	Mount   Kind = "mount"   // a cache mount (RUN --mount=type=cache)
	Other   Kind = "other"
)

// Record is one record of a daemon's disk usage.
type Record struct {
	ID          string    `json:"id"`
	Parents     []string  `json:"parents,omitempty"`
	Description string    `json:"description"`
	Type        string    `json:"type"`
	Size        int64     `json:"size"`
	Created     time.Time `json:"created"`
	LastUsed    time.Time `json:"lastUsed,omitzero"`
	InUse       bool      `json:"inUse,omitempty"`
	Mutable     bool      `json:"mutable,omitempty"`
}

// KindOf classifies a record.
func KindOf(r Record) Kind {
	switch {
	case r.Type == "exec.cachemount":
		return Mount
	case r.Type == "source.local":
		return Context
	case r.Type == "regular" && strings.HasPrefix(r.Description, "pulled from "):
		return Base
	case r.Type == "regular":
		return Layer
	}
	return Other
}

// Owner is a project's build that executed a step.
type Owner struct {
	Instance string `json:"instance"`
	Project  uint64 `json:"project"`
	Target   string `json:"target"`
	Build    int64  `json:"build"`
}

// Step is a step a build on the node executed.
type Step struct {
	// Key and Name as the build's history has them: "[deps 2/2]", "RUN ...".
	Key, Name          string
	Started, Completed time.Time
	Owner              Owner
}

// Slack is how far a record's creation may fall outside its step's run:
// the record is committed as the step completes.
const Slack = 2 * time.Second

// Describe is what BuildKit writes into the description of the record a
// step makes; empty for a step that makes none it can be told by.
func Describe(key, name string) string {
	switch {
	case strings.HasPrefix(name, "RUN "):
		cmd := strings.TrimPrefix(name, "RUN ")
		// Flags (--mount=..., --network=...) are not part of the command.
		for strings.HasPrefix(cmd, "--") {
			i := strings.IndexByte(cmd, ' ')
			if i < 0 {
				return ""
			}
			cmd = strings.TrimLeft(cmd[i+1:], " ")
		}
		if strings.HasPrefix(cmd, "[") {
			// The exec form is described as its argv, which the name does
			// not keep exactly.
			return ""
		}
		return "mount / from exec /bin/sh -c " + cmd
	case strings.HasPrefix(name, "FROM "):
		return ""
	case key != "":
		return key + " " + name
	}
	return name
}

// Match attributes a layer record: the builds that executed a step with
// its description around its creation. One project: its build (the
// earliest, when several of its builds did); several projects: one owner
// per project, the record is shared; none: nil.
func Match(r Record, steps []Step) []Owner {
	if KindOf(r) != Layer {
		return nil
	}
	byProject := map[[2]string]Owner{}
	for _, s := range steps {
		if r.Created.Before(s.Started.Add(-Slack)) || r.Created.After(s.Completed.Add(Slack)) {
			continue
		}
		if d := Describe(s.Key, s.Name); d == "" || d != r.Description {
			continue
		}
		k := [2]string{s.Owner.Instance, strconv.FormatUint(s.Owner.Project, 10)}
		if o, ok := byProject[k]; !ok || s.Owner.Build < o.Build {
			byProject[k] = s.Owner
		}
	}
	out := make([]Owner, 0, len(byProject))
	for _, o := range byProject {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Instance != out[j].Instance {
			return out[i].Instance < out[j].Instance
		}
		return out[i].Project < out[j].Project
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

// Usage is a node's cache by owner.
type Usage struct {
	Total int64
	// Projects is the bytes each project owns alone, largest first.
	Projects []ProjectBytes
	// Shared is the layers several projects made at once; Unknown the
	// layers no build matched; Base, Contexts, Mounts and Other the pools.
	Shared, Unknown, Base, Contexts, Mounts, Other int64
}

// ProjectBytes is a project's share of a node's cache.
type ProjectBytes struct {
	Instance string
	Project  uint64
	Bytes    int64
	Records  int
}

// Attributed is the share of the bytes Usage can name an owner or pool
// for: everything but the unattributed layers.
func (u Usage) Attributed() float64 {
	if u.Total == 0 {
		return 1
	}
	return float64(u.Total-u.Unknown) / float64(u.Total)
}
