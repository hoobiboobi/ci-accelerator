package fleet

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// "new" is the address of the console's Add a cluster form: neither a
// member nor a join token can take it, through the API or otherwise.
func TestNameNewIsReserved(t *testing.T) {
	ctx := context.Background()
	r, _ := openReg(t)
	_, pub, _ := GenerateKey()
	if _, err := r.Add(ctx, "x", OriginAPI, "new", string(pub)); !errors.Is(err, ErrInvalid) {
		t.Errorf("Add new: %v, want ErrInvalid", err)
	}
	if _, _, err := r.IssueJoin(ctx, "x", "new", 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("IssueJoin new: %v, want ErrInvalid", err)
	}
	if _, err := r.Add(ctx, "x", OriginAPI, "newer", string(pub)); err != nil {
		t.Errorf("Add newer: %v", err)
	}
}

// One person's pages of a member take at most a tenth of the bound: many
// distinct addresses do not push out other people's pages.
func TestLastKnownPerPerson(t *testing.T) {
	l := NewLastKnown(200, 1<<20)
	l.put("eu\x00bob\x00/builds", []byte("<p>bob</p>"), "text/html")
	for i := 0; i < 500; i++ {
		l.put(fmt.Sprintf("eu\x00mallory\x00/builds?q=%d", i), []byte("<p>m</p>"), "text/html")
	}
	if _, _, _, ok := l.Get("eu\x00bob\x00/builds"); !ok {
		t.Error("one person's pages pushed out another person's")
	}
	if _, _, _, ok := l.Get("eu\x00mallory\x00/builds?q=499"); !ok {
		t.Error("the newest page was dropped")
	}
	if _, _, _, ok := l.Get("eu\x00mallory\x00/builds?q=479"); ok {
		t.Error("more than a tenth of the bound kept for one person")
	}
	n := 0
	for k := range l.byKey {
		if owner(k) == "eu\x00mallory\x00" {
			n++
		}
	}
	if n != 20 || l.per["eu\x00mallory\x00"] != 20 {
		t.Errorf("mallory keeps %d pages (counted %d), want 20", n, l.per["eu\x00mallory\x00"])
	}
}
