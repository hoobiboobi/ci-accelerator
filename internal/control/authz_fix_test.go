package control

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// reviewer answers like an audience-aware API server: the web's
// ServiceAccount token for the controller's audience is confirmed, one
// issued for another audience comes back without it, anything else is
// not authenticated. asked counts the reviews.
func reviewer(t *testing.T, asked *atomic.Int32) *TokenReviewer {
	kube := fake.NewSimpleClientset()
	kube.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		asked.Add(1)
		tr := a.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview)
		web := authnv1.UserInfo{Username: "system:serviceaccount:box:box-web"}
		switch tr.Spec.Token {
		case "web-token":
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, Audiences: []string{Audience}, User: web}
		case "web-token-api-audience":
			// An authenticator that ignores the audience asked for: valid,
			// but for the API server, not the controller.
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, User: web}
		case "web-token-other-audience":
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, Audiences: []string{"vault"}, User: web}
		}
		return true, tr, nil
	})
	return &TokenReviewer{Kube: kube, Audience: Audience, Allow: map[string]Role{"box/box-web": RoleWeb}}
}

func authorizeFrom(r *TokenReviewer, addr, token string) (Role, error) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = addr
	req.Header.Set("Authorization", "Bearer "+token)
	return r.Authorize(req)
}

// A token of an allowed ServiceAccount is let through only when the
// review confirms the controller's audience.
func TestTokenReviewChecksTheAudience(t *testing.T) {
	var asked atomic.Int32
	r := reviewer(t, &asked)
	if role, err := authorizeFrom(r, "10.0.0.1:1", "web-token"); err != nil || role != RoleWeb {
		t.Fatalf("the web's token: %s, %v", role, err)
	}
	for _, tok := range []string{"web-token-api-audience", "web-token-other-audience"} {
		if role, err := authorizeFrom(r, "10.0.0.2:1", tok); !errors.Is(err, ErrUnauthenticated) || role != "" {
			t.Errorf("%s: %s, %v", tok, role, err)
		}
	}
}

// Unknown tokens cost Kubernetes a bounded number of reviews, per caller
// address and overall, and the callers' confirmed tokens keep working.
func TestTokenReviewsAreBounded(t *testing.T) {
	var asked atomic.Int32
	r := reviewer(t, &asked)
	if _, err := authorizeFrom(r, "10.0.0.1:1", "web-token"); err != nil {
		t.Fatal(err)
	}
	asked.Store(0)
	for i := range 1000 {
		authorizeFrom(r, "10.9.9.9:1", fmt.Sprintf("guess-%d", i))
	}
	if n := asked.Load(); n > 10 {
		t.Errorf("1000 unknown tokens from one address: %d reviews", n)
	}
	if _, err := authorizeFrom(r, "10.9.9.9:1", "guess-x"); !errors.Is(err, ErrReviewUnavailable) {
		t.Errorf("a review past the bound: %v, want try again", err)
	}
	asked.Store(0)
	for i := range 2000 {
		authorizeFrom(r, fmt.Sprintf("10.8.%d.%d:1", i/250, i%250), fmt.Sprintf("many-%d", i))
	}
	if n := asked.Load(); n > 300 {
		t.Errorf("2000 unknown tokens from 2000 addresses: %d reviews", n)
	}
	if role, err := authorizeFrom(r, "10.0.0.1:1", "web-token"); err != nil || role != RoleWeb {
		t.Errorf("the web's confirmed token after the flood: %s, %v", role, err)
	}
}

// A flood of refused calls writes one log line a minute per route and
// status, not one per call.
func TestRefusedCallsLogBounded(t *testing.T) {
	var buf bytes.Buffer
	srv := &Server{Local: &Local{Store: newStore(t)}, Log: slog.New(slog.NewTextHandler(&buf, nil)),
		Authorize: StaticToken("secret-token-123456")}
	h := srv.Handler()
	for i := range 200 {
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/internal/v1/call/M%d", i), strings.NewReader("[]"))
		req.Header.Set("Authorization", fmt.Sprintf("Bearer wrong-%d", i))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("call %d: %d", i, w.Code)
		}
	}
	if n := strings.Count(buf.String(), "controller API refused a call"); n != 1 {
		t.Errorf("200 refused calls wrote %d lines", n)
	}
}
