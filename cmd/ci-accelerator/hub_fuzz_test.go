package main

// Fuzzing the whole hub process in-process: the top-level handler that
// hubMain builds, wired as hubMain wires it (the registry opened by
// openHubRegistry, sign-in loaded from a sign-in file by loadSignIn with
// the owner named as a GitLab admin and synced into an owner binding of
// origin values, a bootstrap API token file, a secrets key, the hub's
// info), with a fake GitLab behind the sign-in, one connected fake member
// whose controller records every request it receives, and one member
// registered but away.
//
// The fuzzer's bytes decode into a sequence of up to eight HTTP requests:
// a method, a path from the hub's whole route set (pages, the management
// API, /m/<name>/..., the members' connect and join endpoints, sign-in and
// sign-out, health checks, raw hostile paths), an Authorization variant
// (the API tokens of every role, the bootstrap token, bad and malformed
// ones), a Cookie variant (the session of every principal, unknown and
// malformed ones, two session cookies, a session under another name), the
// CSRF headers, and a body: raw bytes, an urlencoded form or a JSON object
// built from the fields the hub's forms and API read.
//
// Every input starts from the same state: the fixtures (members, the
// cluster settings, the role bindings, the API tokens, no join tokens, the
// three sessions, the connected member) are checked and put back first,
// so what one input removed does not make the next one's checks vacuous.
//
// The role a request may act with is bounded from its credentials alone:
// its ceiling is the strongest role of any valid credential it carries (a
// known API token after a Bearer scheme in any case, a known session in a
// cookie named as the session cookie). That bound holds whatever order
// the hub resolves credentials in.
//
// INVARIANTS (each a t.Fatalf, so a violation is a crash the fuzzer keeps):
//   I1. No panic (recovered and reported with its stack) and no 500 from
//       any request.
//   I2. Every request returns within a bound after its context ends;
//       exceeding it is a hang finding.
//   I3. A request whose ceiling is nobody or viewer changes nothing in the
//       registry: members, role bindings, API tokens (but their last use),
//       join tokens, cluster settings and secrets, and the audit trail are
//       identical before and after it. Viewers have no hub permission.
//   I4. A request whose ceiling is operator changes no role binding and no
//       API token (roles.write and tokens.write are the owner's).
//   I5. A request whose ceiling is below operator never receives a
//       cluster's settings (the gitlab_url sentinel) or a role binding's
//       label (the label sentinel) in a response body.
//   I6. No response body contains a kept secret value (a cluster's runner
//       token), the secrets key, any API token secret or the bootstrap
//       token, or any session cookie value ever issued.
//   I7. A member's controller never receives the hub's session: no request
//       to it carries X-CI-Session, and no header of it holds a session
//       value, an API token secret or the bootstrap token.
//   I8. A cross-origin state-changing request that reaches the sign-in
//       gate with a valid session (or any request under /auth/) is refused
//       with 403 and changes nothing in the registry.
//   I9. A request to the management API with a Bearer token the hub does
//       not know is answered 401, whatever session cookie comes with it (a
//       token comes without a session).
//   I10. Non-vacuity, on the first request of an input (the fixtures are
//       in place then): an owner or operator signed in (cookie only) sees
//       the cluster settings sentinel on GET /clusters/eu and the label
//       sentinel on GET /access; an owner's GET of the connected member's
//       pages answers 200 and reached the member's controller. Without
//       these, I5 and I7 could pass because nothing was there to leak.
//   I11. The fixtures can be put back before every input (a harness
//       check: a state the reset cannot undo is reported).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/control"
	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// Sentinels that must never leak. They are not derivable from the
// decoder's own byte alphabet, so a match in a response is a real leak,
// not the fuzzer echoing its input.
const (
	fzClusterSentinel = "GL-ZZQ7X3-CLUSTER-GITLAB-URL-SENTINEL"
	fzLabelSentinel   = "LBL-ZZQ7X3-ROLE-BINDING-LABEL-SENTINEL"
	fzRunnerSecret    = "glrt-ZZQ7X3-RUNNER-TOKEN-SECRET-VALUE"
	fzSecretsKey      = "ZZQ7X3-secrets-key-0123456789abcdef0123"
	fzBootstrap       = "cia_boot_ZZQ7X3_0123456789abcdefghij"
	// fzReqWait bounds one request's context; fzHang is how long after it
	// the request must have returned (I2).
	fzReqWait = 2 * time.Second
	fzHang    = 8 * time.Second
	fzActor   = "local"
	fzMaxKept = 64 // ever-issued secrets remembered for I6/I7
)

// fzTrace: FUZZ_HUB_TRACE=1 logs every request's answer (to read what the
// seeds reach).
var fzTrace = os.Getenv("FUZZ_HUB_TRACE") != ""

func fzQuiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fzRecorder is a fake member controller that records, for every request
// it serves, all its headers.
type fzRecorder struct {
	h    http.Handler
	mu   sync.Mutex
	hits int
	seen []string // one line per request: every header name and value
}

func (m *fzRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	for k, vs := range r.Header {
		for _, v := range vs {
			b.WriteString(k + ": " + v + "\n")
		}
	}
	m.mu.Lock()
	m.hits++
	m.seen = append(m.seen, b.String())
	m.mu.Unlock()
	m.h.ServeHTTP(w, r)
}

func (m *fzRecorder) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits
}

// all is every request received since the last reset (one input's).
func (m *fzRecorder) all() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.seen...)
}

func (m *fzRecorder) reset() {
	m.mu.Lock()
	m.seen = nil
	m.mu.Unlock()
}

// fzHub is the standing hub and everything needed to judge a request.
type fzHub struct {
	top    http.Handler
	hub    *hub
	reg    *fleet.Registry
	signIn *auth.Auth
	sealer *fleet.Sealer
	ts     *httptest.Server // the hub's address for the members' links
	hubPEM []byte
	host   string // the signed-in GitLab host (for subjects)
	inst   string // its trace-id prefix

	member   *fzRecorder
	euCancel context.CancelFunc
	euFP     string

	// sessions: the current session cookie of each signed-in principal.
	sessions map[fleet.Role]string
	users    map[fleet.Role]string
	subjects map[fleet.Role]string // the person each session belongs to
	// tokens: active API token secret -> role (the bootstrap token too);
	// tokByName: the fixture tokens by name; tokHint their hints.
	tokens    map[string]fleet.Role
	tokByName map[string]string
	tokHint   map[string]string
	// ever: every session value and token secret issued, for I6 and I7.
	ever []string

	poolPEM  string // a valid member public key the decoder may send
	canon    string // the fixtures' shape after setup
	canonEU  string
	euValues map[string]string
	ownerSub string
	opSub    string
	victim   string
}

var fzTokenFixtures = []struct {
	name string
	role fleet.Role
}{{"owner-tok", fleet.Owner}, {"op-tok", fleet.Operator}, {"viewer-tok", fleet.Viewer}}

// newFzGitLab answers the sign-in dance: the code is the username, the
// token is "at-<code>", /api/v4/user maps a few names to ids, and the
// jobs API lets nobody read anything (404), so a viewer stays a viewer.
func newFzGitLab(tb testing.TB) *httptest.Server {
	ids := map[string]int{"owner": 1, "op": 2, "vwr": 3}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/oauth/token":
			r.ParseForm()
			json.NewEncoder(w).Encode(map[string]any{"access_token": "at-" + r.Form.Get("code"),
				"refresh_token": "rt-" + r.Form.Get("code"), "token_type": "Bearer", "expires_in": 7200})
		case r.URL.Path == "/oauth/revoke":
			fmt.Fprint(w, "{}")
		case r.URL.Path == "/api/v4/user":
			user := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer at-")
			id := ids[user]
			if id == 0 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": id, "username": user, "name": strings.ToUpper(user)})
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"404 Not Found"}`)
		}
	}))
	tb.Cleanup(srv.Close)
	return srv
}

func fzWrite(tb testing.TB, dir, name, content string) string {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		tb.Fatal(err)
	}
	return p
}

// buildFzHub stands up the hub once per fuzzing process, as hubMain does.
func buildFzHub(tb testing.TB) *fzHub {
	ctx := context.Background()
	log := fzQuiet()
	gl := newFzGitLab(tb)
	glURL := gl.URL + "/"
	u, _ := url.Parse(gl.URL)
	urls := []string{glURL}

	// The files hubMain reads.
	files := tb.TempDir()
	data := tb.TempDir()
	priv, _, err := fleet.GenerateKey()
	if err != nil {
		tb.Fatal(err)
	}
	keyFile := fzWrite(tb, files, "hub.key", string(priv))
	tokenFile := fzWrite(tb, files, "api-token", fzBootstrap+"\n")
	secretsFile := fzWrite(tb, files, "secrets-key", fzSecretsKey)
	sessionKey := fzWrite(tb, files, "session-key", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	cid := fzWrite(tb, files, "client-id", "cid")
	csec := fzWrite(tb, files, "client-secret", "sec")
	sif, _ := json.Marshal(map[string]any{"url": "http://localhost", "sessionKeyFile": sessionKey,
		"gitlab": []map[string]any{{"url": glURL, "clientIdFile": cid, "secretFile": csec, "admins": []string{"id:1"}}}})
	signInFile := fzWrite(tb, files, "sign-in.json", string(sif))

	kb, err := os.ReadFile(keyFile)
	if err != nil {
		tb.Fatal(err)
	}
	key, err := fleet.ParsePrivateKey(kb)
	if err != nil {
		tb.Fatal(err)
	}
	token := &bootstrapToken{path: tokenFile, log: log}
	st, err := store.Open(ctx, data, store.Options{Keep: 24 * time.Hour, KeepOutput: 1})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { st.Close() })
	reg, err := openHubRegistry(ctx, data, time.Now(), log)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { reg.Close() })
	reg.KeepRemoved = 30 * 24 * time.Hour
	sb, err := os.ReadFile(secretsFile)
	if err != nil {
		tb.Fatal(err)
	}
	sealer, err := fleet.NewSealer(sb)
	if err != nil {
		tb.Fatal(err)
	}
	client := gitlab.New(urls)
	client.Log = log
	signIn, err := loadSignIn(signInFile, urls, st, client, log)
	if err != nil {
		tb.Fatal(err)
	}
	var owners []string
	for _, a := range signIn.Admins() {
		owners = append(owners, fleet.PersonSubject(a.Host, a.UserID))
	}
	signIn.Elevate = func(host string, id uint64) bool {
		return reg.RoleOf(fleet.PersonSubject(host, id)).Allows(fleet.PermAllProjects)
	}
	if err := reg.SyncOwners(ctx, owners); err != nil {
		tb.Fatal(err)
	}
	info := fleet.HubInfo{URL: "http://localhost"}
	hubPEM, err := fleet.PublicPEM(key.Public())
	if err != nil {
		tb.Fatal(err)
	}
	info.PublicKeys = string(hubPEM)
	h, top, err := newHub(ctx, hubConfig{reg: reg, log: log, key: key, token: token, signIn: signIn, sealer: sealer,
		version: "fuzz", info: info})
	if err != nil {
		tb.Fatal(err)
	}
	ts := httptest.NewServer(top)
	tb.Cleanup(ts.Close)

	host := u.Host
	fh := &fzHub{top: top, hub: h, reg: reg, signIn: signIn, sealer: sealer, ts: ts, hubPEM: hubPEM,
		host: host, inst: history.InstancePrefix(glURL),
		sessions: map[fleet.Role]string{}, users: map[fleet.Role]string{fleet.Owner: "owner", fleet.Operator: "op", fleet.Viewer: "vwr"},
		tokens: map[string]fleet.Role{fzBootstrap: fleet.Owner}, tokByName: map[string]string{}, tokHint: map[string]string{},
		ever: []string{fzBootstrap}, ownerSub: fleet.PersonSubject(host, 1), opSub: fleet.PersonSubject(host, 2),
		victim: fleet.PersonSubject(host, 99),
		subjects: map[fleet.Role]string{fleet.Owner: fleet.PersonSubject(host, 1), fleet.Operator: fleet.PersonSubject(host, 2),
			fleet.Viewer: fleet.PersonSubject(host, 3)},
		euValues: map[string]string{"gitlab_url": "https://" + fzClusterSentinel + ".example.com/",
			"storage": "emptyDir", "runner_token": fzRunnerSecret}}
	if len(owners) != 1 || owners[0] != fh.ownerSub {
		tb.Fatalf("the sign-in file's admins are %v, want %s", owners, fh.ownerSub)
	}
	_, ppub, err := fleet.GenerateKey()
	if err != nil {
		tb.Fatal(err)
	}
	fh.poolPEM = string(ppub)

	// The member's controller, behind a recorder; one for every link.
	mstore, err := store.Open(ctx, tb.TempDir(), store.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { mstore.Close() })
	csrv := &control.Server{Local: &control.Local{Store: mstore}, Config: control.ConfigFor("fuzz", "box", nil, nil), Log: log}
	fh.member = &fzRecorder{h: csrv.Handler()}
	tb.Cleanup(func() {
		if fh.euCancel != nil {
			fh.euCancel()
		}
	})

	// Roles: owner from the values (above), an operator, and a labelled
	// operator binding nobody signs in as: the Access page's sentinel.
	if _, err := reg.Grant(ctx, fzActor, fleet.OriginLocal, fh.opSub, fleet.Operator); err != nil {
		tb.Fatal(err)
	}
	if _, err := reg.Grant(ctx, fzActor, fleet.OriginLocal, fh.victim, fleet.Operator); err != nil {
		tb.Fatal(err)
	}
	if err := reg.NoteName(ctx, fh.victim, fzLabelSentinel); err != nil {
		tb.Fatal(err)
	}
	fh.connectEU(tb)
	fh.addUS(tb)
	if _, err := reg.SaveSettings(ctx, sealer, fzActor, "eu", fh.euValues); err != nil {
		tb.Fatal(err)
	}
	for _, tf := range fzTokenFixtures {
		fh.issue(tb, tf.name, tf.role)
	}
	for _, role := range []fleet.Role{fleet.Owner, fleet.Operator, fleet.Viewer} {
		fh.signInAs(tb, role)
	}
	// The labels sign-in notes for the owner and the operator are recorded
	// at their first request: make one now, so the shape is settled.
	for _, role := range []fleet.Role{fleet.Owner, fleet.Operator} {
		rq := httptest.NewRequest("GET", "http://localhost/api/v1/fleet/version", nil)
		rq.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: fh.sessions[role]})
		fh.top.ServeHTTP(httptest.NewRecorder(), rq)
	}
	fh.canon = fh.shape(ctx)
	fh.canonEU = fh.settingsShape(ctx, "eu")
	if !strings.Contains(fh.canonEU, "gitlab_url") {
		tb.Fatalf("the cluster settings fixture did not save: %s", fh.canonEU)
	}
	return fh
}

func (fh *fzHub) remember(s string) {
	fh.ever = append(fh.ever, s)
	if len(fh.ever) > fzMaxKept {
		fh.ever = append([]string{fzBootstrap}, fh.ever[len(fh.ever)-fzMaxKept+1:]...)
	}
}

func (fh *fzHub) issue(tb testing.TB, name string, role fleet.Role) {
	t, secret, err := fh.reg.IssueToken(context.Background(), fzActor, name, role, 0)
	if err != nil {
		tb.Fatalf("issuing the fixture token %s: %v", name, err)
	}
	if old := fh.tokByName[name]; old != "" {
		delete(fh.tokens, old)
	}
	fh.tokByName[name], fh.tokHint[name], fh.tokens[secret] = secret, t.Hint, role
	fh.remember(secret)
}

// connectEU (re)registers the member "eu" under a new key and links it to
// the hub; it waits until its pages are up.
func (fh *fzHub) connectEU(tb testing.TB) {
	ctx := context.Background()
	if fh.euCancel != nil {
		fh.euCancel()
	}
	fh.reg.Remove(ctx, fzActor, "eu")
	fh.hub.drop("eu")
	mpriv, mpub, err := fleet.GenerateKey()
	if err != nil {
		tb.Fatal(err)
	}
	m, err := fh.reg.Add(ctx, fzActor, fleet.OriginAPI, "eu", string(mpub))
	if err != nil {
		tb.Fatalf("registering eu: %v", err)
	}
	fh.euFP = m.Fingerprint
	mkey, err := fleet.ParsePrivateKey(mpriv)
	if err != nil {
		tb.Fatal(err)
	}
	hubKeys, err := fleet.ParsePublicKeys(fh.hubPEM)
	if err != nil {
		tb.Fatal(err)
	}
	d := &fleet.Dialer{Hub: fh.ts.URL, Name: "eu", Store: "0123456789ab", Version: "fuzz", Key: mkey,
		HubKeys: hubKeys, Handler: fh.member, Log: fzQuiet()}
	dctx, cancel := context.WithCancel(context.Background())
	fh.euCancel = cancel
	go d.Run(dctx)
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if fh.hub.status("eu").Connected {
			return
		}
	}
	tb.Fatal("the fake member eu did not connect")
}

func (fh *fzHub) addUS(tb testing.TB) {
	_, upub, err := fleet.GenerateKey()
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := fh.reg.Add(context.Background(), fzActor, fleet.OriginAPI, "us", string(upub)); err != nil {
		tb.Fatalf("registering us: %v", err)
	}
}

// signInAs runs the OAuth dance against the hub's own handler and keeps
// the session cookie value.
func (fh *fzHub) signInAs(tb testing.TB, role fleet.Role) {
	user := fh.users[role]
	rec := httptest.NewRecorder()
	fh.top.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/sign-in/"+fh.inst+"?to=/", nil))
	var login *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.LoginCookie {
			login = c
		}
	}
	if login == nil {
		tb.Fatalf("sign-in for %s: no login cookie (%d)", user, rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	state := loc.Query().Get("state")
	cb := httptest.NewRequest("GET", "/auth/callback/"+fh.inst+"?code="+user+"&state="+url.QueryEscape(state), nil)
	cb.AddCookie(login)
	rec = httptest.NewRecorder()
	fh.top.ServeHTTP(rec, cb)
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookie && c.Value != "" {
			fh.sessions[role] = c.Value
			fh.remember(c.Value)
			return
		}
	}
	tb.Fatalf("sign-in for %s: no session cookie (%d)", user, rec.Code)
}

// ---- state: the fixtures' shape and the per-request snapshot ----

func (fh *fzHub) settingsShape(ctx context.Context, member string) string {
	ss, _ := fh.reg.SettingsList(ctx)
	for _, s := range ss {
		if s.Member == member {
			return fzSettingsLine(s)
		}
	}
	return ""
}

func fzSettingsLine(s fleet.Settings) string {
	var kv []string
	for k, v := range s.Values {
		kv = append(kv, k+"="+v)
	}
	for _, k := range s.Secrets {
		kv = append(kv, "secret:"+k.Param)
	}
	sort.Strings(kv)
	return s.Member + "{" + strings.Join(kv, ",") + "}"
}

// shape is what the fixtures are: live members' names, settings, role
// bindings (the victim's label), active tokens, active join tokens.
func (fh *fzHub) shape(ctx context.Context) string {
	var out []string
	ms, err := fh.reg.List(ctx)
	if err != nil {
		return "error: " + err.Error()
	}
	for _, m := range ms {
		if m.RemovedAt.IsZero() {
			out = append(out, "member "+m.Name)
		}
	}
	ss, _ := fh.reg.SettingsList(ctx)
	for _, s := range ss {
		out = append(out, "settings "+fzSettingsLine(s))
	}
	bs, _ := fh.reg.Bindings(ctx)
	for _, b := range bs {
		l := "binding " + b.Subject + " " + string(b.Role) + " " + b.Origin
		if b.Subject == fh.victim {
			l += " " + b.Label
		}
		out = append(out, l)
	}
	ts, _ := fh.reg.Tokens(ctx)
	now := time.Now()
	for _, t := range ts {
		if t.Active(now) {
			out = append(out, "token "+t.Name+" "+string(t.Role))
		}
	}
	js, _ := fh.reg.ActiveJoins(ctx)
	out = append(out, "joins "+strconv.Itoa(len(js)))
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// snapshot is every piece of registry state a request might change, but
// what changes by itself (a member's last visit, a token's last use).
// rolesOnly: the role bindings and the API tokens only.
func (fh *fzHub) snapshot(ctx context.Context, rolesOnly bool) string {
	type tok struct {
		Name, Role, Hint string
		Created, Exp     time.Time
		Revoked          time.Time
	}
	var binds []string
	bs, _ := fh.reg.Bindings(ctx)
	for _, b := range bs {
		// Not the label: a person's name is noted at their requests.
		binds = append(binds, b.Subject+" "+string(b.Role)+" "+b.Origin+" "+b.AddedAt.String()+" "+b.AddedBy)
	}
	ts, _ := fh.reg.Tokens(ctx)
	var toks []tok
	for _, t := range ts {
		toks = append(toks, tok{t.Name, string(t.Role), t.Hint, t.CreatedAt, t.ExpiresAt, t.RevokedAt})
	}
	parts := []any{binds, toks}
	if !rolesOnly {
		type mem struct {
			Name, Fingerprint, Origin string
			Added, Removed            time.Time
		}
		ms, _ := fh.reg.List(ctx)
		var mems []mem
		for _, m := range ms {
			mems = append(mems, mem{m.Name, m.Fingerprint, m.Origin, m.AddedAt, m.RemovedAt})
		}
		js, _ := fh.reg.ActiveJoins(ctx)
		ss, _ := fh.reg.SettingsList(ctx)
		for i := range ss {
			for j := range ss[i].Secrets {
				ss[i].Secrets[j].DeliveredAt = time.Time{}
			}
		}
		var last int64
		if es, _ := fh.reg.AuditQuery(ctx, fleet.AuditQuery{Limit: 1}); len(es) > 0 {
			last = es[0].ID
		}
		parts = append(parts, mems, js, ss, last)
	}
	b, _ := json.Marshal(parts)
	return string(b)
}

// restore puts the fixtures back (I11): sessions signed out are signed in
// again, members, settings, bindings, tokens and join tokens as set up.
func (fh *fzHub) restore(t *testing.T) {
	ctx := context.Background()
	for _, role := range []fleet.Role{fleet.Owner, fleet.Operator, fleet.Viewer} {
		if v, _, err := fh.signIn.LoadCookie(ctx, fh.sessions[role]); err != nil || v == nil {
			fh.signInAs(t, role)
		}
	}
	euOK := func() bool {
		m, err := fh.reg.Get(ctx, "eu")
		return err == nil && m.RemovedAt.IsZero() && m.Fingerprint == fh.euFP && fh.hub.status("eu").Connected
	}
	if fh.shape(ctx) == fh.canon && euOK() {
		return
	}
	// Members: only eu (connected, its own key) and us.
	ms, _ := fh.reg.List(ctx)
	live := map[string]bool{}
	for _, m := range ms {
		if !m.RemovedAt.IsZero() {
			continue
		}
		live[m.Name] = true
		if m.Name != "eu" && m.Name != "us" {
			fh.reg.Remove(ctx, fzActor, m.Name)
			fh.hub.drop(m.Name)
		}
	}
	if !euOK() {
		fh.connectEU(t)
	}
	if !live["us"] {
		fh.addUS(t)
	}
	// Settings: eu's as set up, no others.
	ss, _ := fh.reg.SettingsList(ctx)
	for _, s := range ss {
		if s.Member != "eu" {
			fh.reg.DeleteSettings(ctx, fzActor, s.Member)
		}
	}
	if fh.settingsShape(ctx, "eu") != fh.canonEU {
		fh.reg.DeleteSettings(ctx, fzActor, "eu")
		if _, err := fh.reg.SaveSettings(ctx, fh.sealer, fzActor, "eu", fh.euValues); err != nil {
			t.Fatalf("I11 harness: saving eu's settings again: %v", err)
		}
	}
	js, _ := fh.reg.ActiveJoins(ctx)
	for _, j := range js {
		fh.reg.RevokeJoin(ctx, fzActor, j.Member, j.ID)
	}
	// Bindings: the values' owner, the operator, the labelled victim.
	want := map[string]fleet.Role{fh.opSub: fleet.Operator, fh.victim: fleet.Operator}
	bs, _ := fh.reg.Bindings(ctx)
	have := map[string]bool{}
	for _, b := range bs {
		if b.Origin == fleet.OriginValues {
			continue
		}
		r, ok := want[b.Subject]
		if ok && b.Role == r && b.Origin == fleet.OriginLocal {
			have[b.Subject] = true
			continue
		}
		fh.reg.Revoke(ctx, fzActor, b.Subject)
	}
	for s, r := range want {
		if !have[s] {
			if _, err := fh.reg.Grant(ctx, fzActor, fleet.OriginLocal, s, r); err != nil {
				t.Fatalf("I11 harness: granting %s again: %v", s, err)
			}
		}
	}
	fh.reg.NoteName(ctx, fh.victim, fzLabelSentinel)
	// Tokens: the fixtures with the secrets this harness holds, no others.
	ts, _ := fh.reg.Tokens(ctx)
	kept := map[string]bool{}
	now := time.Now()
	for _, tk := range ts {
		if !tk.Active(now) {
			continue
		}
		if h, ok := fh.tokHint[tk.Name]; ok && h == tk.Hint && fh.tokens[fh.tokByName[tk.Name]] == tk.Role {
			kept[tk.Name] = true
			continue
		}
		fh.reg.RevokeToken(ctx, fzActor, tk.Name)
	}
	for _, tf := range fzTokenFixtures {
		if !kept[tf.name] {
			fh.issue(t, tf.name, tf.role)
		}
	}
	if got := fh.shape(ctx); got != fh.canon || !euOK() {
		t.Fatalf("I11 harness: the fixtures could not be put back:\n got %s\nwant %s\neu ok %v", got, fh.canon, euOK())
	}
}

// ---- the byte-driven request decoder ----

type fzReader struct {
	b []byte
	i int
}

func (r *fzReader) more() bool { return r.i < len(r.b) }
func (r *fzReader) u8() byte {
	if r.i >= len(r.b) {
		return 0
	}
	v := r.b[r.i]
	r.i++
	return v
}

// The member names are a small pool, so the registry's cardinality stays
// bounded across millions of inputs; the token names likewise.
var (
	fzNames    = []string{"eu", "us", "ab", "a", "x1", "new", "edit"}
	fzTokNames = []string{"owner-tok", "op-tok", "viewer-tok", "t1", "bootstrap"}
	fzMethods  = []string{"GET", "POST", "PUT", "DELETE", "HEAD", "PATCH", "OPTIONS", "POST"}
	fzKeys     = []string{"subject", "gitlab", "user", "role", "days", "name", "gitlab_url", "storage",
		"runner_token", "public_key", "forget", "expires_in_days", "expires_in_minutes", "token", "values",
		"joins", "token_hours", "concurrent", "cache_limit", "s3_server", "gitlab_token"}
	fzInts = []int{-1, 0, 1, 2, 30, 60, 1440, 3650, 3651, 10080, 10081, 1 << 40}
)

func (fh *fzHub) pool() []string {
	return []string{"owner", "operator", "viewer", "-1", "30", "0", "4000", fh.host, "id:3", "3", "99",
		"gitlab:" + fh.host + "/3", "gitlab:" + fh.host + "/2", "gitlab:" + fh.host + "/99", "gitlab:" + fh.host + "/1",
		"eu", "us", "new", "x1", "owner-tok", "op-tok", "viewer-tok", "t1", "https://gl.example.com/", "emptyDir",
		fh.poolPEM, "", "../../etc", "<script>x</script>", "cia_join_0000", "glrt-fuzz-posted-runner-token", "token", "key",
		"1", "24", "10", "75%", "https://s3.example.com", "hostPath", "ab"}
}

// Path kinds.
const (
	pRoot = iota
	pAccess
	pAccessTokens
	pAudit
	pCluster
	pClusterNew
	pClusterEdit
	pAccessPeople
	pAccessPeopleRemove
	pAccessTokensRevoke
	pClusters
	pClusterJoin
	pClusterRemove
	pMember
	pMemberEsc
	pMemberBare
	pAPIMembers
	pAPIMember
	pAPISettings
	pAPIValues
	pAPIJoins
	pAPIJoin
	pAPIBindings
	pAPIBinding
	pAPITokens
	pAPIToken
	pAPIAudit
	pAPIVersion
	pAPIUnknown
	pPipeline
	pJob
	pConnect
	pJoin
	pSignIn
	pCallback
	pSignOut
	pHealth
	pLive
	pStatic
	pRaw
	pCount
)

// Authorization and cookie variants.
const (
	aNone = iota
	aOwnerTok
	aViewerTok
	aBadTok
	aLowerOwner
	aEmpty
	aBasic
	aSpaceOwner
	aOpTok
	aBootstrap
	aUpperViewer
	aCount
)

const (
	cNone = iota
	cOwner
	cOp
	cViewer
	cUnknown
	cGarbage
	cOwnerExtra
	cGarbageThenOwner
	cOwnerOtherName
	cCount
)

var fzMemberSuffix = []string{"builds", "", "pipelines/%s/%s", "static/console.css", "jobs/%s/%s", "events", "insights", "images"}

// fzReq is one decoded request.
type fzReq struct {
	kind, authVar, cookieVar int
	method, path, name       string
	authz                    string
	cookies                  [][2]string // name, value
	secFetchSite, origin     string
	secFetchMode             string
	contentType              string
	body                     []byte
}

func (fh *fzHub) decode(r *fzReader) fzReq {
	methodSel, pathSel, nameSel := r.u8(), r.u8(), r.u8()
	authSel, cookieSel, originSel, modeSel, bodySel := r.u8(), r.u8(), r.u8(), r.u8(), r.u8()
	q := fzReq{kind: int(pathSel) % pCount, authVar: int(authSel) % aCount, cookieVar: int(cookieSel) % cCount}
	q.method = fzMethods[int(methodSel)%len(fzMethods)]
	name := fzNames[int(nameSel)%len(fzNames)]
	q.name = name
	id := strconv.Itoa(int(nameSel))
	tok := fzTokNames[int(nameSel)%len(fzTokNames)]
	bodyKind, bodyN := int(bodySel)%4, int(bodySel)/4
	switch q.kind {
	case pRoot:
		q.path = "/"
	case pAccess:
		q.path = "/access"
	case pAccessTokens:
		q.path = "/access/tokens"
	case pAudit:
		q.path = "/audit?who=" + url.QueryEscape(fh.ownerSub)
	case pCluster:
		q.path = "/clusters/" + name
	case pClusterNew:
		q.path = "/clusters/new"
	case pClusterEdit:
		q.path = "/clusters/" + name + "/edit"
	case pAccessPeople:
		q.path = "/access/people"
	case pAccessPeopleRemove:
		q.path = "/access/people/remove"
	case pAccessTokensRevoke:
		q.path = "/access/tokens/revoke"
	case pClusters:
		q.path = "/clusters"
	case pClusterJoin:
		q.path = "/clusters/" + name + "/join"
	case pClusterRemove:
		q.path = "/clusters/" + name + "/remove"
	case pMember:
		suf := fzMemberSuffix[int(modeSel)%len(fzMemberSuffix)]
		if strings.Contains(suf, "%s") {
			suf = fmt.Sprintf(suf, fh.inst, id)
		}
		q.path = "/m/" + name + "/" + suf
	case pMemberEsc:
		q.path = "/m/%65u/builds?x=1"
	case pMemberBare:
		q.path = "/m/" + name
	case pAPIMembers:
		q.path = "/api/v1/fleet/members"
	case pAPIMember:
		q.path = "/api/v1/fleet/members/" + name
	case pAPISettings:
		q.path = "/api/v1/fleet/members/" + name + "/settings"
	case pAPIValues:
		q.path = "/api/v1/fleet/members/" + name + "/values"
	case pAPIJoins:
		q.path = "/api/v1/fleet/members/" + name + "/join-tokens"
	case pAPIJoin:
		q.path = "/api/v1/fleet/members/" + name + "/join-tokens/" + id
	case pAPIBindings:
		q.path = "/api/v1/fleet/role-bindings"
	case pAPIBinding:
		q.path = "/api/v1/fleet/role-bindings/gitlab/" + fh.host + "/" + id
	case pAPITokens:
		q.path = "/api/v1/fleet/tokens"
	case pAPIToken:
		q.path = "/api/v1/fleet/tokens/" + tok
	case pAPIAudit:
		q.path = "/api/v1/fleet/audit"
	case pAPIVersion:
		q.path = "/api/v1/fleet/version"
	case pAPIUnknown:
		q.path = "/api/v1/fleet/../fleet/members/%2e%2e"
	case pPipeline:
		q.path = "/pipelines/" + fh.inst + "/" + id
	case pJob:
		q.path = "/jobs/" + fh.inst + "/" + id
	case pConnect:
		q.path = fleet.ConnectPath + "?name=" + name + "&store=0123456789ab&version=fuzz&protocol=" + strconv.Itoa(int(modeSel)%3)
	case pJoin:
		q.path = fleet.JoinPath
	case pSignIn:
		q.path = "/auth/sign-in/" + fh.inst + "?to=/m/" + name + "/"
	case pCallback:
		q.path = "/auth/callback/" + fh.inst + "?code=owner&state=" + id
	case pSignOut:
		q.path = "/auth/sign-out"
	case pHealth:
		q.path = "/healthz"
	case pLive:
		q.path = "/livez"
	case pStatic:
		q.path = "/static/console.css"
	case pRaw:
		// A raw, possibly-hostile path: dot segments, double slashes,
		// encoded characters, drawn from the fuzzer's own bytes.
		raw := "/"
		for j := 0; j < bodyN && r.more(); j++ {
			raw += string(rune('!' + int(r.u8())%90))
		}
		if raw == "/" {
			raw = "/..//%2e%2e/m//"
		}
		q.path = raw
		bodyKind = 0
	}

	switch q.authVar {
	case aOwnerTok:
		q.authz = "Bearer " + fh.tokByName["owner-tok"]
	case aViewerTok:
		q.authz = "Bearer " + fh.tokByName["viewer-tok"]
	case aBadTok:
		q.authz = "Bearer cia_pat_not-a-real-token-value-xyz"
	case aLowerOwner:
		q.authz = "bearer " + fh.tokByName["owner-tok"]
	case aEmpty:
		q.authz = "Bearer "
	case aBasic:
		q.authz = "Basic " + base64.StdEncoding.EncodeToString([]byte("x:"+fh.tokByName["owner-tok"]))
	case aSpaceOwner:
		q.authz = "Bearer  " + fh.tokByName["owner-tok"]
	case aOpTok:
		q.authz = "Bearer " + fh.tokByName["op-tok"]
	case aBootstrap:
		q.authz = "Bearer " + fzBootstrap
	case aUpperViewer:
		q.authz = "BEARER " + fh.tokByName["viewer-tok"]
	}

	sc := auth.SessionCookie
	switch q.cookieVar {
	case cOwner:
		q.cookies = [][2]string{{sc, fh.sessions[fleet.Owner]}}
	case cOp:
		q.cookies = [][2]string{{sc, fh.sessions[fleet.Operator]}}
	case cViewer:
		q.cookies = [][2]string{{sc, fh.sessions[fleet.Viewer]}}
	case cUnknown:
		q.cookies = [][2]string{{sc, base64.RawURLEncoding.EncodeToString(make([]byte, 32))}}
	case cGarbage:
		q.cookies = [][2]string{{sc, "not-base64-$$$"}}
	case cOwnerExtra:
		q.cookies = [][2]string{{sc, fh.sessions[fleet.Owner]}, {"extra", "second-garbage"}}
	case cGarbageThenOwner:
		q.cookies = [][2]string{{sc, "garbage"}, {sc, fh.sessions[fleet.Owner]}}
	case cOwnerOtherName:
		q.cookies = [][2]string{{"ci_other", fh.sessions[fleet.Owner]}}
	}

	switch int(originSel) % 8 {
	case 1:
		q.secFetchSite = "same-origin"
	case 2:
		q.secFetchSite = "cross-site"
	case 3:
		q.secFetchSite = "none"
	case 4:
		q.origin = "https://evil.example"
	case 5:
		q.origin = "http://localhost"
	case 6:
		q.secFetchSite, q.origin = "same-site", "https://evil.example"
	case 7:
		q.secFetchSite, q.origin = "cross-site", "http://localhost"
	}
	switch int(modeSel) % 3 {
	case 1:
		q.secFetchMode = "navigate"
	case 2:
		q.secFetchMode = "cors"
	}

	pool := fh.pool()
	switch bodyKind {
	case 1: // raw bytes, with one of a few content types
		q.contentType = []string{"", "application/json", "application/x-www-form-urlencoded", "text/plain"}[bodyN%4]
		for j := 0; j < bodyN && r.more(); j++ {
			q.body = append(q.body, r.u8())
		}
	case 2: // a form of the fields the hub's pages read
		q.contentType = "application/x-www-form-urlencoded"
		vals := url.Values{}
		for j := 0; j < bodyN%8 && r.more(); j++ {
			k, v := fzKeys[int(r.u8())%len(fzKeys)], pool[int(r.u8())%len(pool)]
			vals.Add(k, v)
		}
		q.body = []byte(vals.Encode())
	case 3: // a JSON object of the fields the API reads
		q.contentType = "application/json"
		obj := map[string]any{}
		for j := 0; j < bodyN%8 && r.more(); j++ {
			k, vb := fzKeys[int(r.u8())%len(fzKeys)], r.u8()
			v := pool[int(vb)%len(pool)]
			switch k {
			case "expires_in_days", "expires_in_minutes", "days":
				obj[k] = fzInts[int(vb)%len(fzInts)]
			case "values":
				obj[k] = map[string]string{"gitlab_url": v, "storage": "emptyDir"}
			case "forget":
				obj[k] = []string{v}
			default:
				obj[k] = v
			}
		}
		q.body, _ = json.Marshal(obj)
	}
	return q
}

// ---- the judge ----

var fzRank = map[fleet.Role]int{fleet.Viewer: 1, fleet.Operator: 2, fleet.Owner: 3}

func fzRoleName(rank int) string {
	return []string{"nobody", "viewer", "operator", "owner"}[rank]
}

// bearerToken is the token after a Bearer scheme (any case), as the
// management API's contract has it; ok false when there is none.
func bearerToken(h string) (string, bool) {
	if len(h) > len("Bearer ") && strings.EqualFold(h[:len("Bearer ")], "Bearer ") {
		return h[len("Bearer "):], true
	}
	return "", false
}

// ceiling is the strongest role any credential of q holds (0 nobody .. 3
// owner): a known token after a Bearer scheme, a known session in a
// cookie named as the session cookie, wherever it is in the header.
func (fh *fzHub) ceiling(q fzReq) int {
	best := 0
	if tok, ok := bearerToken(q.authz); ok {
		if r, ok := fh.tokens[tok]; ok {
			best = max(best, fzRank[r])
		}
	}
	for _, c := range q.cookies {
		if c[0] != auth.SessionCookie {
			continue
		}
		for r, v := range fh.sessions {
			if v == c[1] {
				// The person's role now: an owner may have changed it
				// earlier in this input.
				best = max(best, fzRank[fh.reg.RoleOf(fh.subjects[r])])
			}
		}
	}
	return best
}

// firstSessionValid: the request's first session cookie is one of the
// harness's sessions and still signed in (a sign-out earlier in the input
// ends it): what a sign-in gate reading the cookie sees.
func (fh *fzHub) firstSessionValid(q fzReq) bool {
	for _, c := range q.cookies {
		if c[0] == auth.SessionCookie {
			for _, v := range fh.sessions {
				if v == c[1] {
					sv, _, err := fh.signIn.LoadCookie(context.Background(), v)
					return err == nil && sv != nil
				}
			}
			return false
		}
	}
	return false
}

func fzAPIKind(k int) bool { return k >= pAPIMembers && k <= pAPIUnknown }

func fzUnsafe(m string) bool { return m != "GET" && m != "HEAD" && m != "OPTIONS" }

// crossOrigin: the request says it comes from another site, and not from
// the hub's own (trusted) address.
func fzCrossOrigin(q fzReq) bool {
	switch q.secFetchSite {
	case "same-origin", "none":
		return false
	case "":
		if q.origin == "" {
			return false
		}
		o, err := url.Parse(q.origin)
		return (err != nil || o.Host != "localhost") && q.origin != "http://localhost"
	}
	return q.origin != "http://localhost"
}

// csrfGuarded: the request reaches the console's sign-in gate and its
// cross-origin protection: not the members' connect (GET) and join (POST)
// endpoints, not the health checks or the static files, not a token's
// API request, and either under /auth/ or with a valid session.
func (fh *fzHub) csrfGuarded(q fzReq) bool {
	switch q.kind {
	case pRaw, pHealth, pLive, pStatic, pAPIUnknown:
		return false
	case pConnect:
		if q.method == "GET" || q.method == "HEAD" {
			return false
		}
	case pJoin:
		if q.method == "POST" {
			return false
		}
	case pSignIn, pCallback, pSignOut:
		return true
	}
	if _, ok := bearerToken(q.authz); ok && fzAPIKind(q.kind) {
		return false
	}
	return fh.firstSessionValid(q)
}

var fzMu sync.Mutex

func FuzzHubRoutes(f *testing.F) {
	fh := buildFzHub(f)

	// Seeds: realistic and known-dangerous shapes, each record built from
	// named kinds (the decoder's header: method, path, name, auth, cookie,
	// origin, mode, body; then the body's fields).
	type rec struct {
		m, p, n, a, c, o, mo int
		form, json           [][2]string
		raw                  []byte
	}
	pool := fh.pool()
	idx := func(list []string, v string) byte {
		for i, s := range list {
			if s == v {
				return byte(i)
			}
		}
		panic("seed value not in its pool: " + v)
	}
	nameIdx := func(n string) int { return int(idx(fzNames, n)) }
	methodIdx := func(m string) int { return int(idx(fzMethods, m)) }
	enc := func(rs ...rec) []byte {
		var b []byte
		for _, r := range rs {
			body := byte(0)
			var tail []byte
			switch {
			case r.form != nil:
				body = byte(len(r.form)*4 + 2)
				for _, kv := range r.form {
					tail = append(tail, idx(fzKeys, kv[0]), idx(pool, kv[1]))
				}
			case r.json != nil:
				body = byte(len(r.json)*4 + 3)
				for _, kv := range r.json {
					var v byte
					switch kv[0] {
					default:
						v = idx(pool, kv[1])
					case "expires_in_days", "expires_in_minutes", "days":
						n, _ := strconv.Atoi(kv[1])
						v = 255
						for i, x := range fzInts {
							if x == n {
								v = byte(i)
							}
						}
						if v == 255 {
							panic("seed number not in fzInts: " + kv[1])
						}
					}
					tail = append(tail, idx(fzKeys, kv[0]), v)
				}
			case r.raw != nil:
				body = byte(len(r.raw)*4 + 1)
				tail = r.raw
			}
			b = append(b, byte(r.m), byte(r.p), byte(r.n), byte(r.a), byte(r.c), byte(r.o), byte(r.mo), body)
			b = append(b, tail...)
		}
		return b
	}
	get, post, put, del := methodIdx("GET"), methodIdx("POST"), methodIdx("PUT"), methodIdx("DELETE")
	eu, us, nw := nameIdx("eu"), nameIdx("us"), nameIdx("new")
	const sameOrigin, crossSite = 1, 2
	seeds := [][]byte{
		// The audit's cookie-forwarding trigger: an owner's job and
		// pipeline links fan out to every member.
		enc(rec{m: get, p: pJob, n: 1, c: cOwner, mo: 1}),
		enc(rec{m: get, p: pPipeline, n: 2, c: cOwner, mo: 1}),
		// The owner opens the connected member's pages; nobody the away one.
		enc(rec{m: get, p: pMember, n: eu, c: cOwner}),
		enc(rec{m: get, p: pMember, n: us, mo: 1}),
		// Liveness of the sentinels for the owner and the operator.
		enc(rec{m: get, p: pCluster, n: eu, c: cOwner}),
		enc(rec{m: get, p: pAccess, c: cOp}),
		// A viewer asks for a cluster's settings, Access, Audit, Members.
		enc(rec{m: get, p: pCluster, n: eu, c: cViewer, mo: 1}),
		enc(rec{m: get, p: pAccess, c: cViewer}, rec{m: get, p: pAudit, c: cViewer}, rec{m: get, p: pRoot, c: cViewer}),
		// A viewer's token tries every write of the API (a missing
		// permission check shows here).
		enc(rec{m: del, p: pAPIMember, n: us, a: aViewerTok}),
		enc(rec{m: post, p: pAPITokens, a: aViewerTok, json: [][2]string{{"name", "t1"}, {"role", "owner"}}}),
		enc(rec{m: put, p: pAPISettings, n: eu, a: aUpperViewer, json: [][2]string{{"values", "https://gl.example.com/"}}}),
		enc(rec{m: post, p: pAPIJoins, n: eu, a: aViewerTok, json: [][2]string{{"expires_in_minutes", "30"}}}),
		// An operator tries the owner's writes: roles and tokens.
		enc(rec{m: put, p: pAPIBinding, n: 3, a: aOpTok, json: [][2]string{{"role", "owner"}}}),
		enc(rec{m: post, p: pAPITokens, a: aOpTok, json: [][2]string{{"name", "t1"}, {"role", "owner"}}}),
		enc(rec{m: post, p: pAccessPeople, c: cOp, o: sameOrigin, form: [][2]string{{"subject", "gitlab:" + fh.host + "/3"}, {"role", "owner"}}}),
		enc(rec{m: post, p: pAccessTokens, c: cOp, o: sameOrigin, form: [][2]string{{"name", "t1"}, {"role", "owner"}, {"days", "30"}}}),
		enc(rec{m: post, p: pAccessPeopleRemove, c: cOp, o: sameOrigin, form: [][2]string{{"subject", "gitlab:" + fh.host + "/99"}}}),
		// A viewer's session on the page forms.
		enc(rec{m: post, p: pClusters, c: cViewer, o: sameOrigin, form: [][2]string{{"name", "new"}, {"gitlab_url", "https://gl.example.com/"}, {"storage", "emptyDir"}}}),
		// Nobody writes: the API, a join with an unknown token.
		enc(rec{m: post, p: pAPIMembers, json: [][2]string{{"name", "new"}, {"public_key", fh.poolPEM}}}),
		enc(rec{m: post, p: pJoin, json: [][2]string{{"name", "new"}, {"token", "cia_join_0000"}, {"public_key", fh.poolPEM}}}),
		enc(rec{m: get, p: pConnect, n: nw, mo: 1}),
		// Cross-site writes with a session: a page, the API, sign-out.
		enc(rec{m: post, p: pClusters, c: cOwner, o: crossSite, form: [][2]string{{"name", "new"}}}),
		enc(rec{m: del, p: pAPIMember, n: us, c: cOwner, o: 4}),
		enc(rec{m: post, p: pSignOut, c: cOwner, o: 6}),
		// A bad bearer token with an owner session: the token decides.
		enc(rec{m: get, p: pAPIMembers, a: aBadTok, c: cOwner}),
		enc(rec{m: del, p: pAPIMember, n: us, a: aSpaceOwner, c: cOwner}),
		// Credentials in odd places: two session cookies, the owner's
		// session under another name, an empty bearer.
		enc(rec{m: get, p: pCluster, n: eu, c: cGarbageThenOwner}),
		enc(rec{m: get, p: pAccess, c: cOwnerOtherName}),
		enc(rec{m: get, p: pAPIMembers, a: aEmpty, c: cViewer}),
		// The owner's successful writes, then the state is put back.
		enc(rec{m: post, p: pClusters, c: cOwner, o: sameOrigin, form: [][2]string{{"name", "x1"}, {"gitlab_url", "https://gl.example.com/"},
			{"runner_token", "glrt-fuzz-posted-runner-token"}, {"storage", "emptyDir"}}},
			rec{m: post, p: pClusterJoin, n: nameIdx("x1"), c: cOp, o: sameOrigin},
			rec{m: post, p: pAPIJoins, n: nameIdx("x1"), a: aOpTok, json: [][2]string{{"expires_in_minutes", "60"}}},
			rec{m: get, p: pAPIValues, n: nameIdx("x1"), a: aViewerTok}),
		enc(rec{m: put, p: pAPISettings, n: eu, a: aOwnerTok, json: [][2]string{{"values", "https://gl.example.com/"}}},
			rec{m: get, p: pClusterEdit, n: eu, c: cOp}),
		enc(rec{m: post, p: pClusterEdit, n: eu, c: cOp, o: sameOrigin, form: [][2]string{{"gitlab_url", "https://gl.example.com/"}}},
			rec{m: post, p: pCluster, n: eu, c: cOp, o: sameOrigin, form: [][2]string{{"gitlab_url", "https://gl.example.com/"}, {"storage", "hostPath"}}}),
		enc(rec{m: put, p: pAPIBinding, n: 3, a: aBootstrap, json: [][2]string{{"role", "operator"}}},
			rec{m: post, p: pClusters, c: cViewer, o: sameOrigin, form: [][2]string{{"name", "ab"}, {"gitlab_url", "https://gl.example.com/"},
				{"runner_token", "glrt-fuzz-posted-runner-token"}}}),
		enc(rec{m: post, p: pAPIMembers, a: aOpTok, json: [][2]string{{"name", "x1"}, {"public_key", fh.poolPEM}}},
			rec{m: post, p: pAPITokens, a: aOwnerTok, json: [][2]string{{"name", "t1"}, {"role", "viewer"}, {"expires_in_days", "30"}}}),
		enc(rec{m: post, p: pClusterRemove, n: eu, c: cOwner, o: sameOrigin}, rec{m: get, p: pMember, n: eu, c: cOwner}),
		enc(rec{m: del, p: pAPIMember, n: eu, a: aOwnerTok}, rec{m: get, p: pCluster, n: eu, c: cViewer}),
		enc(rec{m: post, p: pAPIJoins, n: eu, a: aBootstrap, json: [][2]string{{"expires_in_minutes", "30"}}}),
		enc(rec{m: del, p: pAPIToken, n: 0, a: aOwnerTok}, rec{m: get, p: pAPIMembers, a: aOwnerTok}),
		enc(rec{m: post, p: pSignOut, c: cOwner, o: sameOrigin}, rec{m: get, p: pAccess, c: cOwner}),
		enc(rec{m: post, p: pAccessTokens, c: cOwner, o: sameOrigin, form: [][2]string{{"name", "t1"}, {"role", "viewer"}, {"days", "-1"}}}),
		enc(rec{m: get, p: pAPITokens, a: aLowerOwner}, rec{m: get, p: pAPIValues, n: eu, a: aBootstrap}),
		// Escaped and hostile paths; sign-in with a forged callback.
		enc(rec{m: get, p: pMemberEsc, c: cOwner, mo: 1}, rec{m: get, p: pRaw, c: cOwner, raw: []byte("..%2f..%2fm%2feu%2f")}),
		enc(rec{m: get, p: pMemberBare, n: eu, c: cOwner}, rec{m: get, p: pAPIUnknown, a: aOwnerTok}),
		enc(rec{m: get, p: pSignIn, mo: 1}, rec{m: get, p: pCallback, n: 5}),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		fzMu.Lock()
		defer fzMu.Unlock()
		fh.restore(t)
		fh.member.reset()
		r := &fzReader{b: data}
		for n := 0; n < 8 && r.more(); n++ {
			fh.run(t, fh.decode(r), n == 0)
		}
	})
}

// run applies one request and checks every invariant.
func (fh *fzHub) run(t *testing.T, q fzReq, first bool) {
	ctx := context.Background()
	ceil := fh.ceiling(q)
	csrfExpected := fzUnsafe(q.method) && fzCrossOrigin(q) && fh.csrfGuarded(q)
	var before, beforeRoles string
	switch {
	case ceil <= 1 || csrfExpected:
		before = fh.snapshot(ctx, false)
	case ceil == 2:
		beforeRoles = fh.snapshot(ctx, true)
	}
	hitsBefore := fh.member.count()

	req := httptest.NewRequest(q.method, "http://localhost"+fzTarget(q.path), strings.NewReader(string(q.body)))
	if q.authz != "" {
		req.Header.Set("Authorization", q.authz)
	}
	if len(q.cookies) > 0 {
		var parts []string
		for _, c := range q.cookies {
			parts = append(parts, c[0]+"="+c[1])
		}
		req.Header.Set("Cookie", strings.Join(parts, "; "))
	}
	if q.secFetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", q.secFetchSite)
	}
	if q.origin != "" {
		req.Header.Set("Origin", q.origin)
	}
	if q.secFetchMode != "" {
		req.Header.Set("Sec-Fetch-Mode", q.secFetchMode)
	}
	if q.contentType != "" {
		req.Header.Set("Content-Type", q.contentType)
	}

	rec := httptest.NewRecorder()
	cctx, cancel := context.WithTimeout(ctx, fzReqWait)
	defer cancel()
	type outcome struct {
		panicked any
		stack    []byte
	}
	done := make(chan outcome, 1)
	go func() {
		var o outcome
		defer func() {
			if p := recover(); p != nil && p != http.ErrAbortHandler {
				o.panicked, o.stack = p, debug.Stack()
			}
			done <- o
		}()
		fh.top.ServeHTTP(rec, req.WithContext(cctx))
	}()
	var o outcome
	select {
	case o = <-done:
	case <-time.After(fzReqWait + fzHang):
		t.Fatalf("I2 hang: %s %s did not return %v after its context ended", q.method, q.path, fzHang)
	}
	what := fmt.Sprintf("%s %s (auth %d, cookie %d, ceiling %s, site %q, origin %q, %s %q)", q.method, q.path, q.authVar,
		q.cookieVar, fzRoleName(ceil), q.secFetchSite, q.origin, q.contentType, trimFz(string(q.body)))
	// I1: no panic, no 500.
	if o.panicked != nil {
		t.Fatalf("I1 panic serving %s: %v\n%s", what, o.panicked, o.stack)
	}
	res := rec.Result()
	body := rec.Body.String()
	if fzTrace {
		t.Logf("%d <- %s", res.StatusCode, what)
	}
	if res.StatusCode == http.StatusInternalServerError {
		t.Fatalf("I1 500 from %s: %s", what, trimFz(body))
	}

	// I3: nobody and viewers change nothing; I8 (below) too.
	if before != "" {
		if after := fh.snapshot(ctx, false); after != before {
			t.Fatalf("I3/I8 the registry changed by %s -> %d\nbefore %s\nafter  %s", what, res.StatusCode, before, after)
		}
	}
	// I4: an operator changes no role and no token.
	if beforeRoles != "" {
		if after := fh.snapshot(ctx, true); after != beforeRoles {
			t.Fatalf("I4 an operator changed roles or tokens: %s -> %d\nbefore %s\nafter  %s", what, res.StatusCode, beforeRoles, after)
		}
	}
	// I5: below operator, no cluster settings and no binding labels.
	if ceil < 2 {
		if strings.Contains(body, fzClusterSentinel) {
			t.Fatalf("I5 cluster settings leaked to %s: %s", fzRoleName(ceil), what)
		}
		if strings.Contains(body, fzLabelSentinel) {
			t.Fatalf("I5 a role binding's label leaked to %s: %s", fzRoleName(ceil), what)
		}
	}
	// I6: no secret in any response.
	for _, s := range append([]string{fzRunnerSecret, fzSecretsKey}, fh.ever...) {
		if s != "" && strings.Contains(body, s) {
			t.Fatalf("I6 a secret, token or session value appeared in the answer to %s", what)
		}
	}
	// I7: the member never received the hub's session or a hub credential.
	for _, got := range fh.member.all() {
		if strings.Contains(strings.ToLower(got), strings.ToLower(control.SessionHeader)+":") {
			t.Fatalf("I7 the member received %s (from %s):\n%s", control.SessionHeader, what, got)
		}
		for _, s := range fh.ever {
			if s != "" && strings.Contains(got, s) {
				t.Fatalf("I7 the member received a session value or a hub token (from %s):\n%s", what, got)
			}
		}
	}
	// I8: cross-origin writes with a session are refused.
	if csrfExpected && res.StatusCode != http.StatusForbidden {
		t.Fatalf("I8 a cross-origin %s with a session was not refused: %d", what, res.StatusCode)
	}
	// I9: an unknown bearer token on the API is 401, a session or not.
	if tok, ok := bearerToken(q.authz); ok && fzAPIKind(q.kind) && q.kind != pAPIUnknown {
		if _, known := fh.tokens[tok]; !known && res.StatusCode != http.StatusUnauthorized && res.StatusCode != http.StatusMethodNotAllowed &&
			res.StatusCode != http.StatusNotFound {
			t.Fatalf("I9 an unknown bearer token was answered %d, not 401: %s", res.StatusCode, what)
		}
	}
	// I10: non-vacuity on the first request, the fixtures in place.
	if first && q.authVar == aNone && q.method == "GET" && (q.cookieVar == cOwner || q.cookieVar == cOp) {
		switch {
		case q.kind == pCluster && q.name == "eu":
			if res.StatusCode != http.StatusOK || !strings.Contains(body, fzClusterSentinel) {
				t.Fatalf("I10 the %s does not see eu's settings (%d): the leak checks are vacuous", fzRoleName(ceil), res.StatusCode)
			}
		case q.kind == pAccess:
			if res.StatusCode != http.StatusOK || !strings.Contains(body, fzLabelSentinel) {
				t.Fatalf("I10 the %s does not see the labelled binding on Access (%d)", fzRoleName(ceil), res.StatusCode)
			}
		case q.kind == pMember && q.name == "eu" && q.cookieVar == cOwner && strings.HasSuffix(q.path, "/builds"):
			hits := fh.member.count()
			if res.StatusCode != http.StatusOK || hits == hitsBefore {
				t.Fatalf("I10 the owner's %s answered %d and reached the member %d times: I7 is vacuous", q.path, res.StatusCode, hits-hitsBefore)
			}
		}
	}
}

// fzTarget keeps httptest.NewRequest from panicking on a target it
// cannot parse: an unparseable one falls back to a fixed hostile one.
func fzTarget(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if _, err := url.ParseRequestURI(p); err != nil {
		return "/..//%2e%2e/m//"
	}
	return p
}

func trimFz(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
