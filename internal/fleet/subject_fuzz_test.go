package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// canonicalHost is what a person's subject may carry as its host.
var canonicalHost = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?$`)

// subjectRig is a registry and the API over it, shared by the inputs of a
// fuzz run and emptied before each.
type subjectRig struct {
	reg *Registry
	api *API
	mux *http.ServeMux
	who Principal
}

func newSubjectRig(f testing.TB) *subjectRig {
	f.Helper()
	reg, err := OpenRegistry(context.Background(), f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { reg.Close() })
	now := time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)
	reg.now = func() time.Time { return now }
	rig := &subjectRig{reg: reg}
	rig.api = &API{Registry: reg, Version: "test", Who: func(*http.Request) Principal { return rig.who }}
	rig.mux = http.NewServeMux()
	rig.api.Register(rig.mux)
	return rig
}

func (rig *subjectRig) reset(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{`DELETE FROM role_bindings`, `DELETE FROM api_tokens`, `DELETE FROM audit`} {
		if _, err := rig.reg.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := rig.reg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
}

// state is every binding, subject -> role.
func (rig *subjectRig) state(t *testing.T) map[string]Role {
	t.Helper()
	bs, err := rig.reg.Bindings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]Role{}
	for _, b := range bs {
		m[b.Subject] = b.Role
	}
	return m
}

// variant spells a person's path (host, id) another way, by k.
func variant(host, id string, k byte) (string, string) {
	switch k % 12 {
	case 0:
		return strings.ToUpper(host), id
	case 1:
		return host, "0" + id
	case 2:
		return host + ".", id
	case 3:
		return host + ":443", id
	case 4:
		return strings.ReplaceAll(host, "k", "\u212a"), id // Kelvin sign: lower-cases to k
	case 5:
		return host, strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r - '0' + '\uff10' // full-width digits
			}
			return r
		}, id)
	case 6:
		var b strings.Builder
		for _, c := range []byte(host) {
			b.WriteString("%" + strings.ToUpper(hexByte(c)))
		}
		return b.String(), id // decoded by the mux: the same subject
	case 7:
		return host, "+" + id
	case 8:
		return host, id + "/"
	case 9:
		return host + "%00", id
	case 10:
		return " " + host, id
	default:
		return strings.ReplaceAll(host, "i", "\u0131"), id // dotless i
	}
}

func hexByte(c byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[c>>4], digits[c&15]})
}

// FuzzSubjects: who a role binding is for.
//
// Invariants:
//   - ParsePerson accepts only the canonical form: what it accepts,
//     PersonSubject writes back byte for byte (one spelling per person),
//     with an ASCII lower-case host and an id > 0; PersonSubject of what
//     ParsePerson returns is idempotent;
//   - a subject PersonSubject writes for a sign-in host either does not
//     parse (that person cannot be bound) or parses back to the lower-cased
//     host and the same id;
//   - ParseRole takes exactly viewer, operator, owner, and a principal
//     without a subject may do nothing;
//   - over the API, PUT/DELETE /role-bindings/gitlab/{host}/{id} with any
//     path: only an owner changes anything; the bindings after every call
//     are exactly what the successful calls said (a model); every binding
//     is a canonical person subject that a person signing in through the
//     hub's GitLab holds (no binding for a spelling nobody signs in as),
//     so two spellings of one person never hold two roles;
//   - no binding is ever a token's subject, and an issued API token's
//     subject is "token:<name>", never a person and never the values'
//     "token:bootstrap"; its secret opens it, a changed secret does not.
func FuzzSubjects(f *testing.F) {
	rig := newSubjectRig(f)
	type seed struct {
		signIn, path, role string
		actor, k           byte
		token, subject     string
		host               string
		id                 uint64
	}
	for _, s := range []seed{
		{"gitlab.com", "gitlab.com/7", "owner", 0, 0, "ci-bot", "gitlab:gitlab.com/7", "gitlab.com", 7},
		{"GitLab.Example.com:8443", "gitlab.example.com:8443/1", "operator", 0, 3, "deploy.v2", "gitlab:gitlab.example.com:8443/1", "GitLab.Example.com:8443", 1},
		{"gitlab.com", "GitLab.com/7", "owner", 0, 1, "bootstrap", "gitlab:GitLab.com/7", "GITLAB.COM", 7},
		{"gitlab.com", "gitlab.com/007", "owner", 0, 2, "Bootstrap", "gitlab:gitlab.com/007", "gitlab.com", 0},
		{"gitlab.com", "gitlab.com./7", "owner", 0, 2, "bootstrap.", "gitlab:gitlab.com./7", "gitlab.com.", 7},
		{"gitlab.com", "gitlab.com:443/7", "owner", 0, 3, "a", "gitlab:gitlab.com:0443/7", "gitlab.com:443", 7},
		{"kube.example", "kube.example/9", "owner", 0, 4, "token:x", "gitlab:\u212aube.example/9", "\u212aube.example", 9},
		{"gitlab.com", "gitlab.com/\uff17", "owner", 0, 5, "x/y", "gitlab:gitlab.com/\uff17", "gitlab.com", 7},
		{"gitlab.com", "%67itlab.com/7", "owner", 0, 6, "-x", "gitlab:gitlab.com/7\n", "gitlab.com\n", 7},
		{"gitlab.com", "gitlab.com/7", "operator", 1, 0, "ok", "token:bootstrap", "gitlab.com", 7},
		{"gitlab.com", "gitlab.com/7", "owner", 2, 0, "ok", "gitlab:gitlab.com/18446744073709551615", "gitlab.com", 18446744073709551615},
		{"gitlab.com", "gitlab.com/18446744073709551616", "owner", 0, 7, "ok", "gitlab:gitlab.com/18446744073709551616", "gitlab.com", 1},
		{"gitlab.com", "other.example/7", "owner", 0, 0, "ok", "gitlab:/7", "", 7},
		{"", "anything.example/7", "owner", 0, 8, "ok", "gitlab:a:/7", "a:", 7},
		{"gitlab.com", "gitlab.com/7%2F8", "owner", 0, 9, "ok", "gitlab:gitlab.com/7/8", "gitlab.com/x", 7},
		{"gitlab.com", "gitlab.com/../../tokens/x", "owner", 0, 10, "ok", " gitlab:gitlab.com/7", "gitlab.com", 7},
		{"gitlab.com", "gitlab.com/7", "viewer", 0, 11, "ok", "gitlab:gitlab.com/7 ", "gitlab.com", 7},
		{"gitlab.com", "gitlab.com/7", "Owner", 0, 0, "ok", "GITLAB:gitlab.com/7", "gitlab.com", 7},
		{"[::1]:8443", "[::1]:8443/7", "owner", 0, 0, "ok", "gitlab:[::1]:8443/7", "[::1]:8443", 7},
		{"gitlab.com", "gitlab.com/7", "owner\x00", 0, 0, "ok", "gitlab:gitlab.com/+7", "gitlab.com", 7},
	} {
		f.Add(s.signIn, s.path, s.role, s.actor, s.k, s.token, s.subject, s.host, s.id)
	}
	f.Fuzz(func(t *testing.T, signIn, path, role string, actor, k byte, token, subject, host string, id uint64) {
		ctx := context.Background()

		// ---- the pure functions ----
		if h, n, err := ParsePerson(subject); err == nil {
			if back := PersonSubject(h, n); back != subject {
				t.Fatalf("ParsePerson(%q) = %q, %d, written back as %q: two spellings of one person", subject, h, n, back)
			}
			if n == 0 || !canonicalHost.MatchString(h) {
				t.Fatalf("ParsePerson(%q) = %q, %d: not a person", subject, h, n)
			}
			if h2, n2, err := ParsePerson(PersonSubject(h, n)); err != nil || h2 != h || n2 != n {
				t.Fatalf("PersonSubject(ParsePerson(%q)) does not parse back: %q, %d, %v", subject, h2, n2, err)
			}
		}
		if strings.HasPrefix(subject, "token:") || subject == "local" {
			if _, _, err := ParsePerson(subject); err == nil {
				t.Fatalf("ParsePerson(%q) took a non-person subject", subject)
			}
		}
		ps := PersonSubject(host, id)
		if h, n, err := ParsePerson(ps); err == nil && (h != strings.ToLower(host) || n != id) {
			t.Fatalf("PersonSubject(%q, %d) = %q parses as %q, %d", host, id, ps, h, n)
		}
		r, err := ParseRole(role)
		if ok := role == "viewer" || role == "operator" || role == "owner"; ok != (err == nil) || (ok && string(r) != role) {
			t.Fatalf("ParseRole(%q) = %q, %v", role, r, err)
		}
		for _, perm := range []string{PermRolesWrite, PermTokensWrite, PermMembersWrite, PermAllProjects, ""} {
			if (Principal{Role: Owner}).Can(perm) {
				t.Fatalf("a principal without a subject may %q", perm)
			}
		}

		// ---- the API ----
		rig.reset(t)
		signIn = strings.ToLower(signIn) // as the hub builds SignInHosts
		rig.api.SignInHosts = nil
		if signIn != "" {
			rig.api.SignInHosts = []string{signIn}
		}
		rig.who = []Principal{
			{Subject: "token:fuzz-owner", Role: Owner},
			{Subject: "token:fuzz-operator", Role: Operator},
			{Subject: "gitlab:gitlab.com/99", Role: Viewer},
			{},
		}[actor%4]
		model := map[string]Role{}
		check := func(step string) {
			t.Helper()
			got := rig.state(t)
			if len(got) != len(model) {
				t.Fatalf("%s: bindings %v, the calls said %v", step, got, model)
			}
			for s, ro := range got {
				if model[s] != ro {
					t.Fatalf("%s: bindings %v, the calls said %v", step, got, model)
				}
				h, n, err := ParsePerson(s)
				if err != nil || PersonSubject(h, n) != s {
					t.Fatalf("%s: binding %q is not a canonical person subject (%v)", step, s, err)
				}
				if signIn != "" && PersonSubject(signIn, n) != s {
					t.Fatalf("%s: binding %q, but people sign in as %q", step, s, PersonSubject(signIn, n))
				}
				if rig.reg.RoleOf(s) != ro {
					t.Fatalf("%s: RoleOf(%q) = %q, bound %q", step, s, rig.reg.RoleOf(s), ro)
				}
			}
		}
		call := func(method, p, body string) {
			t.Helper()
			req, err := http.NewRequest(method, "http://hub.test/api/v1/fleet/role-bindings/gitlab/"+p, strings.NewReader(body))
			if err != nil {
				return // not a request a client can send
			}
			w := httptest.NewRecorder()
			rig.mux.ServeHTTP(w, req)
			step := method + " " + p + " " + body
			switch {
			case w.Code/100 == 2 && rig.who.Role != Owner:
				t.Fatalf("%s by %q (%s): %d", step, rig.who.Subject, rig.who.Role, w.Code)
			case method == http.MethodPut && w.Code == http.StatusOK:
				var b Binding
				if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
					t.Fatalf("%s: %d %s", step, w.Code, w.Body)
				}
				model[b.Subject] = b.Role
			case w.Code == http.StatusNoContent:
				// The subject as the mux takes it from the path: the two
				// segments after gitlab/, each unescaped.
				segs := strings.Split(req.URL.EscapedPath(), "/")
				var h, rest string
				if len(segs) == 8 {
					h, _ = url.PathUnescape(segs[6])
					rest, _ = url.PathUnescape(segs[7])
				}
				if _, n, err := ParsePerson("gitlab:" + h + "/" + rest); err == nil {
					delete(model, PersonSubject(h, n))
				} else {
					t.Fatalf("%s: %d for a path that names no person", step, w.Code)
				}
			}
			check(step)
		}
		body := `{"role":` + string(mustJSON(role)) + `}`
		call(http.MethodPut, path, body)
		if rig.who.Role == Owner && signIn != "" && (role == "owner" || role == "operator") {
			if _, n, err := ParsePerson("gitlab:" + path); err == nil && PersonSubject(signIn, n) == "gitlab:"+path {
				if got := rig.reg.RoleOf(PersonSubject(signIn, n)); got != Role(role) {
					t.Fatalf("an owner's PUT %s %s left the person %q", path, body, got)
				}
			}
		}
		h, n, ok := strings.Cut(path, "/")
		if !ok {
			n = "7"
		}
		vh, vn := variant(h, n, k)
		other := "operator"
		if role == "operator" {
			other = "owner"
		}
		call(http.MethodPut, vh+"/"+vn, `{"role":"`+other+`"}`)
		if k&0x80 != 0 {
			call(http.MethodDelete, vh+"/"+vn, "")
		} else {
			call(http.MethodPut, path, `{"role":"viewer"}`)
		}

		// ---- tokens ----
		tok, secret, err := rig.reg.IssueToken(ctx, "token:fuzz-owner", token, Role(role), 0)
		if err != nil {
			return
		}
		sub := "token:" + tok.Name
		if tok.Name != token || sub == "token:"+BootstrapToken {
			t.Fatalf("IssueToken(%q) issued %q", token, tok.Name)
		}
		if _, _, err := ParsePerson(sub); err == nil {
			t.Fatalf("token subject %q parses as a person", sub)
		}
		p, ok := rig.reg.TokenPrincipal(ctx, secret)
		if !ok || p.Subject != sub || p.Role != Role(role) {
			t.Fatalf("TokenPrincipal(secret of %q) = %+v, %v", token, p, ok)
		}
		bad := secret[:len(secret)-1] + string(secret[len(secret)-1]^1)
		if p, ok := rig.reg.TokenPrincipal(ctx, bad); ok {
			t.Fatalf("a changed secret opens %+v", p)
		}
		if _, ok := rig.reg.TokenPrincipal(ctx, strings.TrimPrefix(secret, TokenPrefix)); ok {
			t.Fatal("a secret without its prefix opens the token")
		}
		if _, err := rig.reg.Grant(ctx, "token:fuzz-owner", OriginAPI, sub, Owner); err == nil {
			t.Fatalf("a token's subject %q got a binding", sub)
		}
	})
}

func mustJSON(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}
