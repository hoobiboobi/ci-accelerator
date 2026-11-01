package store

import (
	"testing"
	"time"
)

// What the reasons need of a build beyond its steps survives the store:
// the job's no-cache option, whether its steps are all of them, and the
// context folder it named. A previous build is always read back from here.
func TestBuildFieldsForReasonsRoundTrip(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	b := *fixtures(t, s.SecretKey())[0]
	b.NoCache, b.Truncated, b.Context = "deps,api", true, "services/api"
	id, err := s.SaveBuild(ctx, Build{Build: &b, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Build; got.NoCache != b.NoCache || !got.Truncated || got.Context != b.Context {
		t.Errorf("read back: no-cache %q, truncated %v, context %q", got.NoCache, got.Truncated, got.Context)
	}
}
