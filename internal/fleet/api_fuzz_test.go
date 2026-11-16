package fleet

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// Fuzz targets of the hub's management API, its join endpoint and the
// values it generates from a cluster's settings, as an attacker sees
// them. Each target turns the fuzzer's bytes into a structure (a sequence
// of requests, a settings map) with a small byte-driven decoder, so that
// most inputs reach the handlers rather than the JSON parser, and checks
// the invariants written above it with t.Fatalf.
//
// One registry (fleet.db in the target's temporary directory) is made per
// target and process, seeded once, and put back to its seeded rows before
// every input: opening and seeding SQLite costs far more than an input.

// fzMarker starts every secret value the targets give the hub (kept
// secrets of the seed, secrets a fuzzed request sets). Upper case: no
// name, token name or subject can hold it.
const fzMarker = "ZQXJ"

// fzMaxBody is the documented body limit of the API (64 KiB), written out
// here so that a change of the product's constant fails the target.
const fzMaxBody = 64 << 10

// fzForms are the texts a value may take in an answer: as it is and as a
// JSON string escapes it (quotes, backslashes, control bytes, invalid
// UTF-8, HTML characters), so that a leak check does not miss an escaped
// copy.
func fzForms(s string) []string {
	b, _ := json.Marshal(s)
	e := string(b[1 : len(b)-1])
	if e == s {
		return []string{s}
	}
	return []string{s, e}
}

// fzCarries says whether text holds s in any of its forms.
func fzCarries(text, s string) bool {
	for _, f := range fzForms(s) {
		if strings.Contains(text, f) {
			return true
		}
	}
	return false
}

// fzTables are the registry's tables, all of them (a snapshot compares
// every row).
var fzTables = []string{"members", "audit", "role_bindings", "api_tokens", "join_tokens", "cluster_settings", "cluster_secrets"}

type fzToken struct {
	name, secret string
	role         Role
}

type fzJoin struct{ name, secret string }

// fzFixture is the seeded registry of a target and what the seed holds.
type fzFixture struct {
	mu     sync.Mutex
	r      *Registry
	sealer *Sealer
	clock  time.Time
	cols   map[string][]string
	rows   map[string][][]any
	// tokens: own (owner), op (operator), view (viewer).
	tokens []fzToken
	// joins issued by the seed: us (used by the member us), ap (pending),
	// old (expired), rv (revoked by the next one), rv (pending).
	joins []fzJoin
	pubs  map[string]string
	// secrets: what no answer may carry (token and join secrets and their
	// hashes, kept secret values, private keys, the secrets key).
	secrets []string
}

// fzKept are the secrets the seed keeps for a cluster.
func fzKept(member string) map[string]string {
	return map[string]string{
		"runner_token":  "glrt-" + fzMarker + "-runner-" + member + "-0123456789",
		"gitlab_token":  "glpat-" + fzMarker + "-gitlab-" + member,
		"s3_access_key": "AKIA" + fzMarker + strings.ToUpper(member),
		"s3_secret_key": fzMarker + "/s3+secret/" + member,
	}
}

func newFzFixture(f *testing.F) *fzFixture {
	ctx := context.Background()
	r, err := OpenRegistry(ctx, f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { r.Close() })
	r.Log = quiet
	fx := &fzFixture{r: r, clock: time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC), pubs: map[string]string{}}
	r.now = func() time.Time { return fx.clock }
	must := func(err error) {
		f.Helper()
		if err != nil {
			f.Fatal(err)
		}
	}
	fx.sealer, err = NewSealer([]byte(testSecretsKey))
	must(err)
	fx.secrets = append(fx.secrets, testSecretsKey)
	for _, n := range []string{"eu", "us", "spare0", "spare1", "spare2", "spare3"} {
		for {
			priv, pub, err := GenerateKey()
			must(err)
			// A key whose text holds the marker would look like a leak.
			if strings.Contains(string(pub), fzMarker) {
				continue
			}
			fx.pubs[n] = string(pub)
			for _, line := range strings.Split(string(priv), "\n") {
				if line != "" && !strings.HasPrefix(line, "-----") {
					fx.secrets = append(fx.secrets, line)
				}
			}
			break
		}
	}
	must(r.SyncOwners(ctx, []string{"gitlab:gitlab.example.com/7"}))
	_, err = r.Grant(ctx, OriginLocal, OriginLocal, "gitlab:gitlab.example.com/8", Operator)
	must(err)
	for _, tk := range []fzToken{{name: "own", role: Owner}, {name: "op", role: Operator}, {name: "view", role: Viewer}} {
		_, s, err := r.IssueToken(ctx, OriginLocal, tk.name, tk.role, 0)
		must(err)
		tk.secret = s
		fx.tokens = append(fx.tokens, tk)
		fx.secrets = append(fx.secrets, s, tokenHash(s))
	}
	_, err = r.Add(ctx, OriginLocal, OriginAPI, "eu", fx.pubs["eu"])
	must(err)
	issue := func(name string, ttl time.Duration) string {
		_, s, err := r.IssueJoin(ctx, OriginLocal, name, ttl)
		must(err)
		fx.joins = append(fx.joins, fzJoin{name, s})
		fx.secrets = append(fx.secrets, s, joinHash(s))
		return s
	}
	us := issue("us", 0)
	_, err = r.Join(ctx, "us", us, fx.pubs["us"])
	must(err)
	issue("ap", 0)
	fx.clock = fx.clock.Add(-2 * time.Hour)
	issue("old", time.Hour)
	fx.clock = fx.clock.Add(2 * time.Hour)
	issue("rv", 0)
	issue("rv", 0)
	for _, m := range []string{"eu", "ap"} {
		vals := map[string]string{"gitlab_url": "https://gitlab.example.com/", "s3_server": "s3.eu-central-1.amazonaws.com",
			"s3_bucket": "ci-cache-" + m, "s3_region": "eu-central-1", "concurrent": "40"}
		for k, v := range fzKept(m) {
			vals[k] = v
			fx.secrets = append(fx.secrets, v)
		}
		_, err := r.SaveSettings(ctx, fx.sealer, OriginLocal, m, vals)
		must(err)
	}
	// The seed's rows, to put back before every input.
	fx.cols, fx.rows = map[string][]string{}, map[string][][]any{}
	for _, t := range fzTables {
		rows, err := r.db.QueryContext(ctx, `SELECT * FROM `+t)
		must(err)
		cols, err := rows.Columns()
		must(err)
		fx.cols[t] = cols
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			must(rows.Scan(ptrs...))
			fx.rows[t] = append(fx.rows[t], vals)
		}
		must(rows.Err())
		rows.Close()
	}
	return fx
}

// reset puts the seeded rows back and reloads the registry's maps.
func (fx *fzFixture) reset(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	fx.clock = time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)
	tx, err := fx.r.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, tb := range fzTables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+tb); err != nil {
			t.Fatal(err)
		}
		cols := fx.cols[tb]
		q := `INSERT INTO ` + tb + ` (` + strings.Join(cols, ", ") + `) VALUES (?` + strings.Repeat(", ?", len(cols)-1) + `)`
		for _, row := range fx.rows[tb] {
			if _, err := tx.ExecContext(ctx, q, row...); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := fx.r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
}

// snapshot is the registry's whole state as text: every row of every
// table (an API token's last use left out: authenticating writes it) and
// the maps in memory.
func (fx *fzFixture) snapshot(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder
	for _, tb := range fzTables {
		rows, err := fx.r.db.QueryContext(ctx, `SELECT * FROM `+tb)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		var lines []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			var l strings.Builder
			for i, v := range vals {
				if tb == "api_tokens" && cols[i] == "used_at" {
					continue
				}
				if bs, ok := v.([]byte); ok {
					fmt.Fprintf(&l, "%s=%x ", cols[i], bs)
				} else {
					fmt.Fprintf(&l, "%s=%q ", cols[i], fmt.Sprint(v))
				}
			}
			lines = append(lines, l.String())
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		sort.Strings(lines)
		b.WriteString(tb + ":\n" + strings.Join(lines, "\n") + "\n")
	}
	fx.r.mu.Lock()
	var mem []string
	for k, v := range fx.r.byKey {
		mem = append(mem, "key "+k+" "+v)
	}
	for k, v := range fx.r.roles {
		mem = append(mem, fmt.Sprintf("role %s %+v", k, v))
	}
	fx.r.mu.Unlock()
	sort.Strings(mem)
	b.WriteString(strings.Join(mem, "\n"))
	return b.String()
}

// ---- the decoder ----

// fzBytes turns the fuzzer's bytes into choices; past the end every
// choice is the first one (zero).
type fzBytes struct {
	b []byte
	i int
}

func (d *fzBytes) more() bool { return d.i < len(d.b) }

func (d *fzBytes) byte() byte {
	if d.i >= len(d.b) {
		return 0
	}
	c := d.b[d.i]
	d.i++
	return c
}

func (d *fzBytes) n(k int) int {
	if k <= 1 {
		return 0
	}
	return int(d.byte()) % k
}

// str is up to max raw bytes.
func (d *fzBytes) str(max int) string {
	l := d.n(max + 1)
	if d.i+l > len(d.b) {
		l = len(d.b) - d.i
	}
	s := string(d.b[d.i : d.i+l])
	d.i += l
	return s
}

// pick is one of xs, or (one time in len(xs)+1) raw bytes.
func (d *fzBytes) pick(xs ...string) string {
	i := d.n(len(xs) + 1)
	if i == len(xs) {
		return d.str(24)
	}
	return xs[i]
}

// ---- FuzzAPIAuthz ----

// fzLeast is the least role each route needs: written out here rather
// than read from the API's route table, so that a route whose permission
// changes, or a new route, fails the target until this table agrees.
var fzLeast = map[string]Role{
	"GET /api/v1/fleet/version":                             Viewer,
	"GET /api/v1/fleet/members":                             Operator,
	"POST /api/v1/fleet/members":                            Operator,
	"GET /api/v1/fleet/members/{name}":                      Operator,
	"DELETE /api/v1/fleet/members/{name}":                   Operator,
	"GET /api/v1/fleet/members/{name}/join-tokens":          Operator,
	"POST /api/v1/fleet/members/{name}/join-tokens":         Operator,
	"GET /api/v1/fleet/members/{name}/join-tokens/{id}":     Operator,
	"DELETE /api/v1/fleet/members/{name}/join-tokens/{id}":  Operator,
	"GET /api/v1/fleet/members/{name}/settings":             Operator,
	"PUT /api/v1/fleet/members/{name}/settings":             Operator,
	"GET /api/v1/fleet/members/{name}/values":               Operator,
	"GET /api/v1/fleet/role-bindings":                       Operator,
	"GET /api/v1/fleet/role-bindings/gitlab/{host}/{id}":    Operator,
	"PUT /api/v1/fleet/role-bindings/gitlab/{host}/{id}":    Owner,
	"DELETE /api/v1/fleet/role-bindings/gitlab/{host}/{id}": Owner,
	"GET /api/v1/fleet/tokens":                              Owner,
	"POST /api/v1/fleet/tokens":                             Owner,
	"GET /api/v1/fleet/tokens/{name}":                       Owner,
	"DELETE /api/v1/fleet/tokens/{name}":                    Owner,
	"GET /api/v1/fleet/audit":                               Operator,
}

// fzRank orders principals: nobody below every role.
func fzRank(p Principal) int {
	if p.Subject == "" {
		return -1
	}
	switch p.Role {
	case Viewer:
		return 0
	case Operator:
		return 1
	case Owner:
		return 2
	}
	return -1
}

// fzRoutes are the API's paths (and "" for any path under the prefix).
var fzRoutes = []struct{ method, path string }{
	{"GET", "/api/v1/fleet/version"},
	{"GET", "/api/v1/fleet/members"},
	{"POST", "/api/v1/fleet/members"},
	{"GET", "/api/v1/fleet/members/{name}"},
	{"DELETE", "/api/v1/fleet/members/{name}"},
	{"GET", "/api/v1/fleet/members/{name}/join-tokens"},
	{"POST", "/api/v1/fleet/members/{name}/join-tokens"},
	{"GET", "/api/v1/fleet/members/{name}/join-tokens/{id}"},
	{"DELETE", "/api/v1/fleet/members/{name}/join-tokens/{id}"},
	{"GET", "/api/v1/fleet/members/{name}/settings"},
	{"PUT", "/api/v1/fleet/members/{name}/settings"},
	{"GET", "/api/v1/fleet/members/{name}/values"},
	{"GET", "/api/v1/fleet/role-bindings"},
	{"GET", "/api/v1/fleet/role-bindings/gitlab/{host}/{id}"},
	{"PUT", "/api/v1/fleet/role-bindings/gitlab/{host}/{id}"},
	{"DELETE", "/api/v1/fleet/role-bindings/gitlab/{host}/{id}"},
	{"GET", "/api/v1/fleet/tokens"},
	{"POST", "/api/v1/fleet/tokens"},
	{"GET", "/api/v1/fleet/tokens/{name}"},
	{"DELETE", "/api/v1/fleet/tokens/{name}"},
	{"GET", "/api/v1/fleet/audit"},
	{"", ""},
}

var fzMethods = []string{"GET", "POST", "PUT", "DELETE", "HEAD", "PATCH", "OPTIONS", "BREW"}

// fzSegment encodes a path segment: as it is, escaped, every byte
// percent-encoded, or with an escaped slash or dot segment around it.
func fzSegment(d *fzBytes, s string) string {
	switch d.n(6) {
	case 1:
		return url.PathEscape(s)
	case 2:
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			fmt.Fprintf(&b, "%%%02X", s[i])
		}
		return b.String()
	case 3:
		return s + "%2F" + d.pick("x", "..", "join-tokens", "")
	case 4:
		return d.pick("..", ".", "%2e%2e", "") + "/" + s
	}
	return s
}

var fzNames = []string{"eu", "us", "ap", "old", "rv", "new", "fresh", "own", "op", "view", "bootstrap", "EU", "eu.x", "a-", "x", ""}

func (d *fzBytes) name() string { return d.pick(fzNames...) }

// settingsBody is a values map (secrets set marked fzMarker) and a
// forget list; given lists the secret values set.
func (d *fzBytes) settingsValues(fx *fzFixture, given *[]string) map[string]string {
	vals := map[string]string{}
	for _, p := range Params {
		switch d.n(4) {
		case 0:
			continue
		case 1:
			vals[p.Key] = fzValid(d, fx, p)
		case 2:
			vals[p.Key] = d.pick(fzDangerous...)
		case 3:
			vals[p.Key] = ""
		}
		if p.Kind == KindSecret && vals[p.Key] != "" {
			v := fzMarker + d.str(12)
			if p.Key == "runner_token" {
				v = "glrt-" + v + "-0123456789abc"
			}
			vals[p.Key] = v
			*given = append(*given, v)
		}
	}
	if d.n(8) == 0 {
		vals[d.pick("image", "buildkit.image", "values", "public_key ", "")] = d.pick(fzDangerous...)
	}
	return vals
}

// fzValid is a valid value for p.
func fzValid(d *fzBytes, fx *fzFixture, p Param) string {
	switch p.Key {
	case "joins":
		return d.pick("token", "key")
	case "token_hours":
		return d.pick("1", "24", "168")
	case "public_key":
		return fx.pubs[d.pick("spare0", "spare1", "eu", "us")]
	case "storage":
		return d.pick("hostPath", "emptyDir")
	case "host_path":
		return d.pick("/var/lib/buildkit", "/mnt/nvme/bk", "/a")
	case "cache_limit":
		return d.pick("75%", "400GB", "2TB", "1%")
	case "gitlab_url":
		return d.pick("https://gitlab.example.com/", "http://g.example:8443/sub/", "https://GitLab.Example.com")
	case "concurrent":
		return d.pick("10", "1", "1000", "+5", "010")
	case "s3_server":
		return d.pick("s3.eu-central-1.amazonaws.com", "minio:9000", "10.0.0.1:9000")
	case "s3_bucket":
		return d.pick("ci-cache", "a.b-c")
	case "s3_region":
		return d.pick("eu-central-1", "us-east-1")
	}
	return "x"
}

// fzDangerous are values that break YAML, a shell, a path or a URL.
var fzDangerous = []string{
	"\"; image: evil", "x\nimage: evil", "x\n---\nimage: evil", "$(id)", "`id`", "a; rm -rf /", " 75% ", "75%\n", "\u0085", " ",
	"&a x", "*a", "!!binary eA==", "{a: b}", "[1]", "#c", "'q'", "1e3", "0x10", "-5", "1001", "yes", "null", "~", "",
	"/var/lib/../etc", "/var//lib", "/var/lib/$(id)", "/a b", "https://u:p@h.example/", "https://h.example/?q=1", "https://h.example/#f",
	"https://h.example/\"x", "https://h.example/$(id)", "https://h.example/\u0085", "ftp://h.example/", "h.example:99999", "https://",
	"-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n", "glrt-short", "\x00", "\xff\xfe",
}

// fzReq is one request the sequence makes and what it carries.
type fzReq struct {
	method, target, body string
	auth                 string
	// who: the principal the request is from, as the target expects it.
	expect Principal
	// secretsSet: secret values a settings body sets (not part of seen).
	secretsSet []string
}

// buildReq makes the next request of a sequence.
func (d *fzBytes) buildReq(fx *fzFixture, issued []fzToken) fzReq {
	rt := fzRoutes[d.n(len(fzRoutes))]
	q := fzReq{method: rt.method}
	if q.method == "" || d.n(8) == 0 {
		q.method = fzMethods[d.n(len(fzMethods))]
	}
	path := rt.path
	if path == "" {
		path = APIPrefix + d.pick("members/eu/x", "members/eu/join-tokens/1/x", "role-bindings/gitlab/x", "tokens/", "Members", "%2e%2e/version", "version/")
	}
	path = strings.Replace(path, "{name}", fzSegment(d, d.name()), 1)
	path = strings.Replace(path, "{host}", fzSegment(d, d.pick("gitlab.example.com", "GitLab.Example.com", "g.example:8443", "evil.example", "gitlab.example.com:443", "a..b", "-")), 1)
	path = strings.Replace(path, "{id}", fzSegment(d, d.pick("1", "2", "3", "5", "6", "7", "8", "05", "0", "-1", "18446744073709551616", "9223372036854775807", " 1")), 1)
	if strings.HasSuffix(path, "/audit") && d.n(2) == 0 {
		v := url.Values{}
		for _, k := range []string{"limit", "before", "actor", "target"} {
			if d.n(2) == 0 {
				v.Set(k, d.pick("1", "1000", "1001", "0", "-1", "x", "eu", "token:own", "local", "9223372036854775808"))
			}
		}
		path += "?" + v.Encode()
	}
	q.target = path
	// The body: built for the route, raw, empty, or one past the limit.
	var body map[string]any
	switch {
	case strings.HasSuffix(rt.path, "/settings"):
		var forget []any
		for i := d.n(3); i > 0; i-- {
			forget = append(forget, d.pick("gitlab_token", "runner_token", "s3_access_key", "s3_secret_key", "gitlab_url", "nope"))
		}
		vals := d.settingsValues(fx, &q.secretsSet)
		body = map[string]any{"values": vals}
		if forget != nil {
			body["forget"] = forget
		}
	case rt.path == "/api/v1/fleet/members":
		body = map[string]any{"name": d.name(), "public_key": d.pick(fx.pubs["spare0"], fx.pubs["spare1"], fx.pubs["eu"], fx.pubs["us"],
			fx.pubs["spare2"]+fx.pubs["spare3"], "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n", "ssh-ed25519 AAAA")}
	case strings.HasSuffix(rt.path, "/join-tokens"):
		body = map[string]any{"expires_in_minutes": []int{0, 1, 60, 10080, 10081, -1, 1 << 40}[d.n(7)]}
	case strings.HasPrefix(rt.path, "/api/v1/fleet/role-bindings/"):
		body = map[string]any{"role": d.pick("owner", "operator", "viewer", "Owner", "admin", "")}
	case rt.path == "/api/v1/fleet/tokens":
		body = map[string]any{"name": d.pick("ci", "own", "op", "bootstrap", "t.x_y-z", "A", ""), "role": d.pick("owner", "operator", "viewer", "root"),
			"expires_in_days": []int{0, -1, 1, 3650, 3651, -2}[d.n(6)]}
	}
	switch d.n(8) {
	case 0:
		q.body = d.str(64)
		q.secretsSet = nil
	case 1:
		q.body = ""
	default:
		if body != nil {
			if d.n(10) == 0 {
				body[d.pick("extra", "Name", "role ")] = "x"
			}
			b, _ := json.Marshal(body)
			q.body = string(b)
		}
	}
	if d.n(16) == 0 {
		q.body += strings.Repeat(" ", fzMaxBody+1)
	}
	// The principal: nobody, one of the seed's tokens, one issued by the
	// sequence, or an unknown token.
	switch c := d.n(7); {
	case c == 0:
	case c <= 3:
		tk := fx.tokens[c-1]
		q.auth, q.expect = "Bearer "+tk.secret, Principal{Subject: "token:" + tk.name, Role: tk.role}
	case c == 4 && len(issued) > 0:
		tk := issued[d.n(len(issued))]
		q.auth, q.expect = "Bearer "+tk.secret, Principal{Subject: "token:" + tk.name, Role: tk.role}
	case c == 5:
		q.auth = "Bearer " + d.pick(TokenPrefix+"AAAA", "good", fx.joins[1].secret, "Bearer "+fx.tokens[0].secret)
	default:
		q.auth = d.pick("bearer "+fx.tokens[0].secret, "Basic b3duOm93bg==", fx.tokens[0].secret)
	}
	return q
}

// fzTokenActive says what the file says of a token's secret: the
// principal it should open (nobody when revoked, replaced or expired).
func (fx *fzFixture) fzTokenActive(t *testing.T, tk Principal, secret string) Principal {
	t.Helper()
	if tk.Subject == "" {
		return tk
	}
	var role Role
	var hash string
	var revoked, expires int64
	err := fx.r.db.QueryRow(`SELECT role, hash, revoked_at, expires_at FROM api_tokens WHERE name = ?`, strings.TrimPrefix(tk.Subject, "token:")).
		Scan(&role, &hash, &revoked, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}
	}
	if err != nil {
		t.Fatal(err)
	}
	if hash != tokenHash(secret) || revoked != 0 || (expires != 0 && expires <= fx.clock.UnixMilli()) {
		return Principal{}
	}
	return Principal{Subject: tk.Subject, Role: role}
}

// FuzzAPIAuthz sends a sequence of requests to the API, each from nobody,
// a viewer's, an operator's, an owner's token or a token the sequence
// issued. Invariants, for every request:
//   - no panic and no 5xx (no store failure is injected);
//   - the principal the API sees is the one the file says the token is
//     (a revoked, replaced or unknown token is nobody);
//   - a principal without the role the route needs (fzLeast, written out
//     independently) gets 401 (nobody) or 403, never a 2xx, and the
//     registry's state (every row of every table, the maps in memory) is
//     identical after it; so is it after any GET or HEAD, after any
//     refused request (4xx), and for a path no route serves;
//   - no answer (body or header) carries a token's or join token's
//     secret or hash, a kept secret value, a private key or the secrets
//     key, nor any text with the marker of a secret value, unless the
//     requests so far carried it (as it is or JSON-escaped); a newly
//     issued secret is in its own 201 only; only the token and
//     join-token routes answer a secret, the 201 of a member added by
//     key carries none;
//   - an error answer is JSON with an error code; a 401 says Bearer;
//   - a body past the limit is refused (413) by every route that reads one;
//   - DELETE of a cluster (204) leaves nothing of the name: no active
//     member, no settings, no kept secrets, no usable join token;
//     Removed is called once with the name exactly when it removed
//     something, never on another request; Deliver is called once with
//     the name after an accepted settings save, never otherwise;
//   - no stored settings hold a secret param or a secret's text, and no
//     sealed secret holds a known secret in plain;
//   - a join token read under a name is that name's; settings stored hold
//     no secret; a role is granted only for a sign-in host.
func FuzzAPIAuthz(f *testing.F) {
	fx := newFzFixture(f)
	var removed, delivered []string
	a := &API{Registry: fx.r, Version: "0.8.0", Sealer: fx.sealer,
		Who: func(req *http.Request) Principal {
			s, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
			if !ok {
				return Principal{}
			}
			p, ok := fx.r.TokenPrincipal(req.Context(), s)
			if !ok {
				return Principal{}
			}
			return p
		},
		Hub:         HubInfo{URL: "https://hub.example.com", PublicKeys: fx.pubs["spare3"]},
		Status:      func(string) Status { return Status{} },
		Removed:     func(n string) { removed = append(removed, n) },
		Deliver:     func(n string) { delivered = append(delivered, n) },
		SignInHosts: []string{"gitlab.example.com", "g.example:8443"},
	}
	mux := http.NewServeMux()
	a.Register(mux)
	// probe tells which route a request reaches and its path values,
	// independently of the API's handlers.
	type match struct {
		pattern  string
		name, id string
		host     string
	}
	probe := http.NewServeMux()
	var got match
	for p := range fzLeast {
		probe.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			got = match{pattern: p, name: r.PathValue("name"), id: r.PathValue("id"), host: r.PathValue("host")}
		})
	}
	probe.HandleFunc(APIPrefix, func(w http.ResponseWriter, r *http.Request) { got = match{pattern: APIPrefix} })

	// Seeds: every route by every principal with a built body, and
	// shapes the audit of the hub found dangerous.
	for i := range fzRoutes {
		for who := 0; who < 4; who++ {
			f.Add([]byte{byte(i), 0, 0, 0, 0, 0, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 3, byte(who)})
		}
	}
	// Remove the cluster ap (pending, settings and secrets kept) as the
	// operator, then read what is left of it (finding: removal kept the
	// secrets for a cluster registered again under the name).
	f.Add([]byte{4, 0, 0, 2, 0, 3, 2, 10, 0, 1, 0, 0, 2, 3, 2, 11, 0, 2, 0, 0, 2, 3, 2})
	// The owner issues a token, then the token revoked is used again.
	f.Add([]byte{17, 0, 2, 0, 0, 0, 0, 2, 3, 19, 0, 0, 0, 0, 0, 0, 0, 2, 3, 0, 1, 0, 4, 0})
	// A viewer's PUT of settings with secrets, a grant to a host nobody
	// signs in with, an escaped slash in a name.
	f.Add([]byte{10, 0, 0, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 2, 3})
	f.Add([]byte{14, 0, 3, 0, 0, 0, 0, 2, 3, 14, 0, 0, 0, 0, 3, 0, 0, 2, 3})
	f.Add([]byte{3, 0, 3, 0, 0, 2, 2, 3, 3, 3, 0, 3, 1, 2, 2, 3, 2})
	// The operator saves valid settings for eu (only gitlab_url, the
	// kept runner token stays, the S3 secrets drop): accepted, Deliver
	// called for eu.
	f.Add([]byte{10, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 2, 1, 1, 2})
	// The owner deletes rv, which only has join tokens: they are revoked,
	// no member goes, so Removed is not called (a false alarm of the
	// first round's model).
	f.Add([]byte{4, 1, 4, 0, 0, 0, 0, 0, 1, 1, 1})
	// The owner reads rv's join token 5 as "05" (a false alarm of the
	// first round's id check).
	f.Add([]byte{7, 1, 4, 0, 0, 0, 7, 0, 1, 1, 1})

	f.Fuzz(func(t *testing.T, data []byte) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		fx.reset(t)
		removed, delivered = nil, nil
		d := &fzBytes{b: data}
		var issued []fzToken
		var fresh []string // secrets issued by the sequence, each with its answer
		var seen strings.Builder
		var given []string // secret values the sequence set
		before := fx.snapshot(t)
		for step := 0; step < 8 && (step == 0 || d.more()); step++ {
			q := d.buildReq(fx, issued)
			req, err := http.NewRequest(q.method, "http://hub.example.com"+q.target, strings.NewReader(q.body))
			if err != nil {
				continue
			}
			if q.auth != "" {
				req.Header.Set("Authorization", q.auth)
			}
			// What the requests carried, as sent and as decoded.
			seen.WriteString(q.target + "\n" + req.URL.Path + "\n" + req.URL.RawQuery + "\n")
			if u, err := url.QueryUnescape(req.URL.RawQuery); err == nil {
				seen.WriteString(u + "\n")
			}
			body := q.body
			for _, s := range q.secretsSet {
				for _, f := range fzForms(s) {
					body = strings.ReplaceAll(body, f, "")
				}
			}
			seen.WriteString(body + "\n")
			var anyBody any
			if json.Unmarshal([]byte(q.body), &anyBody) == nil {
				fzStrings(anyBody, func(s string) {
					for _, g := range q.secretsSet {
						if s == g {
							return
						}
					}
					seen.WriteString(s + "\n")
				})
			}
			allSeen := seen.String() + strings.Join(q.secretsSet, "\n") + strings.Join(given, "\n")
			given = append(given, q.secretsSet...)

			got = match{}
			probe.ServeHTTP(httptest.NewRecorder(), req.Clone(context.Background()))
			m := got
			least, routed := fzLeast[m.pattern]
			if m.pattern != "" && m.pattern != APIPrefix && !routed {
				t.Fatalf("%s %s: route %q is not in fzLeast", q.method, q.target, m.pattern)
			}
			// The principal the file says, before the request changes it.
			want := Principal{}
			if q.expect.Subject != "" {
				want = fx.fzTokenActive(t, q.expect, strings.TrimPrefix(q.auth, "Bearer "))
			}
			if p := a.Who(req.Clone(context.Background())); p != want {
				t.Fatalf("%s %s: the API sees %+v, the file says %+v", q.method, q.target, p, want)
			}
			nRemoved, nDelivered := len(removed), len(delivered)
			// Whether the name is an active member before the request (a
			// removal closes its link only then).
			var activeBefore int
			if m.name != "" {
				if err := fx.r.db.QueryRow(`SELECT count(*) FROM members WHERE name = ? AND removed_at = 0`, m.name).Scan(&activeBefore); err != nil {
					t.Fatal(err)
				}
			}
			req.Body = io.NopCloser(strings.NewReader(q.body))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			after := fx.snapshot(t)
			code, out := rec.Code, rec.Body.String()
			where := fmt.Sprintf("step %d: %s %s by %q (route %q): %d %s", step, q.method, q.target, want.Subject, m.pattern, code, fzShort(out))

			if code >= 500 {
				t.Fatalf("%s", where)
			}
			ok := code >= 200 && code < 300
			denied := routed && fzRank(want) < fzRank(Principal{Subject: "x", Role: least})
			if denied {
				if ok {
					t.Fatalf("a principal without %s got a 2xx: %s", least, where)
				}
				if want.Subject == "" && code != http.StatusUnauthorized && code != http.StatusMovedPermanently {
					t.Fatalf("nobody not answered 401: %s", where)
				}
				if want.Subject != "" && code != http.StatusForbidden && code != http.StatusMovedPermanently {
					t.Fatalf("a principal without the role not answered 403: %s", where)
				}
			}
			if !routed && ok {
				t.Fatalf("a path no route serves answered 2xx: %s", where)
			}
			if after != before && (denied || !routed || code >= 300 || q.method == "GET" || q.method == "HEAD") {
				t.Fatalf("the state changed: %s\nbefore:\n%s\nafter:\n%s", where, before, after)
			}
			// Removed is called once, with the name, exactly when a DELETE
			// of a cluster removed an active member (its link to close).
			if ok && m.pattern == "DELETE /api/v1/fleet/members/{name}" && activeBefore > 0 {
				if len(removed) != nRemoved+1 || removed[nRemoved] != m.name {
					t.Fatalf("a cluster removed, Removed called with %q: %s", removed[nRemoved:], where)
				}
			} else if len(removed) != nRemoved {
				t.Fatalf("Removed called (%q) without a removal: %s", removed[nRemoved:], where)
			}
			// Deliver is called once, with the name, after a settings save
			// that was accepted, and never otherwise.
			if ok && m.pattern == "PUT /api/v1/fleet/members/{name}/settings" {
				if len(delivered) != nDelivered+1 || delivered[nDelivered] != m.name {
					t.Fatalf("settings saved, Deliver called with %q: %s", delivered[nDelivered:], where)
				}
			} else if len(delivered) != nDelivered {
				t.Fatalf("Deliver called (%q) without a settings save: %s", delivered[nDelivered:], where)
			}
			// The answer: JSON errors, nothing secret.
			if code >= 400 {
				var e struct {
					Error struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if q.method != "HEAD" && (json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Error.Code == "") {
					t.Fatalf("an error not JSON with a code: %s", where)
				}
				if code == http.StatusUnauthorized && !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
					t.Fatalf("a 401 without WWW-Authenticate: Bearer: %s", where)
				}
			}
			reads := map[string]bool{"POST /api/v1/fleet/members": true, "POST /api/v1/fleet/members/{name}/join-tokens": true,
				"PUT /api/v1/fleet/members/{name}/settings": true, "PUT /api/v1/fleet/role-bindings/gitlab/{host}/{id}": true, "POST /api/v1/fleet/tokens": true}
			if len(q.body) > fzMaxBody && reads[m.pattern] && !denied && code != http.StatusRequestEntityTooLarge && code != http.StatusMovedPermanently {
				t.Fatalf("a body past the limit not refused with 413: %s", where)
			}
			var hdr strings.Builder
			for k, vs := range rec.Header() {
				hdr.WriteString(k + ": " + strings.Join(vs, ", ") + "\n")
			}
			answer := hdr.String() + out
			// A secret issued now is in this answer, once.
			var issuedNow struct {
				Secret string `json:"secret"`
				Token  struct {
					Name string `json:"name"`
					Role Role   `json:"role"`
				} `json:"token"`
			}
			// Only the token and join-token routes issue a secret; the 201
			// of a member registered by key carries none.
			issues := m.pattern == "POST /api/v1/fleet/tokens" || m.pattern == "POST /api/v1/fleet/members/{name}/join-tokens"
			if code == http.StatusCreated {
				if err := json.Unmarshal(rec.Body.Bytes(), &issuedNow); err != nil {
					t.Fatalf("a 201 not JSON (%v): %s", err, where)
				}
				if issues && issuedNow.Secret == "" {
					t.Fatalf("a 201 of %s without a secret: %s", m.pattern, where)
				}
				if !issues && issuedNow.Secret != "" {
					t.Fatalf("a 201 of %s with a secret: %s", m.pattern, where)
				}
				if issuedNow.Secret != "" {
					answer = strings.ReplaceAll(answer, issuedNow.Secret, "")
				}
			}
			for _, s := range append(append([]string(nil), fx.secrets...), fresh...) {
				if fzCarries(answer, s) && !fzCarries(allSeen, s) {
					t.Fatalf("the answer carries a secret (%.12q...): %s", s, where)
				}
			}
			for _, s := range given {
				if fzCarries(answer, s) && !fzCarries(seen.String(), s) {
					t.Fatalf("the answer carries a secret value set before (%.12q...): %s", s, where)
				}
			}
			if strings.Contains(answer, fzMarker) && !strings.Contains(seen.String(), fzMarker) {
				t.Fatalf("the answer carries text of a secret value: %s", where)
			}
			if code == http.StatusCreated && issuedNow.Secret != "" {
				for _, s := range append(append([]string(nil), fx.secrets...), fresh...) {
					if issuedNow.Secret == s {
						t.Fatalf("a secret issued twice: %s", where)
					}
				}
				fresh = append(fresh, issuedNow.Secret, tokenHash(issuedNow.Secret), joinHash(issuedNow.Secret))
				if issuedNow.Token.Name != "" {
					issued = append(issued, fzToken{name: issuedNow.Token.Name, secret: issuedNow.Secret, role: issuedNow.Token.Role})
				}
			}
			// Route by route.
			if ok {
				fzRouteChecks(t, fx, a, m.pattern, m.name, m.id, m.host, rec, where)
			}
			fzStateChecks(t, fx, given, where)
			before = after
		}
	})
}

// fzRouteChecks are the invariants of one route's success.
func fzRouteChecks(t *testing.T, fx *fzFixture, a *API, pattern, name, id, host string, rec *httptest.ResponseRecorder, where string) {
	t.Helper()
	ctx := context.Background()
	switch pattern {
	case "DELETE /api/v1/fleet/members/{name}":
		var n int
		fx.r.db.QueryRow(`SELECT (SELECT count(*) FROM members WHERE name = ? AND removed_at = 0) + (SELECT count(*) FROM cluster_settings WHERE member = ?) +
			(SELECT count(*) FROM cluster_secrets WHERE member = ?) + (SELECT count(*) FROM join_tokens WHERE member = ? AND used_at = 0 AND revoked_at = 0 AND expires_at > ?)`,
			name, name, name, name, fx.clock.UnixMilli()).Scan(&n)
		if n != 0 {
			t.Fatalf("%d rows of %q left after its removal: %s", n, name, where)
		}
		if _, err := fx.r.Holder(""); err == nil {
			t.Fatalf("an empty key has a holder: %s", where)
		}
	case "GET /api/v1/fleet/members/{name}/join-tokens/{id}":
		var j JoinToken
		// The id as a decimal number ("05" and "+5" are 5).
		want, perr := strconv.ParseInt(id, 10, 64)
		if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil || perr != nil || j.Member != name || j.ID != want {
			t.Fatalf("a join token of another name or id: %s", where)
		}
	case "GET /api/v1/fleet/members/{name}/join-tokens":
		var js struct {
			JoinTokens []JoinToken `json:"join_tokens"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &js); err != nil {
			t.Fatalf("%v: %s", err, where)
		}
		for _, j := range js.JoinTokens {
			if j.Member != name {
				t.Fatalf("a join token of %q listed for %q: %s", j.Member, name, where)
			}
		}
	case "GET /api/v1/fleet/members/{name}/settings", "PUT /api/v1/fleet/members/{name}/settings":
		var s Settings
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil || s.Member != name {
			t.Fatalf("settings of another name: %s", where)
		}
		for k := range s.Values {
			if p, _ := ParamByKey(k); p.Kind == KindSecret || p.Key == "" {
				t.Fatalf("settings answer a value of %q: %s", k, where)
			}
		}
	case "GET /api/v1/fleet/members/{name}/values":
		dec := yaml.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("values not YAML (%v): %s", err, where)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			t.Fatalf("values hold another document: %s", where)
		}
		if got := fzDig(doc, "control-plane.fleet.name"); got != name {
			t.Fatalf("values of %v for %q: %s", got, name, where)
		}
	case "PUT /api/v1/fleet/role-bindings/gitlab/{host}/{id}":
		// The sign-in hosts as configured, compared here (not through
		// the API's own signsIn).
		ok := false
		for _, h := range a.SignInHosts {
			ok = ok || strings.EqualFold(h, host)
		}
		if !ok {
			t.Fatalf("a role granted to %q: %s", host, where)
		}
	case "POST /api/v1/fleet/members/{name}/join-tokens":
		if m, err := fx.r.Get(ctx, name); err == nil && m.RemovedAt.IsZero() {
			t.Fatalf("a join token issued for the registered member %q: %s", name, where)
		}
	}
}

// fzStateChecks hold in every state: stored settings hold only params
// that are not secrets, and no secret's text; a sealed secret holds no
// plain text of a known secret or of a secret value the sequence set
// (given).
func fzStateChecks(t *testing.T, fx *fzFixture, given []string, where string) {
	t.Helper()
	known := append(append([]string(nil), fx.secrets...), given...)
	rows, err := fx.r.db.Query(`SELECT member, params FROM cluster_settings`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var m, pj string
		if err := rows.Scan(&m, &pj); err != nil {
			t.Fatal(err)
		}
		var vals map[string]string
		if err := json.Unmarshal([]byte(pj), &vals); err != nil {
			t.Fatalf("settings of %q not JSON: %s", m, where)
		}
		for k := range vals {
			if p, ok := ParamByKey(k); !ok || p.Kind == KindSecret {
				t.Fatalf("settings of %q keep %q in plain: %s", m, k, where)
			}
		}
		for _, s := range known {
			// Short values (a bare marker) can be part of a raw plain
			// value the sequence sent: only values of 8 bytes or more count.
			if len(s) >= 8 && fzCarries(pj, s) {
				t.Fatalf("a secret (%.12q...) in the plain settings of %q: %s", s, m, where)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	srows, err := fx.r.db.Query(`SELECT member, param, sealed FROM cluster_secrets`)
	if err != nil {
		t.Fatal(err)
	}
	defer srows.Close()
	for srows.Next() {
		var m, p string
		var sealed []byte
		if err := srows.Scan(&m, &p, &sealed); err != nil {
			t.Fatal(err)
		}
		for _, s := range known {
			// A short value can occur in random bytes by chance: only
			// values of 8 bytes or more count.
			if len(s) >= 8 && bytes.Contains(sealed, []byte(s)) {
				t.Fatalf("the sealed %s of %q holds a secret (%.12q...) in plain: %s", p, m, s, where)
			}
		}
	}
	if err := srows.Err(); err != nil {
		t.Fatal(err)
	}
}

// fzStrings calls fn with every string in a decoded JSON value.
func fzStrings(v any, fn func(string)) {
	switch x := v.(type) {
	case string:
		fn(x)
	case []any:
		for _, e := range x {
			fzStrings(e, fn)
		}
	case map[string]any:
		for k, e := range x {
			fn(k)
			fzStrings(e, fn)
		}
	}
}

func fzShort(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// fzDig is the value at a dotted path of a parsed YAML map.
func fzDig(m map[string]any, path string) any {
	var cur any = m
	for _, p := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

// ---- FuzzJoinEndpoint ----

// fzCounter counts what a handler reads of a body.
type fzCounter struct {
	r io.Reader
	n int
}

func (c *fzCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// FuzzJoinEndpoint trades join tokens at Registry.JoinHandler: bodies
// built from the seed's tokens (pending, used, expired, revoked), names and
// keys, raw bytes, bodies past the limit, odd headers; between the trades
// the sequence issues tokens and removes clusters. Invariants:
//   - no panic, no 5xx; an error is JSON with a code;
//   - a 200 only for a token issued for that name; its answer is the
//     name and the fingerprint of the key sent, nothing else, and the
//     member is registered with that key;
//   - a token registers at most one key: a second registration through
//     it (another key, the member removed, ...) is refused;
//   - without a token issued for the name, no answer says who holds a
//     key (403 or 400, never 409);
//   - an answer that is not 200 changes nothing;
//   - no answer carries a secret (join and API tokens, their hashes,
//     kept secrets, private keys) the body did not carry;
//   - the handler reads at most 64 KiB of a body.
func FuzzJoinEndpoint(f *testing.F) {
	fx := newFzFixture(f)
	h := fx.r.JoinHandler()
	keyNames := []string{"spare0", "spare1", "spare2", "eu", "us"}
	f.Add([]byte{0, 1, 0, 0})                         // ap with its token and a new key: joins
	f.Add([]byte{0, 1, 0, 0, 0, 1, 0, 1})             // ap twice, another key the second time
	f.Add([]byte{0, 0, 4, 4})                         // us with its used token and its own key: as it is
	f.Add([]byte{0, 0, 4, 0})                         // us's used token, another key
	f.Add([]byte{0, 2, 2, 0})                         // the expired token
	f.Add([]byte{0, 3, 3, 0})                         // the revoked token
	f.Add([]byte{0, 1, 0, 3})                         // ap's token with eu's key: whose key?
	f.Add([]byte{0, 5, 1, 3})                         // no token, eu's key: must not say eu
	f.Add([]byte{0, 1, 1, 0})                         // ap's token for another name
	f.Add([]byte{1, 0, 0, 0, 2, 1, 0, 0, 0, 1, 0, 0}) // ap removed, then its token again
	f.Add([]byte{0, 6, 0, 0, 7})                      // a body past the limit
	f.Add([]byte{0, 7, 0, 0})                         // raw bytes

	f.Fuzz(func(t *testing.T, data []byte) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		fx.reset(t)
		ctx := context.Background()
		d := &fzBytes{b: data}
		// issued: name -> secrets issued for it; used: secret -> the
		// fingerprint it registered.
		issued := map[string][]string{}
		for _, j := range fx.joins {
			issued[j.name] = append(issued[j.name], j.secret)
		}
		fp := func(pub string) string {
			ks, err := ParsePublicKeys([]byte(pub))
			if err != nil || len(ks) != 1 {
				return ""
			}
			return Fingerprint(ks[0])
		}
		used := map[string]string{fx.joins[0].secret: fp(fx.pubs["us"])}
		secrets := append([]string(nil), fx.secrets...)
		names := []string{"us", "ap", "old", "rv", "eu", "fresh", "new", "AP", ""}
		before := fx.snapshot(t)
		for step := 0; step < 6 && (step == 0 || d.more()); step++ {
			switch d.n(8) {
			case 1:
				// Issue a token for a name (refused for a registered one).
				n := d.pick(names...)
				if _, s, err := fx.r.IssueJoin(ctx, "token:op", n, 0); err == nil {
					issued[n] = append(issued[n], s)
					secrets = append(secrets, s, joinHash(s))
				}
				before = fx.snapshot(t)
				continue
			case 2:
				fx.r.RemoveCluster(ctx, "token:op", d.pick(names...))
				before = fx.snapshot(t)
				continue
			}
			name := d.pick(names...)
			var tok string
			switch c := d.n(8); c {
			case 0, 1, 2, 3, 4:
				tok = fx.joins[[]int{1, 1, 2, 3, 0}[c]].secret
				if c == 0 && len(issued[name]) > 0 {
					tok = issued[name][len(issued[name])-1]
				}
			case 5:
				tok = d.pick("", JoinPrefix, JoinPrefix+"AAAA", fx.tokens[0].secret)
			case 6:
				tok = fx.joins[4].secret
			default:
				tok = d.str(48)
			}
			pub := fx.pubs[keyNames[d.n(len(keyNames))]]
			switch d.n(6) {
			case 0:
				pub = fx.pubs["spare0"] + fx.pubs["spare1"]
			case 1:
				pub = d.pick("", "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n", "ssh-ed25519 AAAA", "-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIEIA==\n-----END PRIVATE KEY-----\n")
			}
			b, _ := json.Marshal(map[string]string{"name": name, "token": tok, "public_key": pub})
			body := string(b)
			switch d.n(8) {
			case 0:
				body = d.str(96)
			case 1:
				body += strings.Repeat("x", 70<<10)
			case 2:
				body = `{"name":"` + name + `","token":"` + tok + `","public_key":"x","admin":true}`
			case 3:
				body = strings.Repeat(" ", 64<<10) + body
			}
			cnt := &fzCounter{r: strings.NewReader(body)}
			req := httptest.NewRequest(http.MethodPost, JoinPath, cnt)
			switch d.n(4) {
			case 1:
				req.Header.Set("Content-Type", d.pick("text/plain", "application/json; charset=utf-16"))
			case 2:
				req.ContentLength = -1
				req.Header.Set("Transfer-Encoding", "chunked")
			case 3:
				req.Header.Set("Authorization", "Bearer "+fx.tokens[0].secret)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			after := fx.snapshot(t)
			code, out := rec.Code, rec.Body.String()
			where := fmt.Sprintf("step %d: name %q token %.16q key %.40q: %d %s", step, name, tok, pub, code, fzShort(out))
			if code >= 500 {
				t.Fatalf("%s", where)
			}
			if cnt.n > 64<<10 {
				t.Fatalf("the handler read %d bytes of a body: %s", cnt.n, where)
			}
			var parsed struct {
				Name      string `json:"name"`
				Token     string `json:"token"`
				PublicKey string `json:"public_key"`
			}
			decodedOK := json.NewDecoder(strings.NewReader(body)).Decode(&parsed) == nil
			pair := false
			for _, s := range issued[parsed.Name] {
				pair = pair || (decodedOK && s == parsed.Token)
			}
			if code != http.StatusOK && after != before {
				t.Fatalf("a refused trade changed the state: %s", where)
			}
			if code == http.StatusConflict && !pair {
				t.Fatalf("a 409 (it names a key's holder) without a token issued for the name: %s", where)
			}
			if code >= 400 {
				var e struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Error.Code == "" {
					t.Fatalf("an error not JSON with a code: %s", where)
				}
			}
			for _, s := range secrets {
				if strings.Contains(out, s) && !strings.Contains(body, s) {
					t.Fatalf("the answer carries a secret (%.12s...): %s", s, where)
				}
			}
			if strings.Contains(out, fzMarker) {
				t.Fatalf("the answer carries text of a kept secret: %s", where)
			}
			if code == http.StatusOK {
				if !pair {
					t.Fatalf("a 200 without a token issued for the name: %s", where)
				}
				var ans map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &ans); err != nil || len(ans) != 2 || ans["name"] != parsed.Name || ans["fingerprint"] != fp(parsed.PublicKey) || ans["fingerprint"] == "" {
					t.Fatalf("a 200 that is not the name and the key's fingerprint: %s", where)
				}
				m, err := fx.r.Get(ctx, parsed.Name)
				if err != nil || !m.RemovedAt.IsZero() || m.Fingerprint != ans["fingerprint"] {
					t.Fatalf("a 200 without the member registered with the key (%+v, %v): %s", m, err, where)
				}
				if n, _ := fx.r.Holder(m.Fingerprint); n != parsed.Name {
					t.Fatalf("the key's holder is %q: %s", n, where)
				}
				if after != before {
					if was, ok := used[parsed.Token]; ok {
						t.Fatalf("a token used for %s registered again: %s", was, where)
					}
					used[parsed.Token] = ans["fingerprint"]
				}
			}
			before = after
		}
	})
}

// ---- FuzzSettingsValues ----

// fzShellSafe are the characters a generated command may hold outside a
// comment: none a shell interprets but the template's own pipe and line
// continuation.
func fzShellSafe(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune(" ._/:=-\n\\|", c)
}

// fzFixedPaths are the values the generation sets itself.
var fzFixedPaths = map[string]bool{
	"global.console.url": true, "control-plane.fleet.role": true, "control-plane.fleet.name": true, "control-plane.fleet.hub.url": true,
	"control-plane.fleet.hub.publicKeys": true, "control-plane.fleet.hub.caCert": true, "control-plane.fleet.key.existingSecret": true,
	"control-plane.fleet.join.existingSecret": true, "gitlab-runner.runners.secret": true, "control-plane.gitlab.token.existingSecret": true,
	"gitlab-runner.runners.cache.secretName": true,
}

// fzFlatten lists the leaves of a parsed YAML map by dotted path.
func fzFlatten(prefix string, m map[string]any, out map[string]any) {
	for k, v := range m {
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		if mm, ok := v.(map[string]any); ok {
			fzFlatten(p, mm, out)
			continue
		}
		out[p] = v
	}
}

// FuzzSettingsValues saves arbitrary values maps (1 or 2 saves, with or
// without a secrets key, for members registered by key, joined, pending
// or new) and generates the values and commands from what was kept, with
// fuzzed hub details. Invariants:
//   - a save is accepted, or refused with field errors (SettingsError,
//     every field a key of the list or of the input), or refused as
//     invalid (a secret without a secrets key, a bad name); nothing else;
//   - accepted: the kept values are exactly the input's applying,
//     non-secret, non-empty values, trimmed; no secret is among them;
//     the kept secrets are exactly those kept before or given now that
//     still apply and were not forgotten, in the answer and in the file;
//     CheckSettings agrees;
//   - the values file is one YAML document, a map whose leaves are at
//     the paths of the list's params that apply or at the generation's
//     own paths, nothing else; every kept value is there, equal (a number
//     as the number), and so are the hub's details and the member's name;
//     a cluster registered by key or joining by key names the key's
//     Secret and not the join token's, and the other way round;
//   - the commands hold, outside comments, no character a shell
//     interprets beyond the template's pipe, and no value of the settings
//     but the name; there are as many as the case asks for;
//   - no secret value is in the settings, the values or the commands.
func FuzzSettingsValues(f *testing.F) {
	fx := newFzFixture(f)
	f.Add([]byte{0, 1, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1})
	f.Add([]byte{4, 1, 0, 1, 0, 1, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2})
	f.Add([]byte{1, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1})
	// A storage path, a URL and S3 values with YAML and shell in them.
	f.Add([]byte{3, 1, 2, 0, 0, 0, 0, 1, 0, 2, 0, 2, 1, 0, 1, 2, 0, 2, 1, 2, 2, 2, 3})
	// U+0085 in the hub's address (a YAML reader folds it into a line break).
	f.Add([]byte{3, 1, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 1, 1})
	// An S3 server without a region; S3 keys without a server; a public
	// key that is not the member's.
	f.Add([]byte{0, 1, 0, 0, 0, 0, 0, 0, 0, 1, 1, 0, 1, 1, 0, 0, 1, 1})
	f.Add([]byte{2, 1, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 0, 0, 1, 1})

	f.Fuzz(func(t *testing.T, data []byte) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		fx.reset(t)
		ctx := context.Background()
		d := &fzBytes{b: data}
		member := d.pick("eu", "us", "ap", "fresh", "new", "EU", "-x")
		sealer := fx.sealer
		if d.n(4) == 0 {
			sealer = nil
		}
		var given []string
		var s Settings
		accepted := false
		for save := 0; save < 2 && (save == 0 || d.n(2) == 1); save++ {
			vals := d.settingsValues(fx, &given)
			var forget []string
			for i := d.n(3); i > 0; i-- {
				forget = append(forget, d.pick("gitlab_token", "runner_token", "s3_access_key", "gitlab_url"))
			}
			var kept map[string]bool
			if old, err := fx.r.Settings(ctx, member); err == nil {
				kept = old.Kept()
			}
			got, err := fx.r.saveSettings(ctx, sealer, "token:op", member, vals, forget)
			where := fmt.Sprintf("save %d of %q (secrets key %v) values %q forget %q", save, member, sealer != nil, vals, forget)
			var se *SettingsError
			switch {
			case errors.As(err, &se):
				if len(se.Fields) == 0 {
					t.Fatalf("refused without a field: %s", where)
				}
				for _, fe := range se.Fields {
					_, isParam := ParamByKey(fe.Key)
					_, inInput := vals[fe.Key]
					inForget := false
					for _, k := range forget {
						inForget = inForget || k == fe.Key
					}
					if !isParam && !inInput && !inForget {
						t.Fatalf("a field error for %q: %s", fe.Key, where)
					}
				}
				continue
			case errors.Is(err, ErrInvalid):
				continue
			case err != nil:
				t.Fatalf("%v: %s", err, where)
			}
			s, accepted = got, true
			if s.Member != member || !NamePattern.MatchString(member) {
				t.Fatalf("settings of %q kept for %q: %s", s.Member, member, where)
			}
			clean := map[string]string{}
			for k, v := range vals {
				if v = strings.TrimSpace(v); v != "" {
					clean[k] = v
				}
			}
			for _, p := range Params {
				v, has := s.Values[p.Key]
				switch {
				case p.Kind == KindSecret && has:
					t.Fatalf("the secret %s kept as a value: %s", p.Key, where)
				case p.Kind == KindSecret && clean[p.Key] != "" && p.Applies(clean) && sealer != nil && !s.Kept()[p.Key]:
					t.Fatalf("the secret %s given and not kept: %s", p.Key, where)
				case p.Kind != KindSecret && p.Applies(clean) && clean[p.Key] != v:
					t.Fatalf("%s kept as %q, given %q: %s", p.Key, v, clean[p.Key], where)
				case p.Kind != KindSecret && !p.Applies(clean) && has:
					t.Fatalf("%s kept though it does not apply: %s", p.Key, where)
				}
			}
			for k := range s.Values {
				if _, ok := ParamByKey(k); !ok {
					t.Fatalf("%q kept: %s", k, where)
				}
			}
			check := map[string]string{}
			for k, v := range s.Values {
				check[k] = v
			}
			if bad := CheckSettings(check, func() map[string]bool {
				if sealer == nil {
					return SecretsMadeByHand()
				}
				return s.Kept()
			}()); len(bad) > 0 {
				t.Fatalf("kept settings that CheckSettings refuses (%v): %s", bad, where)
			}
			// The kept secrets after an accepted save are exactly: those
			// kept before or given now, that still apply and were not
			// forgotten (one secrets key throughout: none is stale). The
			// answer and the file agree.
			forgot := map[string]bool{}
			for _, k := range forget {
				forgot[k] = true
			}
			wantKept := map[string]bool{}
			for _, p := range Params {
				if p.Kind == KindSecret && p.Applies(clean) && !forgot[p.Key] && (kept[p.Key] || clean[p.Key] != "") {
					wantKept[p.Key] = true
				}
			}
			stored, err := fx.r.Settings(ctx, member)
			if err != nil {
				t.Fatalf("settings saved and not read back (%v): %s", err, where)
			}
			if fmt.Sprint(s.Kept()) != fmt.Sprint(wantKept) || fmt.Sprint(stored.Kept()) != fmt.Sprint(wantKept) {
				t.Fatalf("kept secrets %v (the file: %v), expected %v (kept before %v): %s", s.Kept(), stored.Kept(), wantKept, kept, where)
			}
			sj, _ := json.Marshal(s)
			for _, g := range given {
				if strings.Contains(string(sj), g) {
					t.Fatalf("a secret value in the settings: %s", where)
				}
			}
		}
		if !accepted {
			return
		}
		hub := HubInfo{
			URL:        d.pick("https://hub.example.com", "https://hub.example.com/\u0085image: evil", "https://hub.example.com/x\n# y\nimage: evil", "https://hub.example.com:8443/base/"),
			PublicKeys: d.pick(fx.pubs["spare3"], fx.pubs["spare2"]+fx.pubs["spare3"], "  # indented\n"+fx.pubs["spare3"], strings.TrimRight(fx.pubs["spare3"], "\n")),
			CACert:     d.pick("", "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n", "x: y\n- z\n", "---\n"),
		}
		hub.URL, hub.PublicKeys, hub.CACert = strings.ToValidUTF8(hub.URL, "?"), strings.ToValidUTF8(hub.PublicKeys, "?"), strings.ToValidUTF8(hub.CACert, "?")
		// The join token is one the hub issued (its prefix and base64url),
		// never input: no raw bytes here.
		joinTok := []string{"", JoinPrefix + "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_abcde"}[d.n(2)]
		upgrade := d.n(2) == 1
		in := Generate(s, hub, "0.8.0", joinTok, upgrade)
		where := fmt.Sprintf("member %q settings %q kept %v hub %+v join %q upgrade %v\n%s\n%s", member, s.Values, s.Kept(), hub, joinTok, upgrade, in.Values, strings.Join(in.Commands, "\n"))

		// One YAML document, a map.
		dec := yaml.NewDecoder(strings.NewReader(in.Values))
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("values not YAML: %v: %s", err, where)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			t.Fatalf("values hold another document (%v): %s", err, where)
		}
		leaves := map[string]any{}
		fzFlatten("", doc, leaves)
		allowed := map[string]bool{}
		for _, p := range Params {
			if p.Path != "" && p.Applies(s.Values) {
				allowed[p.Path] = true
			}
		}
		for path, v := range leaves {
			if !allowed[path] && !fzFixedPaths[path] {
				t.Fatalf("values set %s = %v: %s", path, v, where)
			}
			switch v.(type) {
			case string, int:
			default:
				t.Fatalf("values set %s to a %T: %s", path, v, where)
			}
		}
		for _, p := range Params {
			if p.Path == "" || !p.Applies(s.Values) {
				continue
			}
			want := s.Values[p.Key]
			if want == "" {
				want = p.Default
			}
			if want == "" {
				if _, has := leaves[p.Path]; has {
					t.Fatalf("values set %s with no value: %s", p.Path, where)
				}
				continue
			}
			got, has := leaves[p.Path]
			if p.Kind == KindNumber {
				n, _ := strconv.Atoi(want)
				if gi, ok := got.(int); !has || !ok || gi != n {
					t.Fatalf("%s is %v, kept %q: %s", p.Path, got, want, where)
				}
				continue
			}
			if gs, ok := got.(string); !has || !ok || gs != want {
				t.Fatalf("%s is %#v, kept %q: %s", p.Path, got, want, where)
			}
		}
		trim := func(v any) any {
			if s, ok := v.(string); ok {
				return strings.TrimRight(s, "\n")
			}
			return v
		}
		for path, want := range map[string]string{"global.console.url": hub.URL, "control-plane.fleet.hub.url": hub.URL,
			"control-plane.fleet.name": member, "control-plane.fleet.role": "member", "control-plane.fleet.hub.publicKeys": hub.PublicKeys,
			"gitlab-runner.runners.secret": SecretRunner} {
			if trim(leaves[path]) != strings.TrimRight(want, "\n") {
				t.Fatalf("%s is %#v, not %q: %s", path, leaves[path], want, where)
			}
		}
		if hub.CACert != "" && trim(leaves["control-plane.fleet.hub.caCert"]) != strings.TrimRight(hub.CACert, "\n") {
			t.Fatalf("caCert is %#v: %s", leaves["control-plane.fleet.hub.caCert"], where)
		}
		m, err := fx.r.Get(ctx, member)
		byKey := s.Values["joins"] == "key" || (err == nil && m.RemovedAt.IsZero() && m.Origin != OriginJoin)
		_, keySecret := leaves["control-plane.fleet.key.existingSecret"]
		_, joinSecret := leaves["control-plane.fleet.join.existingSecret"]
		if keySecret != byKey || joinSecret == byKey {
			t.Fatalf("registered or joining by key %v, the values name the key's Secret %v, the join token's %v: %s", byKey, keySecret, joinSecret, where)
		}
		// The commands.
		wantN := 1
		if !upgrade {
			wantN = 2
			if joinTok != "" || byKey {
				wantN = 3
			}
		}
		if len(in.Commands) != wantN {
			t.Fatalf("%d commands, not %d: %s", len(in.Commands), wantN, where)
		}
		for _, c := range in.Commands {
			for _, line := range strings.Split(c, "\n") {
				if strings.HasPrefix(line, "#") {
					continue
				}
				for _, r := range line {
					if !fzShellSafe(r) {
						t.Fatalf("a command holds %q: %s", r, where)
					}
				}
			}
			for k, v := range s.Values {
				// A choice's value is one of a closed set (CheckSettings
				// refuses others), not text that reaches a shell.
				if p, _ := ParamByKey(k); p.Kind == KindChoice {
					continue
				}
				if len(v) >= 4 && strings.Contains(c, v) && v != member && !strings.Contains(ChartRef+" ci-accelerator 0.8.0 "+joinTok, v) {
					t.Fatalf("a command holds the value of %s: %s", k, where)
				}
			}
		}
		if in.File != member+".values.yaml" {
			t.Fatalf("file %q: %s", in.File, where)
		}
		all := in.Values + strings.Join(in.Commands, "\n")
		for _, g := range append(append([]string(nil), given...), fx.secrets...) {
			if strings.Contains(all, g) {
				t.Fatalf("a secret value in the values or commands: %s", where)
			}
		}
		if strings.Contains(all, fzMarker) {
			t.Fatalf("text of a secret value in the values or commands: %s", where)
		}
	})
}
