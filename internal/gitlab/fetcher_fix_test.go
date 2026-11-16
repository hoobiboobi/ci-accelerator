package gitlab

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// However many pipelines are asked for, few reads ask GitLab at once and
// a bounded number wait: a caller cannot make the box spend its token's
// rate limit at will.
func TestFetcherBoundsItsReads(t *testing.T) {
	var now, most, calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n := now.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		<-release
		now.Add(-1)
		fmt.Fprint(w, page1)
	}))
	defer srv.Close()
	f := &Fetcher{Client: New([]string{srv.URL}), Store: &saver{}, Every: time.Hour}
	inst := history.InstancePrefix(srv.URL)
	for i := range 1000 {
		f.Ensure(Token("t"), inst, 42, uint64(1000+i), "", time.Time{}, 0)
	}
	time.Sleep(300 * time.Millisecond)
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.inflight)
		f.mu.Unlock()
		if n == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m := most.Load(); m > 16 {
		t.Errorf("%d reads asked GitLab at once, want at most 16", m)
	}
	if c := calls.Load(); c > 256 {
		t.Errorf("%d reads for 1000 asks at once, want at most 256", c)
	}
}

// The times of past reads are not kept for ever: a stream of distinct
// pipelines does not grow the fetcher without end.
func TestFetcherForgetsOldReads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, page1)
	}))
	defer srv.Close()
	f := &Fetcher{Client: New([]string{srv.URL}), Store: &saver{}, Every: time.Millisecond}
	inst := history.InstancePrefix(srv.URL)
	for i := range 3000 {
		f.Ensure(Token("t"), inst, 42, uint64(1+i), "", time.Time{}, time.Second)
	}
	time.Sleep(10 * time.Millisecond)
	f.Ensure(Token("t"), inst, 42, 99999, "", time.Time{}, time.Second)
	f.mu.Lock()
	n := len(f.last)
	f.mu.Unlock()
	if n >= 2048 {
		t.Fatalf("%d read times kept after 3001 reads", n)
	}
}
