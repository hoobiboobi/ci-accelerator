package history

import (
	"bytes"
	"sort"
	"strings"
)

// ReasonKind names why a step was rebuilt.
type ReasonKind string

const (
	// FirstBuild: there is no previous build to compare with.
	FirstBuild ReasonKind = "first-build"
	// StepChanged: the step's own definition changed (its Dockerfile line).
	StepChanged ReasonKind = "step-changed"
	// ArgChanged: the definition differs only in a build argument's value.
	ArgChanged ReasonKind = "arg-changed"
	// BaseImageChanged: an image the build starts from resolved differently.
	BaseImageChanged ReasonKind = "base-image-changed"
	// FilesChanged: same definition, but it reads the build context, whose
	// files changed. Which files is not recorded by BuildKit.
	FilesChanged ReasonKind = "files-changed"
	// NotInCache: same definition, no context input: evicted, or cached on
	// another daemon.
	NotInCache ReasonKind = "not-in-cache"
	// NewStep: the previous build had no step at this position.
	NewStep ReasonKind = "new-step"
)

// Reason explains the first rebuilt step of a build.
type Reason struct {
	Kind  ReasonKind
	Step  Step
	Old   string   // previous definition, argument value or image
	New   string   // current one
	Arg   string   // for ArgChanged
	Paths []string // for FilesChanged: what the step copies
}

// FirstRebuild compares a build with the previous build of the same project
// and target and explains the first step it executed. ok is false when every
// step came from cache or another build.
func FirstRebuild(prev, cur *Build) (Reason, bool) {
	if cur.prov == nil {
		return Reason{}, false
	}
	prevSteps := map[string]Step{}
	if prev != nil {
		for _, s := range prev.Steps {
			prevSteps[s.Key] = s
		}
	}
	for _, s := range cur.Steps {
		if s.State != Built || s.StepID == "" {
			continue
		}
		// An image source is resolved in every build and never marked
		// cached: it only counts when the image changed.
		if img := cur.prov.imageSource(s.StepID); img != "" {
			p, ok := prevSteps[s.Key]
			if prev == nil || !ok || prev.prov == nil {
				continue
			}
			if old := prev.prov.imageSource(p.StepID); old != "" && old != img {
				return Reason{Kind: BaseImageChanged, Step: s, Old: old, New: img}, true
			}
			continue
		}
		if prev == nil || prev.prov == nil {
			return Reason{Kind: FirstBuild, Step: s}, true
		}
		p, ok := prevSteps[s.Key]
		if !ok || p.StepID == "" {
			return Reason{Kind: NewStep, Step: s}, true
		}
		if p.Identity == s.Identity {
			if cur.prov.localInput(s.StepID) {
				return Reason{Kind: FilesChanged, Step: s, Paths: cur.prov.copySources(s.StepID)}, true
			}
			return Reason{Kind: NotInCache, Step: s}, true
		}
		oldOp, newOp := normalizeOp(prev.prov.Steps[p.StepID].Op), normalizeOp(cur.prov.Steps[s.StepID].Op)
		if !bytes.Equal(oldOp, newOp) {
			if arg, o, n, ok := argChange(prev, cur, p.StepID, s.StepID); ok {
				return Reason{Kind: ArgChanged, Step: s, Arg: arg, Old: o, New: n}, true
			}
			return Reason{Kind: StepChanged, Step: s, Old: p.Name, New: s.Name}, true
		}
		// Same operation, different inputs: an earlier step changed but was
		// not reported (it came from cache or another build). Name this one.
		return Reason{Kind: StepChanged, Step: s, Old: p.Name, New: s.Name}, true
	}
	return Reason{}, false
}

// argChange reports a change that is only in environment variables set by
// build arguments of either build.
func argChange(prev, cur *Build, prevID, curID string) (string, string, string, bool) {
	po, co := viewOf(prev.prov.Steps[prevID].Op), viewOf(cur.prov.Steps[curID].Op)
	if po.Op.Exec == nil || co.Op.Exec == nil || strings.Join(po.Op.Exec.Meta.Args, "\x00") != strings.Join(co.Op.Exec.Meta.Args, "\x00") {
		return "", "", "", false
	}
	pe, ce := prev.prov.env(prevID), cur.prov.env(curID)
	var changed []string
	for k := range union(pe, ce) {
		if pe[k] != ce[k] {
			changed = append(changed, k)
		}
	}
	if len(changed) != 1 {
		return "", "", "", false
	}
	k := changed[0]
	if _, ok := cur.BuildArgs[k]; !ok {
		if _, ok := prev.BuildArgs[k]; !ok {
			return "", "", "", false
		}
	}
	return k, pe[k], ce[k], true
}

func union(a, b map[string]string) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// Link resolves Received steps: the build that executed a received vertex is
// the one whose own vertices include it (exact, no timing). Builds must come
// from the same daemon, where the sharing happened.
func Link(builds []*Build) {
	owner := map[string]string{}
	refs := make([]string, 0, len(builds))
	for _, b := range builds {
		refs = append(refs, b.Ref)
	}
	sort.Strings(refs)
	for _, b := range builds {
		for d := range b.Vertices {
			owner[d] = b.Ref
		}
	}
	for _, b := range builds {
		for i := range b.Steps {
			s := &b.Steps[i]
			if s.State == Received && s.ReceivedVertex != "" {
				s.ReceivedFrom = owner[s.ReceivedVertex]
			}
		}
	}
}
