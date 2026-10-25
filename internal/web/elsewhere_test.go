package web

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// A no-change outcome is "other node" only when the previous build ran on
// another daemon; with no previous daemon the cache was here last time, so
// the verdict is eviction. One rule for every settling path.
func TestDecideByDiff(t *testing.T) {
	cross := store.Elsewhere{ID: 1, SHA: "b", PrevSHA: "a", Paths: []string{"api"}, Old: "node-a"}
	same := store.Elsewhere{ID: 2, SHA: "b", PrevSHA: "a", Paths: []string{"api"}}
	for _, c := range []struct {
		name     string
		e        store.Elsewhere
		changed  []string
		complete bool
		err      error
		kind     history.ReasonKind
	}{
		{"hit", cross, []string{"api/main.go"}, true, nil, history.FilesChanged},
		{"no hit, previous build elsewhere", cross, []string{"docs/x.md"}, true, nil, history.OtherNode},
		{"no hit, previous build here", same, []string{"docs/x.md"}, true, nil, history.NotInCache},
		{"incomplete stays unclear", same, nil, false, nil, history.CacheElsewhere},
		{"error stays unclear", cross, nil, false, errors.New("boom"), history.CacheElsewhere},
	} {
		got := decideByDiff(c.e, c.changed, c.complete, c.err)
		if got.Kind != c.kind {
			t.Errorf("%s: %s, want %s", c.name, got.Kind, c.kind)
		}
	}
	if noChangeKind(cross) != history.OtherNode || noChangeKind(same) != history.NotInCache {
		t.Error("noChangeKind must key on the previous build's daemon")
	}
}

// No list text may claim "another node" for a build whose previous build
// ran on the same daemon.
func TestUnclearTextsOnOneNode(t *testing.T) {
	row := store.BuildRow{Reason: history.CacheElsewhere, Built: 1, Completed: time.Now()}
	for name, text := range map[string]string{
		"whyShort":   whyShort(row),
		"reasonText": reasonText(row, rowStatus(row)),
	} {
		if strings.Contains(text, "another node") {
			t.Errorf("%s names another node for a same-daemon rebuild: %q", name, text)
		}
	}
	cross := row
	cross.Old = "glab-buildkit-a"
	if !strings.Contains(whyShort(cross), "another node") {
		t.Errorf("whyShort lost the cross-daemon wording: %q", whyShort(cross))
	}
}
