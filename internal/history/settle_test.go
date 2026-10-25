package history

import "testing"

func TestSettle(t *testing.T) {
	att := func(pipeline uint64) Attribution {
		return Attribution{State: Attributed, Instance: "abcdef", Pipeline: pipeline, Job: pipeline*10 + 1}
	}
	build := func(ref, daemon string, pipeline uint64, steps ...Step) *Build {
		return &Build{Ref: ref, Daemon: daemon, Attribution: att(pipeline), Steps: steps}
	}
	common := Step{Key: "[app 1/2]", Name: "COPY common/ ./common", Identity: "id-common"}
	cached := func(s Step) Step { s.State = Cached; return s }
	built := func(s Step) Step { s.State = Built; return s }
	elsewhere := Reason{Kind: CacheElsewhere, Step: built(common), Old: "node-a", New: "node-b", Paths: []string{"common"}}
	// The previous build ran on this daemon (no Old): the context feeds the
	// step through an ancestor, so files or an evicted cache.
	sameDaemon := Reason{Kind: CacheElsewhere, Step: built(common), Paths: []string{"common"}}
	files := Reason{Kind: FilesChanged, Step: built(common), Paths: []string{"common"}}

	tests := []struct {
		name    string
		builds  []*Build
		reasons []Reason
		kind    ReasonKind
		by      string
	}{
		{"cached on another daemon in the pipeline", []*Build{build("cur", "node-b", 7, built(common)), build("api", "node-c", 7, cached(common))},
			[]Reason{elsewhere, {}}, OtherNode, "api"},
		// A sibling's cache hit proves the files did not change; with the
		// previous build on THIS daemon that means eviction, never
		// "another node".
		{"same-daemon reason with a cache witness is eviction", []*Build{build("cur", "node-b", 7, built(common)), build("api", "node-c", 7, cached(common))},
			[]Reason{sameDaemon, {}}, NotInCache, "api"},
		{"same-daemon reason with a files witness", []*Build{build("cur", "node-b", 7, built(common)), build("test", "node-c", 7, built(common))},
			[]Reason{sameDaemon, files}, FilesChanged, "test"},
		{"rebuilt for changed files in the pipeline", []*Build{build("cur", "node-b", 7, built(common)), build("test", "node-c", 7, built(common))},
			[]Reason{elsewhere, files}, FilesChanged, "test"},
		{"the cache proof wins", []*Build{build("cur", "node-b", 7, built(common)), build("test", "node-c", 7, built(common)), build("api", "node-d", 7, cached(common))},
			[]Reason{elsewhere, files, {}}, OtherNode, "api"},
		{"cached on the same daemon proves nothing", []*Build{build("cur", "node-b", 7, built(common)), build("api", "node-b", 7, cached(common))},
			[]Reason{elsewhere, {}}, CacheElsewhere, ""},
		{"another pipeline proves nothing", []*Build{build("cur", "node-b", 7, built(common)), build("api", "node-c", 8, cached(common)), build("test", "node-c", 8, built(common))},
			[]Reason{elsewhere, {}, files}, CacheElsewhere, ""},
		{"another step proves nothing", []*Build{build("cur", "node-b", 7, built(common)), build("api", "node-c", 7, cached(Step{Key: "[api 1/2]", Identity: "id-api"}))},
			[]Reason{elsewhere, {}}, CacheElsewhere, ""},
		{"changed files of another step prove nothing", []*Build{build("cur", "node-b", 7, built(common)), build("test", "node-c", 7, built(Step{Key: "[test 1/2]", Identity: "id-all"}))},
			[]Reason{elsewhere, {Kind: FilesChanged, Step: built(Step{Key: "[test 1/2]", Name: "COPY . .", Identity: "id-all"}), Paths: []string{"."}}}, CacheElsewhere, ""},
		{"nothing else in the pipeline", []*Build{build("cur", "node-b", 7, built(common))},
			[]Reason{elsewhere}, CacheElsewhere, ""},
	}
	for _, tc := range tests {
		got := Settle(tc.builds, tc.reasons)
		if got[0].Kind != tc.kind || got[0].By != tc.by {
			t.Errorf("%s: %s by %q, want %s by %q", tc.name, got[0].Kind, got[0].By, tc.kind, tc.by)
		}
		if tc.reasons[0].Kind != CacheElsewhere {
			t.Errorf("%s: the input was changed", tc.name)
		}
	}

	// Unattributed builds belong to no pipeline.
	cur, api := build("cur", "node-b", 7, built(common)), build("api", "node-c", 7, cached(common))
	cur.Attribution.State, api.Attribution.State = Unattributed, Unattributed
	if got := Settle([]*Build{cur, api}, []Reason{elsewhere, {}}); got[0].Kind != CacheElsewhere {
		t.Errorf("unattributed: %s", got[0].Kind)
	}
	// Other reasons pass through.
	if got := Settle([]*Build{cur}, []Reason{files}); got[0].Kind != FilesChanged || got[0].By != "" {
		t.Errorf("files-changed: %+v", got[0])
	}
}
