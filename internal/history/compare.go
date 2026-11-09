package history

import (
	"bytes"
	"slices"
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
	// NoCache: the job switched the cache off for the step's stage
	// (buildctl --no-cache, or --opt no-cache=<stages>): every step ran
	// whatever was cached.
	NoCache ReasonKind = "no-cache"
	// RemoteSource: same definition as in the previous build, but the step
	// or an ancestor reads from the network (ADD of an http(s) URL, a git
	// repository by branch or tag) without a checksum: what it fetched may
	// have changed upstream, or the cache was missing — the records cannot
	// tell. Paths names the sources (without credentials or queries).
	RemoteSource ReasonKind = "remote-source"
)

// ChangeKinds are the reasons that are the build's own doing: its code,
// its Dockerfile, its arguments or base image, or its job's choice to
// build without the cache. Like FarmKinds, defined once for every page
// and query.
func ChangeKinds() []ReasonKind {
	return []ReasonKind{FirstBuild, StepChanged, NewStep, ArgChanged, BaseImageChanged, FilesChanged, NoCache}
}

// UnclearKinds are the reasons that name a rebuilt step without being able
// to say whether the build or the farm caused it.
func UnclearKinds() []ReasonKind { return []ReasonKind{CacheElsewhere, RemoteSource} }

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
	prevSteps := stepsByKey(prev, cur)
	for _, s := range cur.Steps {
		if s.State != Built || s.StepID == "" {
			continue
		}
		// An image source is resolved in every build and never marked
		// cached: it only counts when the image changed.
		if img := cur.prov.imageSource(s.StepID); img != "" {
			if prev == nil || prev.prov == nil {
				continue
			}
			old := ""
			if p, _, ok := prevSteps.of(s); ok && p.StepID != "" {
				old = prev.prov.imageSource(p.StepID)
			} else {
				// Not reported (below a cache hit): the image the previous
				// build used instead, when that is unambiguous.
				old = prev.prov.replacedImage(cur.prov, img)
			}
			if old != "" && old != img {
				return Reason{Kind: BaseImageChanged, Step: s, Old: old, New: img}, true
			}
			continue
		}
		// A git or HTTP source is fetched again in every build; it explains
		// nothing by itself. When what it fetched changed, the step that
		// uses it runs too and is explained as fed by a remote source.
		if cur.prov.fetchSource(s.StepID) {
			continue
		}
		if prev == nil || prev.prov == nil {
			return Reason{Kind: FirstBuild, Step: s}, true
		}
		if cur.noCacheFor(s.Key) {
			return Reason{Kind: NoCache, Step: s}, true
		}
		p, cmp, ok := prevSteps.of(s)
		if !ok || p.StepID == "" {
			// A build whose last steps came from the cache reports only
			// those: BuildKit never loads the steps below a cache hit. Its
			// provenance still holds every step, so the same step (same
			// identity: its operation and all below it) is found there.
			if !prev.prov.has(s.Identity) {
				if ok {
					// The previous build reported the step without its
					// definition (another build's vertex, or a platform
					// whose provenance was not read, as in builds stored
					// before every platform's was): not new. Only its
					// text can be compared; the same text says nothing.
					if k, o, n, ok := argInText(prev, cur, p.Name, s.Name); ok {
						if SecretArg(k) {
							o, n = Masked, Masked
						}
						return Reason{Kind: ArgChanged, Step: s, Arg: k, Old: o, New: n}, true
					}
					if p.Name != s.Name {
						return Reason{Kind: StepChanged, Step: s, Old: p.Name, New: s.Name}, true
					}
					return Reason{}, false
				}
				if len(prev.Steps) < len(prev.prov.Steps) && !prev.complete() {
					// Not reported and not the same: new, or changed.
					return Reason{Kind: StepChanged, Step: s}, true
				}
				return Reason{Kind: NewStep, Step: s}, true
			}
			p = Step{Identity: s.Identity}
		}
		if p.Identity == cmp.Identity {
			if cmp.StepID != s.StepID {
				// The layer did not change: the merge ran because the stage
				// under it did, or for want of cache.
				if r, ok := changedAncestor(prev, cur, s, prevSteps); ok {
					return r, true
				}
			}
			crossDaemon := prev.Daemon != "" && cur.Daemon != "" && prev.Daemon != cur.Daemon
			sameGitContext := cur.ContextCommit != "" && prev.ContextCommit == cur.ContextCommit && !cur.prov.localDependent(s.StepID)
			fromContext := cur.prov.contextDependent(s.StepID) && !sameGitContext
			if rs := cur.prov.remoteSources(s.StepID); len(rs) > 0 && !fromContext {
				// Something fetched from the network feeds the step: its
				// content can change while the definition does not, and no
				// comparison of commits can tell. A step the build context
				// feeds as well is the context's case below: a changed file
				// in it proves the change, and no change found leaves it
				// unclear all the same.
				r := Reason{Kind: RemoteSource, Step: s, Paths: rs}
				if crossDaemon {
					r.Old, r.New = prev.Daemon, cur.Daemon
				}
				return r, true
			}
			if fromContext {
				// The build context feeds this step or an ancestor (the
				// file-reading step may itself be cached or received, so
				// the first executed step says nothing by itself): the
				// files or the cache — unclear. Paths carry everything the
				// context feeds into the whole ancestor cone, not just
				// this step's own sources: the rebuild could have been
				// caused by any of them. Settle or GitLab's comparison of
				// the commits decides it.
				// A source fetched from the network rides along in Paths
				// (RemotePath tells it apart): no comparison of commits can
				// rule it out, so the rebuild never becomes the farm's.
				r := Reason{Kind: CacheElsewhere, Step: s, Paths: append(cur.prov.contextSources(s.StepID), cur.prov.remoteSources(s.StepID)...)}
				if crossDaemon {
					r.Old, r.New = prev.Daemon, cur.Daemon
				}
				return r, true
			}
			// No local source anywhere below the step, or a git context at
			// the same commit as before, and it did not change: with the
			// same identity its cache key is the same, so only the cache
			// can be missing, never the code.
			if crossDaemon {
				return Reason{Kind: OtherNode, Step: s, Old: prev.Daemon, New: cur.Daemon}, true
			}
			return Reason{Kind: NotInCache, Step: s}, true
		}
		oldOp, newOp := prev.prov.Steps[p.StepID].Op, cur.prov.Steps[cmp.StepID].Op
		if !bytes.Equal(oldOp, newOp) {
			if args, o, n, ok := argChange(prev, cur, p.StepID, cmp.StepID); ok {
				if len(args) > 1 {
					// Several arguments: their names, not their values.
					return Reason{Kind: ArgChanged, Step: s, Arg: strings.Join(args, ", ")}, true
				}
				if SecretArg(args[0]) {
					o, n = Masked, Masked
				}
				return Reason{Kind: ArgChanged, Step: s, Arg: args[0], Old: o, New: n}, true
			}
			if k, o, n, ok := argInText(prev, cur, p.Name, cmp.Name); ok && sameCommand(prev.prov, cur.prov, p.StepID, cmp.StepID) {
				if SecretArg(k) {
					o, n = Masked, Masked
				}
				return Reason{Kind: ArgChanged, Step: s, Arg: k, Old: o, New: n}, true
			}
			if o, n, ok := envChange(prev, cur, p, cmp); ok {
				return Reason{Kind: StepChanged, Step: s, Old: o, New: n}, true
			}
			return Reason{Kind: StepChanged, Step: s, Old: p.Name, New: cmp.Name}, true
		}
		// Same operation, different inputs: a step it builds on changed, but
		// came from the cache or another job (a stage shared by parallel
		// jobs). Explain the earliest such step that the previous build did
		// not have, as the reason of this one.
		if r, ok := changedAncestor(prev, cur, s, prevSteps); ok {
			return r, true
		}
		return Reason{Kind: StepChanged, Step: s, Old: p.Name, New: cmp.Name}, true
	}
	return Reason{}, false
}

// prevIndex finds the previous build's step for each of cur's steps, by
// key. COPY --link is two operations under one key: a copy onto an empty
// layer ("LINK COPY ...") and the merge that lays that layer onto the stage
// ("COPY ..."), the layer being the merge's last input. A build lists one
// of the two, not always the same one, and a copy compared with a merge
// reads as a changed line ("LINK COPY ... -> COPY ..."). The graph decides
// instead of the text, and works on a build restored from the store, whose
// graph holds the steps it listed and what they build on:
//   - cur lists the copy, the previous build the merge: the merge's layer
//     is the previous copy;
//   - cur lists the merge, the previous build the copy: cur's layer is
//     compared with it (the previous merge is not in its graph).
type prevIndex struct {
	prev, cur *Build
	byKey     map[string]Step
}

func stepsByKey(prev, cur *Build) prevIndex {
	m := prevIndex{prev: prev, cur: cur, byKey: map[string]Step{}}
	if prev != nil {
		for _, s := range prev.Steps {
			m.byKey[s.Key] = s
		}
	}
	return m
}

// of returns the previous build's step p for cur's step s, and the step of
// cur to compare it with: s itself, or s's layer when s is a COPY --link
// merge and p its copy.
func (m prevIndex) of(s Step) (p, cmp Step, ok bool) {
	p, ok = m.byKey[s.Key]
	if !ok || p.StepID == "" || s.StepID == "" || m.prev.prov == nil || m.cur.prov == nil {
		return p, s, ok
	}
	cur, prev := m.cur.prov.opKind(s.StepID), m.prev.prov.opKind(p.StepID)
	switch {
	case cur == "file" && prev == "merge":
		if l, found := m.prev.layer(p); found {
			return l, s, true
		}
	case cur == "merge" && prev == "file":
		if l, found := m.cur.layer(s); found {
			return p, l, true
		}
	}
	return p, s, ok
}

// layer returns the copy of a COPY --link merge step: its last input, when
// that is a file operation, under the merge's key and line.
func (b *Build) layer(merge Step) (Step, bool) {
	in := b.prov.Steps[merge.StepID].Inputs
	if len(in) == 0 || b.prov.opKind(in[len(in)-1]) != "file" {
		return Step{}, false
	}
	l := merge
	l.StepID = in[len(in)-1]
	l.Identity = b.prov.Identity[l.StepID]
	if !strings.HasPrefix(l.Name, "LINK ") {
		l.Name = "LINK " + l.Name
	}
	return l, true
}

// changedAncestor explains a rebuilt step whose own operation did not
// change by the earliest step it builds on (in the Dockerfile's order)
// that the previous build did not have and that was not executed here:
// received from another job or found in the cache. Its change is the
// reason, s stays the step named. ok is false when no such step explains
// it.
func changedAncestor(prev, cur *Build, s Step, prevSteps prevIndex) (Reason, bool) {
	cone := map[string]bool{}
	cur.prov.walkCone(s.StepID, cone, map[string]bool{})
	for _, a := range cur.Steps {
		if a.StepID == "" || a.StepID == s.StepID || !cone[a.StepID] || prev.prov.has(a.Identity) {
			continue
		}
		pa, ca, ok := prevSteps.of(a)
		if !ok || pa.StepID == "" || (ca.StepID != a.StepID && pa.Identity == ca.Identity) {
			// Not listed before, or a COPY --link whose layer is the same.
			continue
		}
		if img := cur.prov.imageSource(a.StepID); img != "" {
			if old := prev.prov.imageSource(pa.StepID); old != "" && old != img {
				return Reason{Kind: BaseImageChanged, Step: s, Old: old, New: img}, true
			}
			continue
		}
		if !bytes.Equal(prev.prov.Steps[pa.StepID].Op, cur.prov.Steps[ca.StepID].Op) {
			if args, o, n, ok := argChange(prev, cur, pa.StepID, ca.StepID); ok {
				if len(args) > 1 {
					return Reason{Kind: ArgChanged, Step: s, Arg: strings.Join(args, ", ")}, true
				}
				if SecretArg(args[0]) {
					o, n = Masked, Masked
				}
				return Reason{Kind: ArgChanged, Step: s, Arg: args[0], Old: o, New: n}, true
			}
			if k, o, n, ok := argInText(prev, cur, pa.Name, ca.Name); ok && sameCommand(prev.prov, cur.prov, pa.StepID, ca.StepID) {
				if SecretArg(k) {
					o, n = Masked, Masked
				}
				return Reason{Kind: ArgChanged, Step: s, Arg: k, Old: o, New: n}, true
			}
			if o, n, ok := envChange(prev, cur, pa, ca); ok {
				return Reason{Kind: StepChanged, Step: s, Old: o, New: n}, true
			}
			return Reason{Kind: StepChanged, Step: s, Old: pa.Name, New: ca.Name}, true
		}
	}
	return Reason{}, false
}

// noCacheFor says whether the job switched the cache off for the stage of
// the step with this key: for every stage, or for the stage named.
func (b *Build) noCacheFor(key string) bool {
	switch b.NoCache {
	case "":
		return false
	case NoCacheAll:
		return true
	}
	f := strings.Fields(stageOfKey(key)) // "[linux/amd64 build 2/3]": the name is last
	if len(f) == 0 {
		return false
	}
	for _, st := range strings.Split(b.NoCache, ",") {
		if strings.TrimSpace(st) == f[len(f)-1] {
			return true
		}
	}
	return false
}

// argInText finds a build argument whose change alone turns one step text
// into the other: BuildKit writes a step's text with the variables it uses
// already replaced (an ENV set from an ARG, "RUN echo $VERSION"), so the
// Dockerfile line is the same while the text carries the new value.
func argInText(prev, cur *Build, old, new string) (name, from, to string, ok bool) {
	names := make([]string, 0, len(cur.BuildArgs))
	for k := range cur.BuildArgs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		a, b := prev.BuildArgs[k], cur.BuildArgs[k]
		if a == "" || b == "" || a == b || !strings.Contains(old, a) {
			continue
		}
		if strings.ReplaceAll(old, a, b) == new {
			return k, a, b, true
		}
	}
	return "", "", "", false
}

// sameCommand says whether two steps run the same command line: BuildKit
// keeps a RUN's variables unexpanded there (only its displayed text has
// the values), so equal arguments mean the Dockerfile line did not change.
func sameCommand(a, b *Provenance, aID, bID string) bool {
	if a == nil || b == nil {
		return false
	}
	ea, eb := viewOf(a.Steps[aID].Op).Op.Exec, viewOf(b.Steps[bID].Op).Op.Exec
	return ea != nil && eb != nil && slices.Equal(ea.Meta.Args, eb.Meta.Args)
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
		if s.State == Built && s.Error != "" && !Canceled(s.Error) {
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
	// "New" needs the previous build's every step: one that failed, or
	// whose top came from the cache, reported only some of them.
	if prev.Error != "" || !prev.complete() {
		return Reason{}, false
	}
	return Reason{Kind: NewStep, Step: *failed}, true
}

// has reports whether the build's provenance holds a step of this identity.
func (p *Provenance) has(identity string) bool {
	if p == nil || identity == "" {
		return false
	}
	for _, v := range p.Identity {
		if v == identity {
			return true
		}
	}
	return false
}

// replacedImage is the image p used that cur does not, when cur's img is
// not among p's and exactly one of p's images is gone: the image img
// replaced. Empty otherwise.
func (p *Provenance) replacedImage(cur *Provenance, img string) string {
	mine, theirs := map[string]bool{}, map[string]bool{}
	for id := range p.Steps {
		if i := p.imageSource(id); i != "" {
			mine[i] = true
		}
	}
	if mine[img] {
		return ""
	}
	for id := range cur.Steps {
		if i := cur.imageSource(id); i != "" {
			theirs[i] = true
		}
	}
	gone := ""
	for i := range mine {
		if !theirs[i] {
			if gone != "" {
				return ""
			}
			gone = i
		}
	}
	return gone
}

// complete reports whether the build reported a step for every executed or
// cached operation of its provenance that has a Dockerfile line: a build
// whose top came from the cache reports only the steps above the hit.
func (b *Build) complete() bool {
	if b.Truncated {
		// Known when the build was read; a restored build's provenance
		// holds only its reported steps and cannot tell.
		return false
	}
	if b.prov == nil {
		return true
	}
	reported := map[string]bool{}
	for _, s := range b.Steps {
		reported[s.StepID] = true
	}
	for id := range b.prov.Lines {
		if !reported[id] {
			return false
		}
	}
	return true
}

// rebuiltAny reports whether the build executed a step other than resolving
// an image (which happens in every build): only then is there a rebuild to
// explain.
func rebuiltAny(b *Build) bool {
	for i, s := range b.Steps {
		if s.StepID != "" && b.Ran(i) {
			return true
		}
	}
	return false
}

// Ran reports whether the build executed step i itself. A base image and
// a git or HTTP source are resolved again in every build and show as run
// even when nothing was built (the steps that use them come from the
// cache), so they do not count. Without provenance only a FROM line is
// told apart, by its text.
func (b *Build) Ran(i int) bool {
	s := b.Steps[i]
	if s.State != Built {
		return false
	}
	if b.prov != nil && s.StepID != "" {
		return b.prov.imageSource(s.StepID) == "" && !b.prov.fetchSource(s.StepID)
	}
	return !strings.HasPrefix(s.Name, "FROM ")
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

// envChange explains a RUN step whose text and command are the same as
// before but whose environment is not, by an ENV line above it (an ENV is
// no step of its own: it only changes the environment of the RUN steps
// after it). Old and new are written as ENV lines ("ENV NAME=value", "ENV
// NAME" when it was not set); several names are listed without values, and
// a name that looks like a secret has its value masked. ok is false when
// the text, the command or nothing in the environment differs.
func envChange(prev, cur *Build, p, s Step) (string, string, bool) {
	if p.Name != s.Name {
		return "", "", false
	}
	po, co := viewOf(prev.prov.Steps[p.StepID].Op), viewOf(cur.prov.Steps[s.StepID].Op)
	if po.Op.Exec == nil || co.Op.Exec == nil || strings.Join(po.Op.Exec.Meta.Args, "\x00") != strings.Join(co.Op.Exec.Meta.Args, "\x00") {
		return "", "", false
	}
	pe, ce := prev.prov.env(p.StepID), cur.prov.env(s.StepID)
	var changed []string
	for k := range union(pe, ce) {
		if pe[k] != ce[k] {
			changed = append(changed, k)
		}
	}
	if len(changed) == 0 {
		return "", "", false
	}
	sort.Strings(changed)
	if len(changed) > 1 {
		names := "ENV " + strings.Join(changed, ", ")
		return names + " (before)", names + " (now)", true
	}
	k := changed[0]
	line := func(env map[string]string) string {
		v, ok := env[k]
		switch {
		case !ok:
			return "ENV " + k + " (not set)"
		case SecretArg(k):
			return "ENV " + k + "=" + Masked
		}
		return "ENV " + k + "=" + v
	}
	return line(pe), line(ce), true
}

// RemotePath says whether a path of a reason is a source fetched from the
// network (a git or HTTP URL), not a file of the build context.
func RemotePath(p string) bool {
	return strings.HasPrefix(p, "git://") || strings.HasPrefix(p, "https://") || strings.HasPrefix(p, "http://")
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
						// Its CPU and memory are the build's that ran it.
						s.Usage = Usage{}
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
			if j == i || !samePipeline(builds[i], builds[j]) || o.Step.Identity != r.Step.Identity || !sameContext(builds[i], builds[j]) {
				continue
			}
			// A FilesChanged that Settle made itself is no evidence: two
			// builds would settle each other in turn and never be asked
			// about again. Only GitLab's comparison makes one without By.
			if o.Kind == FilesChanged && o.By != "" {
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
		if j == i || !samePipeline(builds[i], o) || o.Daemon == "" || o.Daemon == builds[i].Daemon || !sameContext(builds[i], o) {
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

// sameContext says whether two builds of a pipeline read the same build
// context, so that a step of one identity read the same files in both: the
// folders they named (ContextOpt), else the same image (one target, one
// repository pushed first). Otherwise it cannot be told: a Dockerfile used
// for several services, each with its own folder as the context, gives
// their steps the same identities over different files.
func sameContext(a, b *Build) bool {
	if a.Context != "" && b.Context != "" {
		return a.Context == b.Context
	}
	ra, rb := Repositories(a.Images), Repositories(b.Images)
	return a.Target == b.Target && len(ra) > 0 && len(rb) > 0 && ra[0] == rb[0]
}

func samePipeline(a, b *Build) bool {
	return a.Attribution.State == Attributed && b.Attribution.State == Attributed &&
		a.Attribution.Instance == b.Attribution.Instance && a.Attribution.Pipeline == b.Attribution.Pipeline
}
