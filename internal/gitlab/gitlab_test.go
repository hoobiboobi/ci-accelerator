package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

const page1 = `[{"id":11,"name":"build-api","stage":"build","status":"success","web_url":"https://gl/x/-/jobs/11",
 "created_at":"2026-10-20T21:04:40.739Z","started_at":"2026-10-20T21:04:46.595Z","finished_at":"2026-10-20T21:04:55.373Z",
 "duration":8.777798,"queued_duration":5.380261,"runner":{"description":"test-runner"},
 "pipeline":{"id":7,"project_id":42,"status":"running","web_url":"https://gl/x/-/pipelines/7"}}]`
const page2 = `[{"id":12,"name":"deploy","stage":"deploy","status":"pending","web_url":"https://gl/x/-/jobs/12",
 "created_at":"2026-10-20T21:04:40Z","started_at":null,"finished_at":null,"duration":null,"queued_duration":null,"runner":null,
 "pipeline":{"id":7,"project_id":42,"status":"running","web_url":"https://gl/x/-/pipelines/7"}}]`

func TestPipeline(t *testing.T) {
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("PRIVATE-TOKEN"))
		if r.URL.Path != "/api/v4/projects/42/pipelines/7/jobs" || r.URL.Query().Get("per_page") != "100" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("X-Next-Page", "2")
			fmt.Fprint(w, page1)
			return
		}
		fmt.Fprint(w, page2)
	}))
	defer srv.Close()
	c := New([]string{srv.URL + "/"}, "glpat-secret")
	inst := history.InstancePrefix(srv.URL + "/")
	p, err := c.Pipeline(context.Background(), inst, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Jobs) != 2 || p.Status != "running" || p.WebURL != "https://gl/x/-/pipelines/7" || p.Instance != inst || p.Read.IsZero() {
		t.Fatalf("%+v", p)
	}
	j := p.Jobs[0]
	if j.ID != 11 || j.Name != "build-api" || j.Runner != "test-runner" || j.Duration != 8777798*time.Microsecond ||
		j.Waited.Round(time.Millisecond) != 5380*time.Millisecond || j.Started.Format(time.RFC3339Nano) != "2026-10-20T21:04:46.595Z" ||
		j.Finished.IsZero() || j.Queued.IsZero() {
		t.Errorf("job %+v", j)
	}
	if k := p.Jobs[1]; !k.Started.IsZero() || k.Duration != 0 || k.Runner != "" || k.Status != "pending" {
		t.Errorf("pending job %+v", k)
	}
	if len(tokens) != 2 || tokens[0] != "glpat-secret" {
		t.Errorf("tokens %v", tokens)
	}
	if _, err := c.Pipeline(context.Background(), "abcdef", 42, 7); err == nil {
		t.Error("unknown instance read")
	}
}

func TestRateLimitAndErrors(t *testing.T) {
	var status atomic.Int32
	status.Store(429)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch status.Load() {
		case 429:
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
		case 401:
			w.WriteHeader(401)
		default:
			fmt.Fprint(w, "not json")
		}
	}))
	defer srv.Close()
	c := New([]string{srv.URL}, "t")
	inst := history.InstancePrefix(srv.URL)
	_, err := c.Pipeline(context.Background(), inst, 1, 2)
	rl, ok := err.(*RateLimited)
	if !ok || time.Until(rl.Until) < 25*time.Second || time.Until(rl.Until) > 31*time.Second {
		t.Errorf("429: %v", err)
	}
	status.Store(401)
	if _, err := c.Pipeline(context.Background(), inst, 1, 2); err == nil || strings.Contains(err.Error(), "token") || !strings.Contains(err.Error(), "401") {
		t.Errorf("401: %v", err)
	}
	status.Store(200)
	if _, err := c.Pipeline(context.Background(), inst, 1, 2); err == nil {
		t.Error("bad JSON accepted")
	}
	now := time.Unix(1000, 0)
	h := http.Header{}
	h.Set("RateLimit-Reset", "1090")
	if got := retryAfter(h, now); !got.Equal(time.Unix(1090, 0)) {
		t.Errorf("reset: %v", got)
	}
	if got := retryAfter(http.Header{}, now); got.Sub(now) != time.Minute {
		t.Errorf("default: %v", got)
	}
}

type saver struct {
	mu    sync.Mutex
	saved []store.APIPipeline
}

func (s *saver) SaveAPIPipeline(_ context.Context, p store.APIPipeline) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, p)
	return nil
}

func (s *saver) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saved)
}

// Pages ask for a pipeline as often as they render; GitLab is asked once
// per pipeline at a time, a running one again only after Every, a
// finished one never, and nothing while it rate limits.
func TestFetcher(t *testing.T) {
	var calls atomic.Int32
	limited := atomic.Bool{}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		if limited.Load() {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, page1)
	}))
	defer srv.Close()
	sv := &saver{}
	f := &Fetcher{Client: New([]string{srv.URL}, "t"), Store: sv, Every: time.Hour}
	inst := history.InstancePrefix(srv.URL)
	for i := 0; i < 5; i++ {
		f.Ensure(inst, 42, 7, "", time.Time{}, 0)
	}
	close(release)
	f.Ensure(inst, 42, 7, "", time.Time{}, 2*time.Second)
	if calls.Load() != 1 || sv.count() != 1 {
		t.Fatalf("%d calls, %d saved", calls.Load(), sv.count())
	}
	f.Ensure(inst, 42, 7, "running", time.Now(), time.Second)
	f.Ensure(inst, 42, 8, "success", time.Now(), time.Second)
	f.Ensure(inst, 42, 8, "success", time.Now().Add(-2*time.Hour), time.Second) // finished and read: never again
	f.Ensure("abcdef", 42, 9, "", time.Time{}, time.Second)
	f.Ensure(inst, 0, 9, "", time.Time{}, time.Second)
	if calls.Load() != 1 {
		t.Fatalf("read again: %d calls", calls.Load())
	}
	// A finished pipeline that was never read is read once.
	f.Ensure(inst, 42, 10, "success", time.Time{}, 2*time.Second)
	if calls.Load() != 2 {
		t.Fatalf("finished, unread: %d calls", calls.Load())
	}
	limited.Store(true)
	f.Ensure(inst, 42, 11, "", time.Time{}, 2*time.Second)
	until, err := f.Status()
	if err == nil || time.Until(until) < 50*time.Second {
		t.Fatalf("rate limit not kept: %v %v", until, err)
	}
	f.Ensure(inst, 42, 12, "", time.Time{}, time.Second)
	if calls.Load() != 3 {
		t.Errorf("read while rate limited: %d calls", calls.Load())
	}
	var nilF *Fetcher
	nilF.Ensure(inst, 42, 7, "", time.Time{}, time.Second)
}
