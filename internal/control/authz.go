package control

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
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
	// RoleHub is a fleet hub's, over the member's link: it reads what the
	// web reads, but none of the store's writes or the sign-in, and its
	// calls carry no session.
	RoleHub Role = "hub"
	// RoleAny is a static token's (outside a cluster): every role.
	RoleAny Role = "any"
)

// may says whether a caller of role r may use what role want serves.
func (r Role) may(want Role) bool {
	return r == want || r == RoleAny || (r == RoleHub && want == RoleWeb)
}

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
	// Reviews of tokens without a kept answer, bounded per caller address
	// and overall: whoever reaches the API cannot make the controller ask
	// Kubernetes once per request.
	sources map[string]*bucket
	all     bucket
}

// Bounds of the reviews: a caller address may have reviewBurst at once
// and one more every reviewEvery; every caller together allBurst at once
// and one more every allEvery. A legitimate caller needs a review when its
// token is new or its answer is no longer kept (minutes apart).
const (
	reviewBurst = 5
	reviewEvery = time.Second
	allBurst    = 200
	allEvery    = 20 * time.Millisecond
)

// errTooManyReviews: the bounds above were reached; the caller is asked
// to try again (a 503), and a token confirmed lately is still good.
var errTooManyReviews = fmt.Errorf("%w: too many tokens to check at once", ErrReviewUnavailable)

// bucket is a token bucket: up to burst at once, one more every every.
type bucket struct {
	n  float64
	at time.Time
}

func (b *bucket) take(now time.Time, burst int, every time.Duration) bool {
	if b.at.IsZero() {
		b.n = float64(burst)
	} else {
		b.n = min(float64(burst), b.n+float64(now.Sub(b.at))/float64(every))
	}
	b.at = now
	if b.n < 1 {
		return false
	}
	b.n--
	return true
}

// mayReview takes a review for a caller at addr; t.mu is held.
func (t *TokenReviewer) mayReview(addr string, now time.Time) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if t.sources == nil {
		t.sources = map[string]*bucket{}
	}
	if len(t.sources) > 10000 {
		// Drop the addresses whose bucket is full again: they lose nothing.
		for k, b := range t.sources {
			if now.Sub(b.at) >= reviewBurst*reviewEvery {
				delete(t.sources, k)
			}
		}
	}
	b := t.sources[host]
	if b == nil {
		b = &bucket{}
		t.sources[host] = b
	}
	if !b.take(now, reviewBurst, reviewEvery) {
		return false
	}
	return t.all.take(now, allBurst, allEvery)
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
	may := t.mayReview(r.RemoteAddr, time.Now())
	t.mu.Unlock()
	var role Role
	err, keep := errTooManyReviews, false
	if may {
		role, err, keep = t.review(r.Context(), token)
	}
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
		// Refusals go before confirmed tokens: only the allowed
		// ServiceAccounts' tokens are confirmed, a bounded few.
		for k, a := range t.answers {
			if !now.Before(a.stale) || a.err != nil {
				delete(t.answers, k)
			}
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
	// An authenticator that does not know audiences ignores the one asked
	// for and answers none: such a token is not one for the controller.
	if !slices.Contains(tr.Status.Audiences, t.Audience) {
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
