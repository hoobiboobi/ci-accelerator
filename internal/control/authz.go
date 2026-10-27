package control

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Who may call the controller: the web (and later the router) prove
// themselves with a ServiceAccount token Kubernetes projects into their
// pods for the controller's audience; the controller asks Kubernetes
// (TokenReview) whose it is. Outside a cluster (development, tests) a
// static token from a file does instead.

// Errors of the caller's identity.
var (
	ErrUnauthenticated = errors.New("control: no valid token")
	ErrForbidden       = errors.New("control: this caller may not call the controller")
)

// Audience is the audience the callers' tokens are projected for.
const Audience = "ci-accelerator-controller"

func bearer(r *http.Request) string {
	t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(t)
}

// StaticToken lets through the callers that send token.
func StaticToken(token string) func(*http.Request) error {
	want := []byte(token)
	return func(r *http.Request) error {
		got := []byte(bearer(r))
		if len(got) == 0 || subtle.ConstantTimeCompare(got, want) != 1 {
			return ErrUnauthenticated
		}
		return nil
	}
}

// TokenReviewer lets through the ServiceAccounts named in Allow
// (namespace/name), checking each token with Kubernetes and keeping the
// answer for a while.
type TokenReviewer struct {
	Kube     kubernetes.Interface
	Audience string
	Allow    map[string]bool // "namespace/name"
	// Keep is how long an answer is kept (default 5 minutes; a token
	// lives for an hour at least).
	Keep time.Duration

	mu      sync.Mutex
	answers map[[32]byte]review
}

type review struct {
	err   error
	until time.Time
}

// Authorize checks r's token.
func (t *TokenReviewer) Authorize(r *http.Request) error {
	token := bearer(r)
	if token == "" {
		return ErrUnauthenticated
	}
	key := sha256.Sum256([]byte(token))
	t.mu.Lock()
	if a, ok := t.answers[key]; ok && time.Now().Before(a.until) {
		t.mu.Unlock()
		return a.err
	}
	t.mu.Unlock()
	err, keep := t.review(r.Context(), token)
	if keep {
		t.mu.Lock()
		if t.answers == nil || len(t.answers) > 10000 {
			t.answers = map[[32]byte]review{}
		}
		k := t.Keep
		if k == 0 {
			k = 5 * time.Minute
		}
		if err != nil {
			k = 10 * time.Second
		}
		t.answers[key] = review{err: err, until: time.Now().Add(k)}
		t.mu.Unlock()
	}
	return err
}

// review asks Kubernetes; keep says whether the answer may be kept (not
// when Kubernetes could not be asked).
func (t *TokenReviewer) review(ctx context.Context, token string) (error, bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tr, err := t.Kube.AuthenticationV1().TokenReviews().Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{t.Audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("%w: Kubernetes could not check the token: %v", ErrUnauthenticated, err), false
	}
	if !tr.Status.Authenticated {
		return ErrUnauthenticated, true
	}
	ns, name, ok := serviceAccount(tr.Status.User.Username)
	if !ok || !t.Allow[ns+"/"+name] {
		return fmt.Errorf("%w (%s)", ErrForbidden, tr.Status.User.Username), true
	}
	return nil, true
}

// serviceAccount splits system:serviceaccount:<namespace>:<name>.
func serviceAccount(user string) (ns, name string, ok bool) {
	rest, ok := strings.CutPrefix(user, "system:serviceaccount:")
	if !ok {
		return "", "", false
	}
	ns, name, ok = strings.Cut(rest, ":")
	return ns, name, ok && ns != "" && name != ""
}

// FileToken reads a token from path (a projected token is renewed in
// place), keeping it a minute.
func FileToken(path string) func() (string, error) {
	var mu sync.Mutex
	var token string
	var read time.Time
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if token != "" && time.Since(read) < time.Minute {
			return token, nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		token, read = strings.TrimSpace(string(b)), time.Now()
		if token == "" {
			return "", fmt.Errorf("%s is empty", path)
		}
		return token, nil
	}
}
