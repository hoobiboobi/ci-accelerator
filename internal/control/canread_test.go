package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
)

// fakeReads is a controller that answers CanReadAs by the session it is
// asked for, and counts the calls.
type fakeReads struct {
	mu    sync.Mutex
	calls map[string]int
	may   map[string]bool // session -> answer
	fail  bool
}

func (f *fakeReads) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/internal/v1/config":
		_ = json.NewEncoder(w).Encode(Config{Release: "box"})
	case "/internal/v1/call/CanReadAs":
		f.mu.Lock()
		s := r.Header.Get(SessionHeader)
		f.calls[s]++
		fail, ok := f.fail, f.may[s]
		f.mu.Unlock()
		if fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		b, _ := json.Marshal(ok)
		_ = json.NewEncoder(w).Encode(answer{R: []json.RawMessage{b}})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeReads) count(s string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[s]
}

// The web asks the controller what a person may read and keeps the answer
// for a while: per person (session), never one person's answer for
// another, never for an admin or nobody, and not when the controller
// failed.
func TestCanReadPerSession(t *testing.T) {
	f := &fakeReads{calls: map[string]int{}, may: map[string]bool{"alice": true, "bob": false}}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := Dial(cctx, ClientOptions{URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	person := &auth.Viewer{}
	as := func(s string) (bool, error) {
		return c.CanRead(auth.WithCookie(ctx, s), person, "abcdef", 7)
	}

	if ok, err := as("alice"); !ok || err != nil {
		t.Fatalf("alice: %v %v", ok, err)
	}
	if ok, err := as("bob"); ok || err != nil {
		t.Fatalf("bob got alice's answer: %v %v", ok, err)
	}
	for range 3 {
		if ok, _ := as("alice"); !ok {
			t.Fatal("alice's cached answer changed")
		}
		if ok, _ := as("bob"); ok {
			t.Fatal("bob's cached answer changed")
		}
	}
	if f.count("alice") != 1 || f.count("bob") != 1 {
		t.Errorf("calls %v, want one per person", f.calls)
	}
	// Another project is another question.
	if _, err := c.CanRead(auth.WithCookie(ctx, "alice"), person, "abcdef", 8); err != nil || f.count("alice") != 2 {
		t.Errorf("another project: calls %v, %v", f.calls, err)
	}

	if ok, err := c.CanRead(ctx, nil, "abcdef", 7); ok || err != nil {
		t.Errorf("nobody may read: %v %v", ok, err)
	}
	if ok, err := c.CanRead(auth.WithCookie(ctx, "root"), &auth.Viewer{Admin: true}, "abcdef", 7); !ok || err != nil {
		t.Errorf("an admin may read: %v %v", ok, err)
	}
	if f.count("root") != 0 || f.count("") != 0 {
		t.Errorf("asked the controller for an admin or nobody: %v", f.calls)
	}

	// A failing controller: no answer, nothing kept.
	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
	if ok, err := as("carol"); ok || err == nil {
		t.Fatalf("failing controller: %v %v", ok, err)
	}
	f.mu.Lock()
	f.fail, f.may["carol"] = false, true
	f.mu.Unlock()
	if ok, err := as("carol"); !ok || err != nil {
		t.Errorf("after the failure: %v %v (an error must not be kept as a no)", ok, err)
	}
}
