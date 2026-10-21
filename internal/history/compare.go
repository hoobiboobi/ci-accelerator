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
	// CacheElsewhere: same definition as in the previous build, which ran
	// on another daemon: its cache is there, not here. For a step that reads
	// the build context, Paths lists what it reads: whether those files
	// changed as well, BuildKit's history cannot tell. Settle decides it
	// when another build of the same pipeline can.
	CacheElsewhere ReasonKind = "cache-elsewhere"
	// OtherNode: the step reads no files and did not change, or its files
	// did not change (a job of the same pipeline proves it, see Settle):
	// the daemon lacked the cache:
	// another build of the same pipeline (same commit) found this step in
	// the cache on another daemon. Set by Settle.
	OtherNode ReasonKind = "other-node"
	// NewStep: the previous build had no step at this position.
	NewStep ReasonKind = "new-step"
)

// Reason explains the first rebuilt step of a build.
type Reason struct {
	Kind  ReasonKind
	Step  Step
	Old   string   // previous definition, argument value or image
	New   string   // current one
	Arg   string   // for ArgChanged: the argument, or several joined by ", "
	Paths []string // for FilesChanged: what the step copies
	// By is the build of the same pipeline that settled a CacheElsewhere
	// reason (see Settle).
	By string
}

// FirstRebuild compares a build with the previous build of the same project
// and target and explains the first step it executed. ok is false when every
// step came from cache or another build.
func FirstRebuild(prev, cur *Build) (Reason, bool) {
	if cur.prov == nil || !rebuiltAny(cur) {
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
			if prev.Daemon != "" && cur.Daemon != "" && prev.Daemon != cur.Daemon {
				if !cur.prov.localInput(s.StepID) {
					// It reads no files and did not change: only the
					// cache can be missing here, never the code.
					return Reason{Kind: OtherNode, Step: s, Old: prev.Daemon, New: cur.Daemon}, true
				}
				// It reads files: they may have changed too.
				return Reason{Kind: CacheElsewhere, Step: s, Old: prev.Daemon, New: cur.Daemon,
					Paths: cur.prov.copySources(s.StepID)}, true
			}
			if cur.prov.localInput(s.StepID) {
				return Reason{Kind: FilesChanged, Step: s, Paths: cur.prov.copySources(s.StepID)}, true
			}
			return Reason{Kind: NotInCache, Step: s}, true
		}
		oldOp, newOp := prev.prov.Steps[p.StepID].Op, cur.prov.Steps[s.StepID].Op
		if !bytes.Equal(oldOp, newOp) {
			if args, o, n, ok := argChange(prev, cur, p.StepID, s.StepID); ok {
				if len(args) > 1 {
					// Several arguments: their names, not their values.
					return Reason{Kind: ArgChanged, Step: s, Arg: strings.Join(args, ", ")}, true
				}
				if SecretArg(args[0]) {
					o, n = Masked, Masked
				}
				return Reason{Kind: ArgChanged, Step: s, Arg: args[0], Old: o, New: n}, true
			}
			return Reason{Kind: StepChanged, Step: s, Old: p.Name, New: s.Name}, true
		}
		// Same operation, different inputs: an earlier step changed but was
		// not reported (it came from cache or another build). Name this one.
		return Reason{Kind: StepChanged, Step: s, Old: p.Name, New: s.Name}, true
	}
	return Reason{}, false
}

// rebuiltAny reports whether the build executed a step other than resolving
// an image (which happens in every build): only then is there a rebuild to
// explain.
func rebuiltAny(b *Build) bool {
	for _, s := range b.Steps {
		if s.State == Built && s.StepID != "" && b.prov.imageSource(s.StepID) == "" {
			return true
		}
	}
	return false
}

// argChange reports a change that is only in environment variables set by
// build arguments of either build: their names (sorted) and, for one, its
// old and new value.
func argChange(prev, cur *Build, prevID, curID string) ([]string, string, string, bool) {
	po, co := viewOf(prev.prov.Steps[prevID].Op), viewOf(cur.prov.Steps[curID].Op)
	if po.Op.Exec == nil || co.Op.Exec == nil || strings.Join(po.Op.Exec.Meta.Args, "\x00") != strings.Join(co.Op.Exec.Meta.Args, "\x00") {
		return nil, "", "", false
	}
	pe, ce := prev.prov.env(prevID), cur.prov.env(curID)
	var changed []string
	for k := range union(pe, ce) {
		if pe[k] == ce[k] {
			continue
		}
		_, inCur := cur.BuildArgs[k]
		_, inPrev := prev.BuildArgs[k]
		if !inCur && !inPrev {
			return nil, "", "", false
		}
		changed = append(changed, k)
	}
	if len(changed) == 0 {
		return nil, "", "", false
	}
	sort.Strings(changed)
	return changed, pe[changed[0]], ce[changed[0]], true
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

// Link resolves which build executed a received step, from the builds of
// one daemon (where the sharing happened):
//   - a vertex of another build in this build's progress (a step that
//     depends on the build context) belongs to that build only;
//   - a vertex in both builds' own mappings (a step that reads nothing from
//     the context) with the same start in both is one execution, by the
//     earlier build. A vertex executed again later starts at another time.
func Link(builds []*Build) {
	sorted := append([]*Build(nil), builds...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Started.Before(sorted[j].Started) })
	owners := map[string][]*Build{}
	for _, b := range sorted {
		for d := range b.Vertices {
			owners[d] = append(owners[d], b)
		}
	}
	for _, b := range builds {
		changed := false
		for i := range b.Steps {
			s := &b.Steps[i]
			switch {
			case s.State == Received && s.ReceivedVertex != "" && s.ReceivedVertex != s.vertex:
				for _, o := range owners[s.ReceivedVertex] {
					if o.Ref != b.Ref {
						s.ReceivedFrom = o.Ref
						break
					}
				}
			case s.vertex != "" && (s.State == Built || s.State == Received):
				start, ok := b.vertexStart[s.vertex]
				if !ok || (b.prov != nil && b.prov.imageSource(s.StepID) != "") {
					continue
				}
				for _, o := range owners[s.vertex] {
					if o.Ref == b.Ref || !o.Started.Before(b.Started) {
						continue
					}
					if t, ok := o.vertexStart[s.vertex]; ok && t.Equal(start) {
						s.State, s.ReceivedVertex, s.ReceivedFrom = Received, s.vertex, o.Ref
						s.foreignStarted, changed = start, true
						break
					}
				}
			}
		}
		if changed {
			waits(b.Started, b.Steps)
		}
	}
}

// Settle decides the CacheElsewhere reasons of a pipeline's builds with
// each other. Builds of one pipeline built the same commit, so the same
// step read the same files in each of them:
//   - another build found the same step in the cache on another daemon:
//     the files did not change, this daemon lacked the cache (OtherNode);
//   - another build rebuilt the same step on the daemon of its own previous
//     build because its files changed: they changed (FilesChanged).
//
// Otherwise the reason stays CacheElsewhere: the files or the daemon, the
// records cannot tell which. reasons[i] is FirstRebuild's reason for
// builds[i] (zero when there is none); only builds attributed to the same
// pipeline are compared. The result is in the same order.
func Settle(builds []*Build, reasons []Reason) []Reason {
	out := append([]Reason(nil), reasons...)
	for i, r := range reasons {
		if r.Kind != CacheElsewhere || r.Step.Identity == "" {
			continue
		}
		if by := cachedOnAnotherDaemon(builds, i, r.Step.Identity); by != "" {
			out[i].Kind, out[i].By = OtherNode, by
			continue
		}
		for j, o := range reasons {
			if j != i && samePipeline(builds[i], builds[j]) && o.Kind == FilesChanged && o.Step.Identity == r.Step.Identity {
				out[i].Kind, out[i].By = FilesChanged, builds[j].Ref
				break
			}
		}
	}
	return out
}

// cachedOnAnotherDaemon returns a build of builds[i]'s pipeline, on another
// daemon, that found the step with this identity in its cache.
func cachedOnAnotherDaemon(builds []*Build, i int, identity string) string {
	for j, o := range builds {
		if j == i || !samePipeline(builds[i], o) || o.Daemon == "" || o.Daemon == builds[i].Daemon {
			continue
		}
		for _, s := range o.Steps {
			if s.Identity == identity && s.State == Cached {
				return o.Ref
			}
		}
	}
	return ""
}

func samePipeline(a, b *Build) bool {
	return a.Attribution.State == Attributed && b.Attribution.State == Attributed &&
		a.Attribution.Instance == b.Attribution.Instance && a.Attribution.Pipeline == b.Attribution.Pipeline
}
