package web

import (
	"fmt"
	"strings"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// Status is how a build ended, in one of five words the whole console uses
// (plus running): colour, icon and words together, never colour alone.
type Status struct {
	Key   string // cached, change, farm, unclear, failed, running
	Label string // short: in chips and lists
	Long  string // in full: on a build's own page
	Icon  string
}

var statuses = map[string]Status{
	"cached":   {"cached", "From cache", "From cache", "st_cached"},
	"change":   {"change", "Your change", "Rebuilt · your change", "st_change"},
	"farm":     {"farm", "Build farm", "Rebuilt · build farm", "st_farm"},
	"unclear":  {"unclear", "Unclear", "Rebuilt · node or files, unclear", "st_unclear"},
	"failed":   {"failed", "Failed", "Failed", "st_failed"},
	"canceled": {"failed", "Canceled", "Canceled", "st_failed"},
	"running":  {"running", "Running", "Running", "clock"},
}

// statusKeys are the statuses in the order the pages list them.
var statusKeys = []string{"cached", "change", "farm", "unclear", "failed"}

// statusOf says how a build ended: by its error, the reason of its first
// rebuilt step, and how many steps it executed.
func statusOf(kind history.ReasonKind, buildErr string, built int, running bool) Status {
	switch {
	case running:
		return statuses["running"]
	case canceled(buildErr):
		return statuses["canceled"]
	case buildErr != "":
		return statuses["failed"]
	}
	switch kind {
	case history.FirstBuild, history.StepChanged, history.NewStep, history.ArgChanged, history.BaseImageChanged, history.FilesChanged:
		return statuses["change"]
	case history.OtherNode, history.NotInCache:
		return statuses["farm"]
	case history.CacheElsewhere:
		return statuses["unclear"]
	}
	if built > 0 {
		// Executed steps with nothing to compare them with.
		return statuses["unclear"]
	}
	return statuses["cached"]
}

func rowStatus(b store.BuildRow) Status {
	return statusOf(b.Reason, b.Error, b.Built, b.Completed.IsZero())
}

// whyShort says in a few words why a build took what it took, for a row
// of a list. The build page says it in full.
func whyShort(b store.BuildRow) string {
	st := rowStatus(b)
	switch st.Key {
	case "running":
		return "building"
	case "cached":
		return "every step came from the cache"
	case "failed":
		if st.Label == "Canceled" {
			return "the job was canceled while it built"
		}
		return "failed: " + clip(firstLine(b.Error), 90)
	}
	switch b.Reason {
	case history.FirstBuild:
		return "first build of " + orDefault(b.Target) + " on record"
	case history.FilesChanged:
		if viaVerified(b.Via) && len(b.Paths) > 0 {
			return "files changed: " + strings.Join(clipList(b.Paths, 2), ", ")
		}
		return "the files it copies changed"
	case history.StepChanged, history.NewStep:
		return "its Dockerfile changed"
	case history.ArgChanged:
		return "build argument " + b.Arg + " changed"
	case history.BaseImageChanged:
		return "the base image changed"
	case history.OtherNode:
		return "cached on another node, not on this one"
	case history.NotInCache:
		return "no longer in this node's cache"
	case history.CacheElsewhere:
		if b.Old == "" {
			return "files or an evicted cache, unclear"
		}
		return "last built on another node"
	}
	return fmt.Sprintf("%d step%s ran; nothing to compare with", b.Built, plural(b.Built))
}

// viaVerified says whether a reason's paths are GitLab's verified diff
// hits: only then may a page name them as changed. A witness (reason_by)
// proves THAT files changed, never which.
func viaVerified(via string) bool { return strings.HasPrefix(via, "gitlab ") }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func clipList(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return append(append([]string{}, xs[:n]...), fmt.Sprintf("%d more", len(xs)-n))
}
