// Package auth signs people in to the console through GitLab and decides
// what each of them may see.
//
// Sign-in is OAuth 2.0 authorization code with PKCE against each GitLab
// the installation serves, as a confidential client. GitLab never calls
// the console: the browser carries the redirects, and the console calls
// GitLab's token endpoint and API. A session lives in the store; the
// cookie holds a random id and the store only its hash, and the person's
// GitLab token is sealed with a key the store does not hold.
//
// A person may see a project when GitLab lets their token read its jobs;
// admins (GitLab usernames named in the configuration) see everything.
package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// Cookie names. The __Host- prefix makes the browser refuse the cookie
// unless it is Secure, set by this host and for every path.
const (
	SessionCookie = "__Host-ci-session"
	LoginCookie   = "__Host-ci-login"
)

// Scopes asked for: an OpenID sign-in and reading the API. GitLab has no
// narrower scope that reads jobs.
var Scopes = []string{"openid", "read_api"}

// Instance is a GitLab the console signs people in with.
type Instance struct {
	// URL is the GitLab's address exactly as the runner has it.
	URL string `json:"url"`
	// ClientID and Secret are the OAuth application's.
	ClientID string `json:"-"`
	Secret   string `json:"-"`
	// Admins are GitLab usernames who see everything in the console.
	Admins []string `json:"admins"`
}

// Config is how people sign in.
type Config struct {
	// PublicURL is where browsers reach the console: https, or http on a
	// loopback address (a port-forward).
	PublicURL string
	Instances []Instance
	// Key seals tokens and the sign-in state: 32 bytes.
	Key []byte
	// Idle ends a session not used for that long (default 7 days).
	Idle time.Duration
	// MaxAge ends a session that long after sign-in, used or not (default
	// 30 days): then GitLab is asked again.
	MaxAge time.Duration
	// Allow and Deny are how long GitLab's answer on a project is kept
	// (default 10 minutes and 90 seconds).
	Allow, Deny time.Duration
	// HTTP calls GitLab's OAuth endpoints (default: 10 s timeout).
	HTTP *http.Client
	// Now replaces the clock in tests.
	Now func() time.Time
}

// Errors of the sign-in.
var (
	// ErrSignedOut: the session is gone, or GitLab no longer takes its token.
	ErrSignedOut = errors.New("auth: signed out")
	// ErrSignIn: the sign-in could not be completed (state, instance, code).
	ErrSignIn = errors.New("auth: the sign-in did not complete")
	// ErrDenied: the person did not allow the console at GitLab.
	ErrDenied = errors.New("auth: access was not allowed at GitLab")
)

// Auth signs people in and answers what they may see.
type Auth struct {
	cfg    Config
	public string
	store  *store.Store
	client *gitlab.Client
	log    *slog.Logger
	aead   cipher.AEAD
	insts  map[string]*inst
	order  []string

	locks sync.Map // session id (hex) -> *sync.Mutex: one refresh at a time
	cache access
}

type inst struct {
	Instance
	prefix, host string
	oauth        *oauth2.Config
	admins       map[string]bool
}

// ParseKey reads a session key: 32 bytes, base64.
func ParseKey(b []byte) ([]byte, error) {
	s := strings.TrimSpace(string(b))
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if k, err := enc.DecodeString(s); err == nil && len(k) == 32 {
			return k, nil
		}
	}
	return nil, errors.New("auth: the session key must be 32 bytes in base64 (openssl rand -base64 32)")
}

// New checks cfg. client must know every instance's URL: the console
// reads GitLab with it.
func New(cfg Config, st *store.Store, client *gitlab.Client, log *slog.Logger) (*Auth, error) {
	if cfg.MaxAge == 0 {
		cfg.MaxAge = 30 * 24 * time.Hour
	}
	if cfg.Idle == 0 {
		cfg.Idle = 7 * 24 * time.Hour
	}
	if cfg.Allow == 0 {
		cfg.Allow = 10 * time.Minute
	}
	if cfg.Deny == 0 {
		cfg.Deny = 90 * time.Second
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	public, err := publicURL(cfg.PublicURL)
	if err != nil {
		return nil, err
	}
	if len(cfg.Key) != 32 {
		return nil, errors.New("auth: the session key must be 32 bytes")
	}
	block, err := aes.NewCipher(cfg.Key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	if len(cfg.Instances) == 0 {
		return nil, errors.New("auth: no GitLab to sign in with")
	}
	a := &Auth{cfg: cfg, public: public, store: st, client: client, log: log, aead: aead, insts: map[string]*inst{}}
	for _, in := range cfg.Instances {
		prefix := history.InstancePrefix(in.URL)
		if in.ClientID == "" || in.Secret == "" {
			return nil, fmt.Errorf("auth: %s: the OAuth application's id and secret are needed", in.URL)
		}
		if !client.Knows(prefix) {
			return nil, fmt.Errorf("auth: %s is not one of the console's GitLab URLs (they must match exactly)", in.URL)
		}
		if a.insts[prefix] != nil {
			return nil, fmt.Errorf("auth: %s is given twice", in.URL)
		}
		u, err := url.Parse(in.URL)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("auth: %q is not a URL", in.URL)
		}
		base := strings.TrimRight(in.URL, "/")
		x := &inst{Instance: in, prefix: prefix, host: u.Host, admins: map[string]bool{}, oauth: &oauth2.Config{
			ClientID: in.ClientID, ClientSecret: in.Secret, Scopes: Scopes,
			Endpoint:    oauth2.Endpoint{AuthURL: base + "/oauth/authorize", TokenURL: base + "/oauth/token", AuthStyle: oauth2.AuthStyleInHeader},
			RedirectURL: public + "/auth/callback/" + prefix,
		}}
		for _, n := range in.Admins {
			x.admins[strings.ToLower(n)] = true
		}
		a.insts[prefix] = x
		a.order = append(a.order, prefix)
	}
	return a, nil
}

// publicURL checks where browsers reach the console: session cookies are
// Secure, so plain http only on a loopback address.
func publicURL(s string) (string, error) {
	u, err := url.Parse(strings.TrimRight(s, "/"))
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("auth: the console's public URL %q is not a plain URL", s)
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", fmt.Errorf("auth: the console's public URL %q must be https (plain http only on localhost)", s)
		}
	default:
		return "", fmt.Errorf("auth: the console's public URL %q must be https", s)
	}
	return u.String(), nil
}

// Choice is a GitLab people can sign in with.
type Choice struct{ Instance, Host string }

// Instances lists the GitLabs people can sign in with, in configured order.
func (a *Auth) Instances() []Choice {
	out := make([]Choice, 0, len(a.order))
	for _, p := range a.order {
		out = append(out, Choice{Instance: p, Host: a.insts[p].host})
	}
	return out
}

// PublicURL is where browsers reach the console.
func (a *Auth) PublicURL() string { return a.public }

// RedirectURL is what the OAuth application of instance must list.
func (a *Auth) RedirectURL(instance string) string {
	if x := a.insts[instance]; x != nil {
		return x.oauth.RedirectURL
	}
	return ""
}

// ---- sealing ----

func (a *Auth) seal(plain, ad []byte) []byte { return a.aead.Seal(nil, nil, plain, ad) }

func (a *Auth) open(sealed, ad []byte) ([]byte, error) { return a.aead.Open(nil, nil, sealed, ad) }

// tokens are what a session keeps of GitLab's answer.
type tokens struct {
	Access  string    `json:"a"`
	Refresh string    `json:"r"`
	Expiry  time.Time `json:"e"`
}

// tokenAD binds a sealed token to its session and instance, so a sealed
// value cannot be moved to another row.
func tokenAD(id []byte, instance string) []byte {
	return append(append([]byte("token\x00"), id...), "\x00"+instance...)
}

func (a *Auth) sealTokens(id []byte, instance string, t tokens) ([]byte, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	return a.seal(b, tokenAD(id, instance)), nil
}

func (a *Auth) openTokens(id []byte, instance string, b []byte) (tokens, error) {
	var t tokens
	plain, err := a.open(b, tokenAD(id, instance))
	if err != nil {
		return t, err
	}
	return t, json.Unmarshal(plain, &t)
}

func random() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported systems
	}
	return b
}

func hash(raw []byte) []byte {
	s := sha256.Sum256(raw)
	return s[:]
}

// ---- sign-in ----

// pending is a sign-in in progress, sealed into a short-lived cookie that
// binds GitLab's answer to this browser.
type pending struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Instance string `json:"i"`
	To       string `json:"t"`
	Expires  int64  `json:"x"`
}

var loginAD = []byte("login")

// SafePath keeps a return address inside the console: a local path, never
// another site.
func SafePath(to string) string {
	if to == "" || to[0] != '/' || strings.HasPrefix(to, "//") || strings.ContainsAny(to, "\\\r\n\t") || strings.HasPrefix(to, "/auth/") {
		return "/"
	}
	u, err := url.Parse(to)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	return to
}

// Begin sends the browser to instance's GitLab to sign in; after it, the
// person lands on to.
func (a *Auth) Begin(w http.ResponseWriter, r *http.Request, instance, to string) error {
	x := a.insts[instance]
	if x == nil {
		return ErrSignIn
	}
	p := pending{State: base64.RawURLEncoding.EncodeToString(random()), Verifier: oauth2.GenerateVerifier(),
		Instance: instance, To: SafePath(to), Expires: a.cfg.Now().Add(10 * time.Minute).Unix()}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: LoginCookie, Value: base64.RawURLEncoding.EncodeToString(a.seal(b, loginAD)),
		Path: "/", MaxAge: 600, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, x.oauth.AuthCodeURL(p.State, oauth2.S256ChallengeOption(p.Verifier)), http.StatusFound)
	return nil
}

// Finish completes a sign-in on instance's callback: it checks GitLab's
// answer against the browser's sign-in, exchanges the code, starts a
// session and returns where the person goes next.
func (a *Auth) Finish(w http.ResponseWriter, r *http.Request, instance string) (string, error) {
	c, err := r.Cookie(LoginCookie)
	http.SetCookie(w, &http.Cookie{Name: LoginCookie, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	if err != nil {
		return "/", fmt.Errorf("%w: no sign-in started in this browser", ErrSignIn)
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return "/", ErrSignIn
	}
	plain, err := a.open(raw, loginAD)
	var p pending
	if err != nil || json.Unmarshal(plain, &p) != nil {
		return "/", ErrSignIn
	}
	q := r.URL.Query()
	x := a.insts[instance]
	switch {
	case x == nil || p.Instance != instance:
		// GitLab's answer came back to another GitLab's address.
		return "/", fmt.Errorf("%w: the answer came from another GitLab", ErrSignIn)
	case a.cfg.Now().Unix() > p.Expires:
		return p.To, fmt.Errorf("%w: it took too long", ErrSignIn)
	case subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(p.State)) != 1:
		return p.To, fmt.Errorf("%w: the answer is not for this sign-in", ErrSignIn)
	case q.Get("error") != "":
		return p.To, ErrDenied
	case q.Get("code") == "":
		return p.To, ErrSignIn
	}
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, a.cfg.HTTP)
	tok, err := x.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(p.Verifier))
	if err != nil {
		return p.To, fmt.Errorf("%w: %v", ErrSignIn, redactErr(err))
	}
	u, err := a.client.User(ctx, gitlab.Token(tok.AccessToken), instance)
	if err != nil {
		return p.To, fmt.Errorf("%w: %v", ErrSignIn, err)
	}
	// A new session every time: an id from before the sign-in is never
	// carried over, and its token is revoked rather than left valid at
	// GitLab with nobody holding it.
	if old, err := r.Cookie(SessionCookie); err == nil {
		if b, err := base64.RawURLEncoding.DecodeString(old.Value); err == nil {
			a.end(r.Context(), hash(b))
		}
	}
	cookie := random()
	id := hash(cookie)
	sealed, err := a.sealTokens(id, instance, tokens{Access: tok.AccessToken, Refresh: tok.RefreshToken, Expiry: a.expiry(tok)})
	if err != nil {
		return p.To, err
	}
	now := a.cfg.Now()
	if err := a.store.SaveSession(r.Context(), store.Session{ID: id, Instance: instance, UserID: u.ID, Username: u.Username,
		Name: u.Name, Token: sealed, Created: now, Seen: now}); err != nil {
		return p.To, err
	}
	a.setSession(w, cookie)
	if a.log != nil {
		a.log.Info("signed in", "gitlab", x.host, "user", u.Username)
	}
	return p.To, nil
}

func (a *Auth) setSession(w http.ResponseWriter, cookie []byte) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: base64.RawURLEncoding.EncodeToString(cookie), Path: "/",
		MaxAge: int(a.cfg.Idle.Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// redactErr keeps an OAuth error's code and drops the response body.
func redactErr(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.ErrorCode != "" {
			return fmt.Errorf("GitLab answered %s", re.ErrorCode)
		}
		if re.Response != nil {
			return fmt.Errorf("GitLab answered %s", re.Response.Status)
		}
	}
	return err
}

// ---- sessions ----

// Viewer is a person signed in. It is the credential the console reads
// GitLab with while serving them.
type Viewer struct {
	Instance string // the GitLab's trace-id prefix
	Host     string // the GitLab's host, to show
	UserID   uint64
	Username string
	Name     string
	Admin    bool

	a  *Auth
	id []byte
}

// touchEvery bounds how often a session's last use is written.
const touchEvery = 10 * time.Minute

// Load returns the person signed in on r, or nil. A session past its idle
// time, of a GitLab no longer configured, or whose token no longer opens
// is ended.
func (a *Auth) Load(w http.ResponseWriter, r *http.Request) (*Viewer, error) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return nil, nil
	}
	cookie, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || len(cookie) != 32 {
		return nil, nil
	}
	id := hash(cookie)
	x, err := a.store.Session(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := a.cfg.Now()
	in := a.insts[x.Instance]
	if in == nil || now.Sub(x.Seen) > a.cfg.Idle || now.Sub(x.Created) > a.cfg.MaxAge {
		return nil, a.store.DeleteSession(r.Context(), id)
	}
	if _, err := a.openTokens(id, x.Instance, x.Token); err != nil {
		// Sealed with another key: the key was changed.
		return nil, a.store.DeleteSession(r.Context(), id)
	}
	if now.Sub(x.Seen) > touchEvery {
		if err := a.store.TouchSession(r.Context(), id, now); err != nil {
			return nil, err
		}
		a.setSession(w, cookie)
	}
	return &Viewer{Instance: x.Instance, Host: in.host, UserID: x.UserID, Username: x.Username, Name: x.Name,
		Admin: in.admins[strings.ToLower(x.Username)], a: a, id: id}, nil
}

// SignOut ends the session on r: the token is revoked at GitLab (so it
// opens nothing any more), the session deleted and the cookie cleared.
func (a *Auth) SignOut(w http.ResponseWriter, r *http.Request) error {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return nil
	}
	cookie, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil
	}
	return a.end(r.Context(), hash(cookie))
}

// end revokes a session's token at GitLab and deletes the session.
func (a *Auth) end(ctx context.Context, id []byte) error {
	x, err := a.store.Session(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if in := a.insts[x.Instance]; in != nil {
		if t, err := a.openTokens(id, x.Instance, x.Token); err == nil {
			if err := a.revoke(ctx, in, t.Access); err != nil && a.log != nil {
				a.log.Warn("token not revoked at GitLab", "gitlab", in.host, "err", err)
			}
		}
	}
	return a.store.DeleteSession(ctx, id)
}

// revoke asks GitLab to revoke a token; GitLab revokes its refresh token
// with it.
func (a *Auth) revoke(ctx context.Context, in *inst, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body := url.Values{"token": {token}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(in.URL, "/")+"/oauth/revoke", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(in.ClientID), url.QueryEscape(in.Secret))
	res, err := a.cfg.HTTP.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("revoke: %s", res.Status)
	}
	return nil
}

// Prune ends the sessions idle for longer than the idle time.
func (a *Auth) Prune(ctx context.Context) (int64, error) {
	return a.store.DeleteIdleSessions(ctx, a.cfg.Now().Add(-a.cfg.Idle), a.cfg.Now().Add(-a.cfg.MaxAge))
}

// ---- the viewer's token ----

// refreshAhead renews a token this long before it expires.
const refreshAhead = time.Minute

// Token returns the viewer's GitLab token, renewed when it is about to
// expire.
func (v *Viewer) Token(ctx context.Context) (string, error) {
	t, err := v.a.tokens(ctx, v)
	if err != nil {
		return "", err
	}
	if t.Expiry.IsZero() || t.Expiry.After(v.a.cfg.Now().Add(refreshAhead)) {
		return t.Access, nil
	}
	return v.a.refresh(ctx, v, t.Access)
}

// Renew is called when GitLab refused refused: the token is renewed unless
// another request renewed it already.
func (v *Viewer) Renew(ctx context.Context, refused string) (string, error) {
	return v.a.refresh(ctx, v, refused)
}

// Who names the viewer for GitLab's rate limits.
func (v *Viewer) Who() string { return fmt.Sprintf("%s/%d", v.Instance, v.UserID) }

func (a *Auth) tokens(ctx context.Context, v *Viewer) (tokens, error) {
	x, err := a.store.Session(ctx, v.id)
	if errors.Is(err, store.ErrNotFound) {
		return tokens{}, ErrSignedOut
	}
	if err != nil {
		return tokens{}, err
	}
	t, err := a.openTokens(v.id, v.Instance, x.Token)
	if err != nil {
		return tokens{}, ErrSignedOut
	}
	return t, nil
}

// refresh renews a session's token, one renewal per session at a time:
// GitLab revokes a refresh token as soon as it is used, so a second,
// concurrent renewal would fail and end the session. stale is the token
// the caller has; when the session already holds a newer one that is not
// about to expire, that one is returned without asking GitLab.
func (a *Auth) refresh(ctx context.Context, v *Viewer, stale string) (string, error) {
	m, _ := a.locks.LoadOrStore(fmt.Sprintf("%x", v.id), &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	t, err := a.tokens(ctx, v)
	if err != nil {
		return "", err
	}
	if t.Access != stale && (t.Expiry.IsZero() || t.Expiry.After(a.cfg.Now().Add(refreshAhead))) {
		return t.Access, nil
	}
	in := a.insts[v.Instance]
	if in == nil || t.Refresh == "" {
		a.store.DeleteSession(ctx, v.id)
		return "", ErrSignedOut
	}
	octx := context.WithValue(ctx, oauth2.HTTPClient, a.cfg.HTTP)
	nt, err := in.oauth.TokenSource(octx, &oauth2.Token{RefreshToken: t.Refresh, Expiry: time.Unix(1, 0)}).Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && (re.ErrorCode == "invalid_grant" || re.ErrorCode == "invalid_client" || re.ErrorCode == "unauthorized_client") {
			// GitLab no longer takes the session's grant: revoked, the
			// person blocked, the application deleted.
			a.store.DeleteSession(ctx, v.id)
			return "", ErrSignedOut
		}
		return "", fmt.Errorf("auth: renewing the GitLab token: %v", redactErr(err))
	}
	next := tokens{Access: nt.AccessToken, Refresh: nt.RefreshToken, Expiry: a.expiry(nt)}
	if next.Refresh == "" {
		next.Refresh = t.Refresh
	}
	sealed, err := a.sealTokens(v.id, v.Instance, next)
	if err != nil {
		return "", err
	}
	// Stored before use: the old pair is gone at GitLab already.
	if err := a.store.SetSessionToken(ctx, v.id, sealed); err != nil {
		return "", err
	}
	return next.Access, nil
}

// expiry is when a token GitLab just gave expires, by the console's clock;
// GitLab gives a lifetime (2 hours unless an admin changed it).
func (a *Auth) expiry(t *oauth2.Token) time.Time {
	if t.ExpiresIn > 0 {
		return a.cfg.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	}
	return t.Expiry
}

// ---- what a viewer may see ----

type answer struct {
	allow bool
	err   error
	until time.Time
}

// access keeps GitLab's answers on projects, per person and project.
type access struct {
	mu       sync.Mutex
	answers  map[string]answer
	inflight map[string]chan struct{}
}

// maxAnswers bounds the kept answers.
const maxAnswers = 20000

// CanRead says whether v may see a project's builds and jobs: admins
// always; others when GitLab lets their token read the project's jobs. A
// project of another GitLab, or no project at all, only admins. GitLab's
// answer is kept for a while; when GitLab cannot answer (down, rate
// limiting) the answer is no, with the error.
func (a *Auth) CanRead(ctx context.Context, v *Viewer, instance string, project uint64) (bool, error) {
	if v == nil {
		return false, nil
	}
	if v.Admin {
		return true, nil
	}
	if project == 0 || instance != v.Instance {
		return false, nil
	}
	key := fmt.Sprintf("%s/%d/%d", instance, v.UserID, project)
	c := &a.cache
	for {
		c.mu.Lock()
		if c.answers == nil {
			c.answers, c.inflight = map[string]answer{}, map[string]chan struct{}{}
		}
		if ans, ok := c.answers[key]; ok && a.cfg.Now().Before(ans.until) {
			c.mu.Unlock()
			return ans.allow, ans.err
		}
		wait, asking := c.inflight[key]
		if !asking {
			done := make(chan struct{})
			c.inflight[key] = done
			c.mu.Unlock()
			allow, err := a.client.CanReadJobs(ctx, v, instance, project)
			ans := answer{allow: allow && err == nil, err: err}
			switch {
			case err != nil && ctx.Err() != nil:
				// The page went away; nothing learned.
			case err != nil:
				ans.until = a.cfg.Now().Add(10 * time.Second)
			case allow:
				ans.until = a.cfg.Now().Add(a.cfg.Allow)
			default:
				ans.until = a.cfg.Now().Add(a.cfg.Deny)
			}
			c.mu.Lock()
			delete(c.inflight, key)
			if !ans.until.IsZero() {
				if len(c.answers) >= maxAnswers {
					c.prune(a.cfg.Now())
				}
				c.answers[key] = ans
			}
			c.mu.Unlock()
			close(done)
			return ans.allow, ans.err
		}
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// prune drops answers past their time, and all of them when that is not
// enough; c.mu is held.
func (c *access) prune(now time.Time) {
	for k, ans := range c.answers {
		if !now.Before(ans.until) {
			delete(c.answers, k)
		}
	}
	if len(c.answers) >= maxAnswers {
		c.answers = map[string]answer{}
	}
}

// ---- context ----

type ctxKey struct{}

// With returns ctx carrying v.
func With(ctx context.Context, v *Viewer) context.Context { return context.WithValue(ctx, ctxKey{}, v) }

// From returns the viewer ctx carries, or nil.
func From(ctx context.Context) *Viewer {
	v, _ := ctx.Value(ctxKey{}).(*Viewer)
	return v
}
