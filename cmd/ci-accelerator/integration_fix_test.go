package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// A box that starts as a fleet member ends the sessions of its own sign-in,
// fresh ones included: a box that leaves the fleet later takes none back.
func TestMemberEndsOwnSessions(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	for _, id := range []string{"a", "b"} {
		if err := st.SaveSession(ctx, store.Session{ID: []byte(id), Instance: "0a1b2c", UserID: 7, Username: "alice", Token: []byte("sealed"), Created: now, Seen: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := endSessions(ctx, st, quiet()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := st.Session(ctx, []byte(id)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("session %s after joining: %v", id, err)
		}
	}
}
