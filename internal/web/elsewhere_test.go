package web

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
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

type noChange struct{ calls int }

func (c *noChange) Changed(context.Context, string, uint64, string, string) ([]string, bool, error) {
	c.calls++
	return []string{"docs/readme.md"}, true, nil
}

// The controller settles every project's open rebuilds without a page
// being opened: it finds the projects, and each pass closes them.
func TestSettleProjectsWithoutAPage(t *testing.T) {
	_, st, _ := console(t)
	ps, err := st.ElsewhereProjects(ctx, time.Time{})
	if err != nil || len(ps) == 0 {
		t.Fatalf("fixtures: no project with an open rebuild (%v, %v)", ps, err)
	}
	c := &noChange{}
	for _, p := range ps {
		if n, err := SettleProject(ctx, st, c, p, testNow, 0, 25); err != nil || n == 0 {
			t.Errorf("%s: settled %d, %v", p, n, err)
		}
	}
	if left, _ := st.ElsewhereProjects(ctx, time.Time{}); len(left) != 0 {
		t.Errorf("still open: %v", left)
	}
}

type flaky struct{ err error }

func (f flaky) Changed(context.Context, string, uint64, string, string) ([]string, bool, error) {
	return nil, false, f.err
}

// fakeSettle is a store of open rebuilds for SettleProject.
type fakeSettle struct {
	open    []store.Elsewhere
	settled []store.Settled
}

func (f *fakeSettle) Elsewhere(context.Context, store.ProjectKey, time.Time, int) ([]store.Elsewhere, error) {
	return f.open, nil
}

func (f *fakeSettle) SettleElsewhere(_ context.Context, s []store.Settled) error {
	f.settled = append(f.settled, s...)
	return nil
}

// GitLab failing for a moment leaves the rebuilds open; GitLab saying no
// (a 4xx) settles them as unresolved.
func TestSettleKeepsOpenOnATransientError(t *testing.T) {
	open := []store.Elsewhere{{ID: 1, SHA: "b", PrevSHA: "a", Paths: []string{"api"}}, {ID: 2, SHA: "b", PrevSHA: "a", Paths: []string{"web"}}}
	p := store.ProjectKey{Instance: "abcdef", ID: 42}
	f := &fakeSettle{open: open}
	if n, err := SettleProject(ctx, f, flaky{errors.New("dial tcp: i/o timeout")}, p, testNow, 0, 25); err == nil || n != 0 || len(f.settled) != 0 {
		t.Errorf("a network error settled %d (%+v), err %v", n, f.settled, err)
	}
	f = &fakeSettle{open: open}
	SettleProject(ctx, f, flaky{&gitlab.StatusError{Code: 502, Status: "502 Bad Gateway"}}, p, testNow, 0, 25)
	if len(f.settled) != 0 {
		t.Errorf("a 502 settled %+v", f.settled)
	}
	f = &fakeSettle{open: open}
	SettleProject(ctx, f, flaky{&gitlab.StatusError{Code: 404, Status: "404 Not Found"}}, p, testNow, 0, 25)
	if len(f.settled) != 2 || !strings.HasPrefix(f.settled[0].Via, "unresolved") {
		t.Errorf("a 404 did not settle them as unresolved: %+v", f.settled)
	}
}

// A farm rebuild under a replaced daemon on the same node is said as
// such in lists and groups, never "another node".
func TestReplacedDaemonTexts(t *testing.T) {
	b := store.BuildRow{Reason: history.OtherNode, Built: 3, Completed: time.Now(), Daemon: "bk-new", Old: "bk-old", Node: "n1", OldNode: "n1"}
	st := rowStatus(b)
	for name, text := range map[string]string{"whyShort": whyShort(b), "reasonText": reasonText(b, st)} {
		if strings.Contains(text, "another node") || !strings.Contains(text, "replaced") {
			t.Errorf("%s: %q", name, text)
		}
	}
	if fingerprint(b, st) == fingerprint(store.BuildRow{Reason: history.OtherNode, Built: 3, Completed: time.Now(), Daemon: "bk-a", Old: "bk-b", Node: "n1", OldNode: "n2"}, st) {
		t.Error("a replaced daemon grouped with another node")
	}
	b.OldPersists = true
	if text := whyShort(b); text != "no longer in this node's cache" {
		t.Errorf("persistent cache: %q", text)
	}
	moved := b
	moved.OldNode = "n2"
	if text := whyShort(moved); !strings.Contains(text, "another node") {
		t.Errorf("another node: %q", text)
	}
}

// Unclear rebuilds are grouped by what their text says.
func TestUnclearGroupsByText(t *testing.T) {
	now := time.Now()
	rows := []store.BuildRow{
		{Reason: history.CacheElsewhere, Built: 2, Completed: now, Daemon: "bk-new", Old: "bk-old", Node: "n1", OldNode: "n1"},
		{Reason: history.CacheElsewhere, Built: 2, Completed: now, Daemon: "bk-a", Old: "bk-b", Node: "n1", OldNode: "n2"},
		{Reason: history.CacheElsewhere, Built: 2, Completed: now, Daemon: "bk-a"},
		{Built: 2, Completed: now, Daemon: "bk-a"},
	}
	seen := map[string]string{}
	for _, b := range rows {
		st := rowStatus(b)
		fp, text := fingerprint(b, st), reasonText(b, st)
		if other, ok := seen[fp]; ok && other != text {
			t.Errorf("group %s holds %q and %q", fp, other, text)
		}
		seen[fp] = text
	}
	if len(seen) != 4 {
		t.Errorf("groups %v", seen)
	}
}
