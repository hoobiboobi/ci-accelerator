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
	// NotInCache: same definition, no build-context input anywhere below
	// the step, and the previous build ran on this daemon (Old empty):
	// the cache here no longer had it.
	NotInCache ReasonKind = "not-in-cache"
	// CacheElsewhere: same definition as in the previous build, but the
	// build context feeds the step or an ancestor, so the files or the
	// cache — the records cannot tell which. Paths lists what the context
	// feeds into its ancestry. Old/New name the daemons when the previous
	// build ran elsewhere; both empty means it ran here. Settle decides it
	// when another build of the same pipeline can; GitLab's comparison of
	// the commits decides the rest.
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

// FarmKinds are the reasons the build farm answers for: the code did not
// change and the cache was missing — on another node, or no longer on this
// one. The definition exists exactly once; every consumer (the pages'
// statusOf, the feed's outcome filters, the rollups, the Nodes counters)
// derives from it or is locked to it by a test, so the Overview and the
// Nodes page can never disagree about what counts as the farm's fault.
func FarmKinds() []ReasonKind { return []ReasonKind{OtherNode, NotInCache} }

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
			crossDaemon := prev.Daemon != "" && cur.Daemon != "" && prev.Daemon != cur.Daemon
			if cur.prov.contextDependent(s.StepID) {
				// The build context feeds this step or an ancestor (the
				// file-reading step may itself be cached or received, so
				// the first executed step says nothing by itself): the
				// files or the cache — unclear. Paths carry everything the
				// context feeds into the whole ancestor cone, not just
				// this step's own sources: the rebuild could have been
				// caused by any of them. Settle or GitLab's comparison of
				// the commits decides it.
				r := Reason{Kind: CacheElsewhere, Step: s, Paths: cur.prov.contextSources(s.StepID)}
				if crossDaemon {
					r.Old, r.New = prev.Daemon, cur.Daemon
				}
				return r, true
			}
			// No local source anywhere below the step and it did not
			// change: with the same identity its cache key is the same,
			// so only the cache can be missing, never the code.
			if crossDaemon {
				return Reason{Kind: OtherNode, Step: s, Old: prev.Daemon, New: cur.Daemon}, true
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

// FailedStep explains a failed build by the step that failed. BuildKit
// records no provenance for a failed build, so FirstRebuild cannot compare
// it, but the steps' text is known: the failed step is compared with the
// step in the same place (stage, position and step count) of the previous
// build of the image. NewStep: there was none, and no step of the previous
// build has its text; StepChanged: the text there differs (Old, New). ok
// is false when the same step was there, or moved: it passed then.
func FailedStep(prev, cur *Build) (Reason, bool) {
	if prev == nil || cur.Error == "" {
		return Reason{}, false
	}
	var failed *Step
	for i := range cur.Steps {
		s := &cur.Steps[i]
		// Steps stopped because another one failed say "canceled".
		if s.State == Built && s.Error != "" && !strings.Contains(s.Error, "context canceled") {
			failed = s
			break
		}
	}
	if failed == nil {
		return Reason{}, false
	}
	for _, p := range prev.Steps {
		if p.Key == failed.Key {
			if p.Name == failed.Name {
				return Reason{}, false
			}
			return Reason{Kind: StepChanged, Step: *failed, Old: p.Name, New: failed.Name}, true
		}
	}
	for _, p := range prev.Steps {
		if p.Name == failed.Name {
			return Reason{}, false
		}
	}
	return Reason{Kind: NewStep, Step: *failed}, true
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
			// The rule every settling shares: an "the cache was elsewhere"
			// outcome is OtherNode only when the previous build ran on
			// another daemon (Old). With no previous daemon the files did
			// not change and the cache was HERE last time: eviction.
			if r.Old == "" {
				out[i].Kind, out[i].By = NotInCache, by
				continue
			}
			out[i].Kind, out[i].By = OtherNode, by
			continue
		}
		for j, o := range reasons {
			if j == i || !samePipeline(builds[i], builds[j]) || o.Step.Identity != r.Step.Identity {
				continue
			}
			// A sibling that rebuilt the same step although its previous
			// build ran on its own daemon (FilesChanged once settled, or a
			// same-daemon CacheElsewhere): the cache was there and still
			// missed, so the files changed (erring towards the change, as
			// everywhere: an eviction in the same breath is the rare
			// alternative).
			if o.Kind == FilesChanged || (o.Kind == CacheElsewhere && o.Old == "") {
				out[i].Kind, out[i].By = FilesChanged, builds[j].Ref
				break
			}
		}
	}
	return out
}

// cachedOnAnotherDaemon returns a build of builds[i]'s pipeline, on another
// daemon, that found the step with this identity in its cache. A witness
// whose daemon also EXECUTED the step in this pipeline proves nothing: its
// cache hit may be that very execution's product (same commit, written
// seconds earlier), not a survivor from before the change.
func cachedOnAnotherDaemon(builds []*Build, i int, identity string) string {
	for j, o := range builds {
		if j == i || !samePipeline(builds[i], o) || o.Daemon == "" || o.Daemon == builds[i].Daemon {
			continue
		}
		for _, s := range o.Steps {
			if s.Identity == identity && s.State == Cached && !builtInPipelineOn(builds, builds[i], o.Daemon, identity) {
				return o.Ref
			}
		}
	}
	return ""
}

// builtInPipelineOn says whether any build of cur's pipeline executed the
// step with this identity on the given daemon.
func builtInPipelineOn(builds []*Build, cur *Build, daemon, identity string) bool {
	for _, b := range builds {
		if b.Daemon != daemon || !samePipeline(cur, b) {
			continue
		}
		for _, s := range b.Steps {
			if s.Identity == identity && s.State == Built {
				return true
			}
		}
	}
	return false
}

func samePipeline(a, b *Build) bool {
	return a.Attribution.State == Attributed && b.Attribution.State == Attributed &&
		a.Attribution.Instance == b.Attribution.Instance && a.Attribution.Pipeline == b.Attribution.Pipeline
}
