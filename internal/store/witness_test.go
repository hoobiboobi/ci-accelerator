package store

import (
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// PreviousStarted names the start of the build Previous returns, for every
// attributed build of the fixtures, and nothing for one without a
// previous build.
func TestPreviousStartedIsPreviousStart(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	bs := fixtures(t, s.SecretKey())
	saveJobs(t, s, bs)
	for _, b := range bs {
		if _, err := s.SaveBuild(ctx, Build{Build: b, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	var with, without int
	for _, b := range bs {
		if b.Attribution.State != history.Attributed {
			continue
		}
		prev, _, err := s.Previous(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		started, ok, err := s.PreviousStarted(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case prev == nil && ok:
			t.Errorf("%s: no previous build, but PreviousStarted gives %s", b.Ref, started)
		case prev == nil:
			without++
		case !ok || !started.Equal(prev.Build.Started.Truncate(time.Millisecond)):
			t.Errorf("%s: PreviousStarted %s (%v), previous %s started %s", b.Ref, started, ok, prev.Build.Ref, prev.Build.Started)
		default:
			with++
		}
	}
	if with == 0 || without == 0 {
		t.Fatalf("fixtures give %d builds with a previous one and %d without", with, without)
	}
}
