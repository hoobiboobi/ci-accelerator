package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// The comparison is GitLab's direct one (straight=true: from..to, not
// from the merge base), whose paths are the ones that differ between the
// two commits: a stand-in that answers only that request.
func TestChangedAsksTheDirectComparison(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api/v4/projects/42/repository/compare" || q.Get("straight") != "true" || q.Get("from") != "aaaa" || q.Get("to") != "bbbb" {
			http.Error(w, "not the direct comparison", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"diffs":[{"old_path":"a.txt","new_path":"a.txt","a_mode":"100644","b_mode":"100644"}]}`)
	}))
	defer srv.Close()
	c := New([]string{srv.URL})
	files, complete, err := c.Changed(context.Background(), Token("t"), history.InstancePrefix(srv.URL), 42, "aaaa", "bbbb")
	if err != nil || !complete || strings.Join(files, ",") != "a.txt" {
		t.Errorf("files %v, complete %v, %v", files, complete, err)
	}
}

// An answer larger than MaxCompare is not read to its end: the list is
// not complete, never "complete with no paths".
func TestChangedTooLargeIsIncomplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"diffs":[{"old_path":"a.txt","new_path":"a.txt","a_mode":"100644","b_mode":"100644","diff":"`)
		chunk := strings.Repeat("x", 1<<20)
		for range MaxCompare>>20 + 1 {
			if _, err := fmt.Fprint(w, chunk); err != nil {
				return
			}
		}
		fmt.Fprint(w, `"}]}`)
	}))
	defer srv.Close()
	c := New([]string{srv.URL})
	files, complete, err := c.Changed(context.Background(), Token("t"), history.InstancePrefix(srv.URL), 42, "aaaa", "bbbb")
	if err != nil || complete {
		t.Errorf("an answer over MaxCompare: files %v, complete %v, %v", files, complete, err)
	}
}

// While GitLab asks a credential to wait, the fetcher does not ask it
// for a comparison.
func TestFetcherChangedWaitsWhileRateLimited(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	f := &Fetcher{Client: New([]string{srv.URL})}
	inst := history.InstancePrefix(srv.URL)
	var rl *RateLimited
	if _, _, err := f.Changed(context.Background(), Token("t"), inst, 42, "a", "b"); !errors.As(err, &rl) {
		t.Fatalf("GitLab limits the rate: %v", err)
	}
	if _, _, err := f.Changed(context.Background(), Token("t"), inst, 42, "a", "c"); !errors.As(err, &rl) {
		t.Errorf("while limited: %v", err)
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("GitLab was asked %d times, once while it asked to wait", n)
	}
}

// issued is a person's token of one GitLab.
type issued struct {
	tok, by string
}

func (i issued) Token(context.Context) (string, error)         { return i.tok, nil }
func (i issued) Renew(context.Context, string) (string, error) { return "", ErrRefused }
func (i issued) Who() string                                   { return "person" }
func (i issued) IssuedBy() string                              { return i.by }

// A person's token goes to the GitLab that issued it only.
func TestIssuedTokenGoesToItsGitLabOnly(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := New([]string{"https://gitlab-a.example.com", srv.URL})
	a, b := history.InstancePrefix("https://gitlab-a.example.com"), history.InstancePrefix(srv.URL)
	cred := issued{tok: "at-a", by: a}
	if c.Sends(cred, b) || !c.Sends(cred, a) || !c.Sends(Token("t"), b) {
		t.Errorf("Sends: to b %v, to a %v, an installation token to b %v", c.Sends(cred, b), c.Sends(cred, a), c.Sends(Token("t"), b))
	}
	ctx := context.Background()
	if _, err := c.Pipeline(ctx, cred, b, 5, 7); !errors.Is(err, ErrOtherGitLab) {
		t.Errorf("Pipeline: %v", err)
	}
	if _, err := c.JobLogPart(ctx, cred, b, 5, 7, LogQuery{}); !errors.Is(err, ErrOtherGitLab) {
		t.Errorf("JobLogPart: %v", err)
	}
	if _, _, err := c.Changed(ctx, cred, b, 5, "x", "y"); !errors.Is(err, ErrOtherGitLab) {
		t.Errorf("Changed: %v", err)
	}
	if _, err := c.User(ctx, cred, b); !errors.Is(err, ErrOtherGitLab) {
		t.Errorf("User: %v", err)
	}
	if ok, err := c.CanReadJobs(ctx, cred, b, 5); ok || err != nil {
		t.Errorf("CanReadJobs: %v, %v", ok, err)
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("the other GitLab was asked %d times with a token it did not issue", n)
	}
}
