package web

import (
	"reflect"
	"slices"
	"testing"

	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// The API's hints of many builds are each build's own (the memo of step
// runs shared between them changes nothing), a missing build is skipped,
// a build without hints is left out, and only the first MaxHintBuilds of a
// list are looked at.
func TestBuildHintsOfAList(t *testing.T) {
	_, st, refs := console(t)
	var ids []int64
	for _, id := range refs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	all, err := st.BuildHints(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	with := int64(0)
	for _, id := range ids {
		s, err := st.Load(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		one, err := st.Hints(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		got, listed := all[id]
		if len(one) == 0 {
			if listed {
				t.Errorf("build %d: listed without hints: %+v", id, got)
			}
			continue
		}
		with = id
		if !reflect.DeepEqual(got, one) {
			t.Errorf("build %d: the list's hints differ from its own:\n%+v\n%+v", id, got, one)
		}
	}
	if with == 0 {
		t.Fatal("fixtures: no build with hints")
	}

	missing := int64(1 << 40)
	if hs, err := st.BuildHints(ctx, []int64{missing, with}); err != nil || len(hs) != 1 || hs[with] == nil {
		t.Errorf("a missing build: %v, %v", hs, err)
	}
	long := make([]int64, store.MaxHintBuilds, store.MaxHintBuilds+1)
	for i := range long {
		long[i] = missing + int64(i)
	}
	if hs, err := st.BuildHints(ctx, append(long, with)); err != nil || len(hs) != 0 {
		t.Errorf("past MaxHintBuilds: %v, %v", hs, err)
	}
	// The MaxHintBuilds-th of a longer list is still looked at.
	edge := append(append(slices.Clone(long[:store.MaxHintBuilds-1]), with), missing-1)
	if hs, err := st.BuildHints(ctx, edge); err != nil || hs[with] == nil {
		t.Errorf("the last one within MaxHintBuilds: %v, %v", hs, err)
	}
}
