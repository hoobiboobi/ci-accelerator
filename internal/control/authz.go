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

// Role is what a caller is: the web reads the pages' data, a router reads
// where projects are warm and reports where it sent jobs.
type Role string

const (
	RoleWeb    Role = "web"
	RoleRouter Role = "router"
	// RoleAny is a static token's (outside a cluster): every role.
	RoleAny Role = "any"
)

// may says whether a caller of role r may use what role want serves.
func (r Role) may(want Role) bool { return r == want || r == RoleAny }

// Errors of the caller's identity.
var (
	ErrUnauthenticated = errors.New("control: no valid token")
	ErrForbidden       = errors.New("control: this caller may not call the controller")
	// ErrReviewUnavailable: Kubernetes could not be asked about the token
	// (the API server restarting or slow); the caller may try again.
	ErrReviewUnavailable = errors.New("control: Kubernetes could not check the token")
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

// StaticToken lets through the callers that send token, in every role.
func StaticToken(token string) func(*http.Request) (Role, error) {
	want := []byte(token)
	return func(r *http.Request) (Role, error) {
		got := []byte(bearer(r))
		if len(got) == 0 || subtle.ConstantTimeCompare(got, want) != 1 {
			return "", ErrUnauthenticated
		}
		return RoleAny, nil
	}
}

// TokenReviewer lets through the ServiceAccounts named in Allow
// (namespace/name), checking each token with Kubernetes and keeping the
// answer for a while.
type TokenReviewer struct {
	Kube     kubernetes.Interface
	Audience string
	Allow    map[string]Role // "namespace/name"
	// Keep is how long an answer is kept (default 5 minutes; a token
	// lives for an hour at least).
	Keep time.Duration
	// Grace is how long a confirmed token stays good while Kubernetes
	// cannot be asked (default 10 minutes: the kubelet renews an hour's
	// projected token after 48 minutes, so one in use was valid for at
	// least 12 more when it was confirmed).
	Grace time.Duration

	mu      sync.Mutex
	answers map[[32]byte]review
}

type review struct {
	role  Role
	err   error
	until time.Time
	stale time.Time
}

// Authorize checks r's token and says the caller's role.
func (t *TokenReviewer) Authorize(r *http.Request) (Role, error) {
	token := bearer(r)
	if token == "" {
		return "", ErrUnauthenticated
	}
	key := sha256.Sum256([]byte(token))
	t.mu.Lock()
	if a, ok := t.answers[key]; ok && time.Now().Before(a.until) {
		t.mu.Unlock()
		return a.role, a.err
	}
	t.mu.Unlock()
	role, err, keep := t.review(r.Context(), token)
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !keep {
		// Kubernetes is away, not the caller wrong: a token it confirmed
		// a short while ago is still good, so the pages keep working
		// through an API server restart.
		if a, ok := t.answers[key]; ok && a.err == nil && now.Before(a.stale) {
			return a.role, nil
		}
		return role, err
	}
	if t.answers == nil {
		t.answers = map[[32]byte]review{}
	}
	if len(t.answers) > 10000 {
		for k, a := range t.answers {
			if !now.Before(a.stale) {
				delete(t.answers, k)
			}
		}
		if len(t.answers) > 10000 {
			t.answers = map[[32]byte]review{}
		}
	}
	k, g := t.Keep, t.Grace
	if k == 0 {
		k = 5 * time.Minute
	}
	if g == 0 {
		g = 10 * time.Minute
	}
	if err != nil {
		k, g = 10*time.Second, 10*time.Second
	}
	t.answers[key] = review{role: role, err: err, until: now.Add(k), stale: now.Add(g)}
	return role, err
}

// review asks Kubernetes; keep says whether the answer may be kept (not
// when Kubernetes could not be asked).
func (t *TokenReviewer) review(ctx context.Context, token string) (Role, error, bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tr, err := t.Kube.AuthenticationV1().TokenReviews().Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{t.Audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrReviewUnavailable, err), false
	}
	if !tr.Status.Authenticated {
		return "", ErrUnauthenticated, true
	}
	ns, name, ok := serviceAccount(tr.Status.User.Username)
	role := t.Allow[ns+"/"+name]
	if !ok || role == "" {
		return "", fmt.Errorf("%w (%s)", ErrForbidden, tr.Status.User.Username), true
	}
	return role, nil, true
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
