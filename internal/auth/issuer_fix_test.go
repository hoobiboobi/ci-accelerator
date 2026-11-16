package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

type nothingSaved struct{}

func (nothingSaved) SaveAPIPipeline(context.Context, store.APIPipeline) ([]uint64, error) {
	return nil, nil
}

// An admin signed in through one GitLab may open a project of another
// (CanRead says yes): the console reads nothing of that GitLab with the
// admin's token, which only the first one issued, and so renews nothing
// at the first one on the second's refusal.
func TestTokenStaysWithItsGitLab(t *testing.T) {
	e := setup(t)
	var mu sync.Mutex
	var sent []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized) // a token of another GitLab
	}))
	defer other.Close()
	client := gitlab.New([]string{e.gl.srv.URL, other.URL})
	b, err := New(Config{PublicURL: "https://ci.example.com", Key: make([]byte, 32), Now: e.clock.now,
		Instances: []Instance{{URL: e.gl.srv.URL, ClientID: "cid", Secret: "sec", Admins: []string{"id:1"}}, {URL: other.URL, ClientID: "cid", Secret: "sec"}}},
		e.st, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.a = b
	instB := history.InstancePrefix(other.URL)
	root := e.load(t, mustCookie(e.signIn(t, "root", "/")))
	if !root.Admin {
		t.Fatal("root is no admin")
	}
	if ok, _ := b.CanRead(context.Background(), root, instB, 5); !ok {
		t.Fatal("an admin may not read the other GitLab's project")
	}
	e.gl.mu.Lock()
	refreshes := e.gl.refreshes
	e.gl.mu.Unlock()

	ctx := context.Background()
	if _, err := client.Pipeline(ctx, root, instB, 5, 77); err == nil {
		t.Error("a pipeline of the other GitLab was read")
	}
	if _, err := client.JobLogPart(ctx, root, instB, 5, 9001, gitlab.LogQuery{}); err == nil {
		t.Error("a job log of the other GitLab was read")
	}
	if _, _, err := client.Changed(ctx, root, instB, 5, "a", "b"); err == nil {
		t.Error("a comparison of the other GitLab was read")
	}
	if ok, _ := client.CanReadJobs(ctx, root, instB, 5); ok {
		t.Error("the other GitLab's project said readable")
	}
	f := &gitlab.Fetcher{Client: client, Store: nothingSaved{}}
	f.Ensure(root, instB, 5, 78, "running", time.Time{}, 2*time.Second)
	if _, err := f.JobLogPart(ctx, root, instB, 5, 9002, gitlab.LogQuery{}); err == nil {
		t.Error("the fetcher read a job log of the other GitLab")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, s := range sent {
		if strings.Contains(s, "Bearer") {
			t.Errorf("the other GitLab received the admin's token: %s", s)
		}
	}
	e.gl.mu.Lock()
	defer e.gl.mu.Unlock()
	if e.gl.refreshes != refreshes {
		t.Errorf("the other GitLab's refusals renewed the token %d times at its own GitLab", e.gl.refreshes-refreshes)
	}

	// Its own GitLab is read as before.
	if _, err := client.Pipeline(ctx, root, e.inst, 42, 1); err != nil && strings.Contains(err.Error(), "another GitLab") {
		t.Errorf("its own GitLab: %v", err)
	}
}
