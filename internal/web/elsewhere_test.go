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

// statusOf must call "farm" exactly the kinds history.FarmKinds lists —
// the store's SQL counters derive from the same list, so this test is the
// lock that keeps the Overview and the Nodes page in agreement.
func TestFarmStatusFollowsFarmKinds(t *testing.T) {
	farm := map[history.ReasonKind]bool{}
	for _, k := range history.FarmKinds() {
		farm[k] = true
	}
	kinds := []history.ReasonKind{"", history.FirstBuild, history.StepChanged, history.NewStep,
		history.ArgChanged, history.BaseImageChanged, history.FilesChanged, history.NotInCache,
		history.CacheElsewhere, history.OtherNode}
	for _, k := range kinds {
		got := statusOf(k, "", 1, false).Key == "farm"
		if got != farm[k] {
			t.Errorf("statusOf(%q) farm = %v, history.FarmKinds says %v", k, got, farm[k])
		}
	}
}

// List texts name a path as changed only when GitLab's comparison verified
// it (Via "gitlab a..b"); a witness-settled reason keeps candidates
// unnamed.
func TestFilesChangedNamedOnlyWhenVerified(t *testing.T) {
	witness := store.BuildRow{Reason: history.FilesChanged, Paths: []string{"deps.lock", "services/api"}, Built: 1, Completed: time.Now()}
	verified := witness
	verified.Via = "gitlab 11111111..22222222"
	for name, text := range map[string]string{
		"whyShort":   whyShort(witness),
		"reasonText": reasonText(witness, rowStatus(witness)),
	} {
		if strings.Contains(text, "deps.lock") {
			t.Errorf("%s names an unverified path as changed: %q", name, text)
		}
	}
	if got := whyShort(verified); !strings.Contains(got, "deps.lock") {
		t.Errorf("whyShort dropped the verified paths: %q", got)
	}
	if got := reasonText(verified, rowStatus(verified)); !strings.Contains(got, "deps.lock") {
		t.Errorf("reasonText dropped the verified paths: %q", got)
	}
	// Unverified rows share one Overview group whatever their candidate
	// paths; verified ones group by the named files.
	other := witness
	other.Paths = []string{"payload.txt"}
	if fingerprint(witness, rowStatus(witness)) != fingerprint(other, rowStatus(other)) {
		t.Error("unverified files rows with equal labels landed in different groups")
	}
	if fingerprint(verified, rowStatus(verified)) == fingerprint(witness, rowStatus(witness)) {
		t.Error("a verified files row shares the unverified group")
	}
}
