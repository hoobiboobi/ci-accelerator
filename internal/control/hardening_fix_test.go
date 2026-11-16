package control

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/hoobiboobi/ci-accelerator/internal/route"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// A refused token's answer is kept for ten seconds at most (a token
// Kubernetes did not know yet is let in soon after), a confirmed one for
// Keep.
func TestRefusedReviewKeptBriefly(t *testing.T) {
	kube := fake.NewSimpleClientset()
	kube.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		tr := a.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview)
		if tr.Spec.Token == "web-token" {
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, Audiences: []string{Audience},
				User: authnv1.UserInfo{Username: "system:serviceaccount:box:box-web"}}
		}
		return true, tr, nil
	})
	r := &TokenReviewer{Kube: kube, Audience: Audience, Allow: map[string]Role{"box/box-web": RoleWeb}}
	check := func(token string) error {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		_, err := r.Authorize(req)
		return err
	}
	kept := func(token string) time.Duration {
		r.mu.Lock()
		defer r.mu.Unlock()
		a, ok := r.answers[sha256.Sum256([]byte(token))]
		if !ok {
			t.Fatalf("no kept answer for %s", token)
		}
		return time.Until(a.until)
	}
	if err := check("garbage"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("unknown token: %v", err)
	}
	if d := kept("garbage"); d > 10*time.Second || d <= 0 {
		t.Errorf("a refusal is kept for %s, want at most 10 s", d)
	}
	if err := check("web-token"); err != nil {
		t.Fatalf("web: %v", err)
	}
	if d := kept("web-token"); d < 4*time.Minute {
		t.Errorf("a confirmed token is kept for %s, want the default 5 minutes", d)
	}
}

// One report carries at most maxDecisions decisions.
func TestDecisionsReportBounded(t *testing.T) {
	st := newStore(t)
	now := time.Now()
	rt := &Routes{Store: st, Live: daemons{}, Every: time.Hour, Now: func() time.Time { return now }}
	srv := &Server{Local: &Local{Store: st}, Authorize: roles, Routes: rt}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	post := func(n int) int {
		ds := make([]store.Decision, n)
		for i := range ds {
			ds[i] = store.Decision{Project: route.Project{Instance: "0a1b2c", ID: 7}, JobID: uint64(i + 1), Node: "n1", Daemon: "bk-1", Reason: route.ZoneHash, At: now}
		}
		b, _ := json.Marshal(ds)
		req, _ := http.NewRequest("POST", ts.URL+"/internal/v1/route/decisions", strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer router")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if got := post(maxDecisions + 1); got != http.StatusRequestEntityTooLarge {
		t.Errorf("%d decisions: %d, want 413", maxDecisions+1, got)
	}
	if got := post(maxDecisions); got != http.StatusNoContent {
		t.Errorf("%d decisions: %d, want 204", maxDecisions, got)
	}
}
