package history

import (
	"reflect"
	"testing"
)

// A build restored from the store's parts gives the same reasons as the
// build read from its record: as the earlier build, and as both.
func TestRestoredBuildsGiveTheSameReasons(t *testing.T) {
	bs := loadFixtures(t)
	restore := func(b *Build) *Build {
		t.Helper()
		defs := map[string]Def{}
		for _, d := range b.Defs() {
			defs[d.Identity] = d
		}
		c := *b
		c.prov = nil // only exported fields and the defs go through the store
		r, err := Restore(c, defs)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	view := func(r Reason, ok bool) any {
		return []any{ok, r.Kind, r.Step.Key, r.Step.Identity, r.Old, r.New, r.Arg, r.Paths}
	}
	pairs, kinds := 0, map[ReasonKind]bool{}
	for i, cur := range bs {
		for _, prev := range bs[:i] {
			if prev.Target != cur.Target || prev.prov == nil || cur.prov == nil {
				continue
			}
			want := view(FirstRebuild(prev, cur))
			if got := view(FirstRebuild(restore(prev), cur)); !reflect.DeepEqual(got, want) {
				t.Errorf("%s after restored %s: %v, want %v", cur.Ref, prev.Ref, got, want)
			}
			if got := view(FirstRebuild(restore(prev), restore(cur))); !reflect.DeepEqual(got, want) {
				t.Errorf("restored %s after restored %s: %v, want %v", cur.Ref, prev.Ref, got, want)
			}
			r, _ := FirstRebuild(prev, cur)
			kinds[r.Kind] = true
			pairs++
		}
	}
	for _, k := range []ReasonKind{StepChanged, ArgChanged, BaseImageChanged, CacheElsewhere} {
		if !kinds[k] {
			t.Errorf("no pair of fixtures gave %s: the test covers less than it claims", k)
		}
	}
	t.Logf("%d pairs", pairs)
}

func TestRestoreNeedsTheWholeGraph(t *testing.T) {
	b := byJob(t, loadFixtures(t), 1001, "api")
	defs := map[string]Def{}
	for _, d := range b.Defs() {
		defs[d.Identity] = d
	}
	delete(defs, b.Steps[len(b.Steps)-1].Identity) // the last step needs its own definition
	c := *b
	if _, err := Restore(c, defs); err == nil {
		t.Error("restored a build with a definition missing")
	}
}
