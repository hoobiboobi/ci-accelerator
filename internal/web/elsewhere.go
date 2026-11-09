package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// A rebuild whose cache was on another node (CacheElsewhere) is the
// change's doing when the files its first rebuilt step copies changed
// since the image's previous build, and the build farm's when they did
// not. BuildKit's records cannot tell; GitLab's comparison of the two
// commits can. A comparison never changes, and every image of a pipeline
// shares it.

// settleBatch bounds GitLab requests per pass over a project.
const settleBatch = 25

// settleEvery is how often a project's builds are looked at again.
const settleEvery = 30 * time.Second

// settleLater settles a project's CacheElsewhere builds in the
// background with the reader's GitLab credential, at most once per
// settleEvery.
func (s *Server) settleLater(r *http.Request, p store.ProjectKey) {
	api := s.api(r)
	if api == nil || p.ID == 0 || !api.Knows(p.Instance) {
		return
	}
	s.settleMu.Lock()
	if s.settling == nil {
		s.settling = map[store.ProjectKey]time.Time{}
	}
	if last, ok := s.settling[p]; ok && (last.IsZero() || time.Since(last) < settleEvery) {
		s.settleMu.Unlock()
		return
	}
	s.settling[p] = time.Time{} // running
	s.settleMu.Unlock()
	go func() {
		// The page's values (whose token GitLab is read with) without its
		// deadline: the page is answered long before this ends.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
		defer cancel()
		n, err := s.settle(ctx, api, p, 0, settleBatch)
		if err != nil {
			s.log.Warn("settling rebuilds with GitLab", "project", p.String(), "err", err)
		} else if n > 0 {
			s.log.Info("settled rebuilds with GitLab", "project", p.String(), "builds", n)
		}
		s.settleMu.Lock()
		s.settling[p] = time.Now()
		s.settleMu.Unlock()
		s.statsMu.Lock()
		delete(s.stats, p.String())
		s.statsMu.Unlock()
	}()
}

// settle decides a project's CacheElsewhere builds (only build only, when
// not 0), asking GitLab at most calls times; it returns how many it
// settled either way.
func (s *Server) settle(ctx context.Context, api GitLab, p store.ProjectKey, only int64, calls int) (int, error) {
	var c Changer = api
	if s.opt.Auth != nil && api != nil {
		c = personal{api}
	}
	return SettleProject(ctx, s.data, c, p, s.now(), only, calls)
}

// personal is GitLab read with the reader's own token (sign-in): what it
// may not read is the reader's limit, not the commits'.
type personal struct{ Changer }

func (personal) personal() {}

// SettleData is what settling reads and writes: the open rebuilds, their
// answers, and GitLab's comparisons of commits already asked for (they
// never change, so a rebuild read again is settled without asking again).
type SettleData interface {
	Elsewhere(ctx context.Context, p store.ProjectKey, since time.Time, limit int) ([]store.Elsewhere, error)
	SettleElsewhere(ctx context.Context, settled []store.Settled) error
	Compared(ctx context.Context, p store.ProjectKey, from, to string) ([]string, bool, bool, error)
	// ComparedMarked is Compared for comparisons stored with submodules
	// marked, which a git context's verdict needs.
	ComparedMarked(ctx context.Context, p store.ProjectKey, from, to string) ([]string, bool, bool, error)
	SaveCompare(ctx context.Context, p store.ProjectKey, from, to string, files []string, complete bool) error
}

// Changer tells which files changed between two commits of a project.
type Changer interface {
	Changed(ctx context.Context, instance string, project uint64, from, to string) ([]string, bool, error)
}

// SettleProject decides a project's CacheElsewhere builds of the last 31
// days before now (only build only, when not 0) with GitLab's comparison
// of the commits, asking at most calls times. It returns how many it
// settled, and GitLab's rate limit when it stopped on one. The pages use it
// with the reader's token; the controller with the installation's, or
// with api nil (sign-in on: no token of its own) from the comparisons
// stored before, which never change.
func SettleProject(ctx context.Context, data SettleData, api Changer, p store.ProjectKey, now time.Time, only int64, calls int) (int, error) {
	pending, err := data.Elsewhere(ctx, p, now.Add(-31*24*time.Hour), 2000)
	if err != nil {
		return 0, err
	}
	type pair struct{ from, to string }
	type diff struct {
		files    []string
		complete bool
		err      error
		// marked: submodules are marked (GitLab asked now, or stored so);
		// a git context's verdict needs that.
		marked bool
	}
	diffs := map[pair]*diff{}
	var out []store.Settled
	var stop error
	for _, e := range pending {
		if only != 0 && e.ID != only {
			continue
		}
		switch {
		case e.SHA == "" || e.PrevSHA == "":
			out = append(out, asked(store.Settled{ID: e.ID, Kind: history.CacheElsewhere, Via: "unresolved: a commit is not known"}, e))
			continue
		case e.SHA == e.PrevSHA:
			out = append(out, asked(store.Settled{ID: e.ID, Kind: noChangeKind(e), Via: "same commit " + short(e.SHA, 8)}, e))
			continue
		}
		k := pair{e.PrevSHA, e.SHA}
		d := diffs[k]
		if d != nil && e.Git && !d.marked {
			d = nil // read for a local context from an older comparison
		}
		if d == nil {
			compared := data.Compared
			if e.Git {
				compared = data.ComparedMarked
			}
			files, complete, ok, err := compared(ctx, p, k.from, k.to)
			if err != nil {
				return 0, err
			}
			if ok {
				d = &diff{files, complete, nil, e.Git}
				diffs[k] = d
			}
		}
		if d == nil {
			if api == nil || stop != nil || calls <= 0 {
				continue
			}
			calls--
			files, complete, err := api.Changed(ctx, p.Instance, p.ID, k.from, k.to)
			var rl *gitlab.RateLimited
			var se *gitlab.StatusError
			switch {
			case errors.As(err, &rl) || ctx.Err() != nil:
				stop = err
				continue
			case err != nil && !(errors.As(err, &se) && se.Code >= 400 && se.Code < 500):
				// GitLab or the network failed for now (5xx, a timeout): asked
				// again on a later pass, never stored as unresolved.
				stop = err
				continue
			case err != nil && isPersonal(api) && (se.Code == 401 || se.Code == 403 || se.Code == 404):
				// The reader may not read the repository: another reader may.
				// Nothing is stored for everyone.
				stop = err
				continue
			}
			d = &diff{files, complete, err, true}
			diffs[k] = d
			if err == nil {
				if err := data.SaveCompare(ctx, p, k.from, k.to, files, complete); err != nil {
					return 0, err
				}
			}
		}
		out = append(out, asked(decideByDiff(e, d.files, d.complete, d.err), e))
	}
	if err := data.SettleElsewhere(ctx, out); err != nil {
		return 0, err
	}
	return len(out), stop
}

func isPersonal(c Changer) bool {
	_, ok := c.(interface{ personal() })
	return ok
}

// asked ties an answer to the question it answers.
func asked(s store.Settled, e store.Elsewhere) store.Settled {
	s.Asked, s.Old, s.Open = true, e.Old, e.Paths
	return s
}

// decideByDiff settles one build with the files GitLab says changed
// between the previous build's commit and its own.
func decideByDiff(e store.Elsewhere, changed []string, complete bool, err error) store.Settled {
	via := fmt.Sprintf("gitlab %s..%s", short(e.PrevSHA, 8), short(e.SHA, 8))
	if err != nil {
		return store.Settled{ID: e.ID, Kind: history.CacheElsewhere, Via: "unresolved: " + clip(err.Error(), 120)}
	}
	var hit []string
	seen := map[string]bool{}
	for _, f := range changed {
		// A submodule's new commit is its path with a trailing slash
		// (gitlab.Client.Changed): everything below it changed.
		sub := strings.HasSuffix(f, "/")
		f = strings.TrimSuffix(f, "/")
		if seen[f] {
			continue // a file that became a submodule: one path
		}
		folder := e.GitFolder
		if folder == "" {
			folder = e.Folder
		}
		// A submodule holding the context folder changes the whole
		// context, wherever the context came from.
		if sub && folder != "" && folder != "." && strings.HasPrefix(folder+"/", f+"/") {
			seen[f] = true
			hit = append(hit, f)
			continue
		}
		rel := ""
		if e.GitFolder != "" {
			// A git context is the repository's folder at the commit:
			// only files inside it count, by their path in it.
			var in string
			if rel, in = inFolder(f, e.GitFolder); in == "" {
				continue
			}
		}
		for _, src := range e.Paths {
			if (e.GitFolder == "" && touches(f, src, sub)) || (e.GitFolder != "" && touchesExactly(rel, src, sub)) {
				seen[f] = true
				hit = append(hit, f)
				break
			}
		}
	}
	switch {
	case len(hit) > 0:
		return store.Settled{ID: e.ID, Kind: history.FilesChanged, Paths: clipList(hit, 20), Via: via}
	case complete && len(e.Paths) > 0:
		for _, src := range e.Paths {
			if history.RemotePath(src) {
				// The repository did not change, but the step also builds
				// on something fetched from the network: still unclear.
				return store.Settled{ID: e.ID, Kind: history.CacheElsewhere, Via: via}
			}
		}
		return store.Settled{ID: e.ID, Kind: noChangeKind(e), Via: via}
	}
	return store.Settled{ID: e.ID, Kind: history.CacheElsewhere, Via: "unresolved: GitLab's comparison was cut short"}
}

// noChangeKind is the verdict for a build whose files provably did not
// change — the rule every settling shares (history.Settle applies the same
// one): OtherNode only when the previous build ran on another daemon; with
// no previous daemon the cache was HERE last time, so it was evicted.
func noChangeKind(e store.Elsewhere) history.ReasonKind {
	if e.Old == "" {
		return history.NotInCache
	}
	return history.OtherNode
}

// inFolder is a file's path inside folder ("." for the root) and the file
// itself when it is inside; in is "" when it is not.
func inFolder(file, folder string) (rel, in string) {
	if folder == "." {
		return file, file
	}
	if r, ok := strings.CutPrefix(file, strings.TrimSuffix(folder, "/")+"/"); ok {
		return r, file
	}
	return "", ""
}

// touchesExactly reports whether a copy source, relative to the context,
// covers a file relative to the context: the same path, a folder above it
// or a glob matching it or a folder above it; for a submodule (sub) also
// a source inside it. Unlike touches, the source is never matched against
// the file's trailing path parts: the folder of the context is known.
func touchesExactly(rel, src string, sub bool) bool {
	src = strings.TrimSuffix(strings.TrimPrefix(src, "./"), "/")
	if src == "" || src == "." || rel == src || strings.HasPrefix(rel, src+"/") {
		return true
	}
	if sub && (strings.HasPrefix(src, rel+"/") || globAbove(src, rel)) {
		return true
	}
	if strings.ContainsAny(src, "*?[") {
		for d := rel; d != "." && d != "/" && d != ""; d = path.Dir(d) {
			if ok, _ := path.Match(src, d); ok {
				return true
			}
		}
	}
	return false
}

// globAbove reports whether a path is above what a glob copies: the
// glob's first segments match it (a submodule "vendor/lib" for
// "vendor/*/go.mod").
func globAbove(src, p string) bool {
	ss, ps := strings.Split(src, "/"), strings.Split(p, "/")
	if len(ps) >= len(ss) {
		return false
	}
	ok, _ := path.Match(strings.Join(ss[:len(ps)], "/"), p)
	return ok
}

// touches says whether a changed file of the repository is something a
// COPY source reads. The build context may be a folder of the repository,
// so the source is also matched against the file's trailing parts: a
// change then counts as the step's more often than it should, never less.
// A submodule (sub) counts for the sources inside it too: its new commit
// is one changed path for everything below it.
func touches(file, src string, sub bool) bool {
	src = strings.TrimSuffix(strings.TrimPrefix(src, "./"), "/")
	if src == "" || src == "." {
		return true
	}
	glob := strings.ContainsAny(src, "*?[")
	for rest := file; ; {
		if rest == src || strings.HasPrefix(rest, src+"/") || (sub && (strings.HasPrefix(src, rest+"/") || globAbove(src, rest))) {
			return true
		}
		if glob {
			for d := rest; d != "." && d != "/" && d != ""; d = path.Dir(d) {
				if ok, _ := path.Match(src, d); ok {
					return true
				}
			}
		}
		i := strings.IndexByte(rest, '/')
		if i < 0 {
			return false
		}
		rest = rest[i+1:]
	}
}
