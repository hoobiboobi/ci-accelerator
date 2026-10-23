package store

import (
	"errors"
	"testing"
	"time"
)

func TestSessions(t *testing.T) {
	s := open(t, t.TempDir(), &clock{t: time.Now()})
	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	a := Session{ID: []byte("a-hash"), Instance: "993196", UserID: 7, Username: "alice", Name: "Alice", Token: []byte("sealed"), Created: t0, Seen: t0}
	if err := s.SaveSession(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.Session(ctx, a.ID)
	if err != nil || got.Username != "alice" || got.UserID != 7 || string(got.Token) != "sealed" || !got.Seen.Equal(t0) {
		t.Fatalf("session = %+v, %v", got, err)
	}
	if _, err := s.Session(ctx, []byte("other")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
	if err := s.SetSessionToken(ctx, a.ID, []byte("sealed-2")); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchSession(ctx, a.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Session(ctx, a.ID)
	if string(got.Token) != "sealed-2" || !got.Seen.Equal(t0.Add(time.Hour)) {
		t.Fatalf("after update: %+v", got)
	}
	if n, err := s.DeleteIdleSessions(ctx, t0.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("a session seen at the bound is not idle: %d, %v", n, err)
	}
	if n, err := s.DeleteIdleSessions(ctx, t0.Add(2*time.Hour)); err != nil || n != 1 {
		t.Fatalf("idle sessions deleted: %d, %v", n, err)
	}
	if err := s.SaveSession(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session: %v", err)
	}
	if err := s.DeleteSession(ctx, a.ID); err != nil {
		t.Fatalf("deleting a gone session: %v", err)
	}
}
