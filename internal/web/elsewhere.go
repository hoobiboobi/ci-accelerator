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
	pending, err := s.data.Elsewhere(ctx, p, s.now().Add(-31*24*time.Hour), 2000)
	if err != nil {
		return 0, err
	}
	type pair struct{ from, to string }
	type diff struct {
		files    []string
		complete bool
		err      error
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
			out = append(out, store.Settled{ID: e.ID, Kind: history.CacheElsewhere, Via: "unresolved: a commit is not known"})
			continue
		case e.SHA == e.PrevSHA:
			out = append(out, store.Settled{ID: e.ID, Kind: noChangeKind(e), Via: "same commit " + short(e.SHA, 8)})
			continue
		}
		k := pair{e.PrevSHA, e.SHA}
		d := diffs[k]
		if d == nil {
			if stop != nil || calls <= 0 {
				continue
			}
			calls--
			files, complete, err := api.Changed(ctx, p.Instance, p.ID, k.from, k.to)
			var rl *gitlab.RateLimited
			if errors.As(err, &rl) || ctx.Err() != nil {
				stop = err
				continue
			}
			d = &diff{files, complete, err}
			diffs[k] = d
		}
		out = append(out, decideByDiff(e, d.files, d.complete, d.err))
	}
	if err := s.data.SettleElsewhere(ctx, out); err != nil {
		return 0, err
	}
	return len(out), stop
}

// decideByDiff settles one build with the files GitLab says changed
// between the previous build's commit and its own.
func decideByDiff(e store.Elsewhere, changed []string, complete bool, err error) store.Settled {
	via := fmt.Sprintf("gitlab %s..%s", short(e.PrevSHA, 8), short(e.SHA, 8))
	if err != nil {
		return store.Settled{ID: e.ID, Kind: history.CacheElsewhere, Via: "unresolved: " + clip(err.Error(), 120)}
	}
	var hit []string
	for _, f := range changed {
		for _, src := range e.Paths {
			if touches(f, src) {
				hit = append(hit, f)
				break
			}
		}
	}
	switch {
	case len(hit) > 0:
		return store.Settled{ID: e.ID, Kind: history.FilesChanged, Paths: clipList(hit, 20), Via: via}
	case complete && len(e.Paths) > 0:
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

// touches says whether a changed file of the repository is something a
// COPY source reads. The build context may be a folder of the repository,
// so the source is also matched against the file's trailing parts: a
// change then counts as the step's more often than it should, never less.
func touches(file, src string) bool {
	src = strings.TrimSuffix(strings.TrimPrefix(src, "./"), "/")
	if src == "" || src == "." {
		return true
	}
	glob := strings.ContainsAny(src, "*?[")
	for rest := file; ; {
		if rest == src || strings.HasPrefix(rest, src+"/") {
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
