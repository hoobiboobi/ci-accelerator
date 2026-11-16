package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The box's own controller is read whole, as before the fleet: a large
// answer (a thousand builds of long Dockerfiles) is not cut at the bound
// of a member's.
func TestOwnControllerAnswerIsNotCut(t *testing.T) {
	size := MaxMemberAnswer + 1<<20
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/config" {
			w.Write([]byte(`{"Release":"box"}`))
			return
		}
		b, _ := json.Marshal(strings.Repeat("b", size))
		w.Write([]byte(`{"r":[` + string(b) + `]}`))
	}))
	defer ts.Close()
	c, err := Dial(ctx, ClientOptions{URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := c.Output(ctx, 1, 0); err != nil || len(out) != size {
		t.Fatalf("an answer of %d MiB: %d bytes, %v", size>>20, len(out), err)
	}
}

// After a stream that worked, the next connection is tried a second
// later, however often the controller restarted before.
func TestChangeStreamReconnectsSoonAfterAGoodStream(t *testing.T) {
	var streams atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/config" {
			w.Write([]byte(`{"Release":"box"}`))
			return
		}
		streams.Add(1)
		w.WriteHeader(http.StatusOK) // and the stream ends: a controller restart
	}))
	defer ts.Close()
	c, err := Dial(ctx, ClientOptions{URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	run, cancel := context.WithTimeout(context.Background(), 4500*time.Millisecond)
	defer cancel()
	c.Run(run)
	// A second apart: at 0, 1, 2, 3 and 4 s; growing waits would give
	// 0, 1 and 3 s.
	if n := streams.Load(); n < 4 {
		t.Errorf("%d connections in 4.5 s after streams that each worked", n)
	}
}

// The daemons say whether the controller answered: none listed while it
// does not is "not known", not "no daemons".
func TestDaemonsKnown(t *testing.T) {
	down := atomic.Bool{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/config" {
			w.Write([]byte(`{"Release":"box"}`))
			return
		}
		if down.Load() {
			http.Error(w, "restarting", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"r":[[{"Name":"d1"}]]}`))
	}))
	defer ts.Close()
	c, err := Dial(ctx, ClientOptions{URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	if ds, ok := c.DaemonsKnown(); !ok || len(ds) != 1 {
		t.Fatalf("an answering controller: %v, %v", ds, ok)
	}
	down.Store(true)
	c.markLive() // a change: read again
	if ds, ok := c.DaemonsKnown(); ok || len(ds) != 0 {
		t.Errorf("a controller that does not answer: %v, known %v", ds, ok)
	}
}
