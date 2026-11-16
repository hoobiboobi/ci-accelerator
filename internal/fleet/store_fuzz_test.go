package fleet

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fuzz targets for the hub's store (fleet.db) as a state machine: the
// registry, roles, API and join tokens, backup and restore. Every target
// checks invariants, not only the absence of panics; a broken invariant
// fails the input with t.Fatalf so that the fuzzer records it.
//
// Helpers here start with "sfz" so that they never clash with the other
// test files of the package.

// sfzEpoch is the fuzzed registries' start time (a whole second).
var sfzEpoch = time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC).UnixMilli()

// sfzKeySet: three distinct member keys, a second spelling of key 0 (a
// note block and a comment line before it: the same key, the same
// fingerprint) and something that is no key at all.
type sfzKeySet struct {
	pem [5]string
	fp  [5]string // "" for the one that is no key
}

var (
	sfzKeysOnce sync.Once
	sfzKeysVal  sfzKeySet
	sfzKeysErr  error
	sfzFree     string // a key no fuzzed registry holds before the input
)

func sfzKeys(t testing.TB) sfzKeySet {
	sfzKeysOnce.Do(func() {
		for i := 0; i < 3; i++ {
			_, pub, err := GenerateKey()
			if err != nil {
				sfzKeysErr = err
				return
			}
			sfzKeysVal.pem[i] = string(pub)
		}
		sfzKeysVal.pem[3] = "# the same key\n-----BEGIN NOTE-----\nAAAA\n-----END NOTE-----\n" + sfzKeysVal.pem[0]
		sfzKeysVal.pem[4] = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI"
		for i := 0; i < 4; i++ {
			_, fp, err := memberKey(sfzKeysVal.pem[i])
			if err != nil {
				sfzKeysErr = err
				return
			}
			sfzKeysVal.fp[i] = fp
		}
		_, pub, err := GenerateKey()
		if err != nil {
			sfzKeysErr = err
			return
		}
		sfzFree = string(pub)
	})
	if sfzKeysErr != nil {
		t.Fatal(sfzKeysErr)
	}
	return sfzKeysVal
}

// sfzClass is an error's kind as the callers of the registry tell them
// apart.
func sfzClass(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrInvalid):
		return "invalid"
	case errors.Is(err, ErrExists):
		return "exists"
	case errors.Is(err, ErrNotFound):
		return "notfound"
	case errors.Is(err, ErrDenied):
		return "denied"
	case errors.Is(err, ErrLocked):
		return "locked"
	}
	return "other: " + err.Error()
}

func sfzCount(t testing.TB, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

// ---- FuzzRegistryOps ----

// FuzzRegistryOps turns the input into 1..40 operations (4 bytes each:
// the operation and three arguments) over a small universe: 4 member
// names ("new" is not a valid one), 3 keys (plus a second spelling of
// key 0 and a non-key), 3 people (plus 4 non-canonical spellings of
// them), 3 token names ("bootstrap" is refused), 5 actors. A model of the
// store predicts every outcome; after every operation:
//
//   - the operation's result (ok, invalid, exists, notfound, denied,
//     locked) is the model's, and no operation panics;
//   - a key is held by at most one member not removed, and Holder names
//     the model's holder for every key;
//   - the members, join tokens, API tokens and role bindings in the file
//     are the model's (a used, revoked or expired join token never
//     registers; a join token of a removed cluster is revoked);
//   - RoleOf is the model's role for every person and Viewer for every
//     other spelling of a person and for a token's subject; an owner from
//     the values is changed neither by Grant nor by Revoke, and the last
//     owner stays unless a local command takes the role;
//   - TokenPrincipal yields a principal only for the current secret of
//     an API token neither revoked nor expired, with the model's role;
//   - the audit grows with every change of state and only then (Prune
//     forgets old records: the count starts again after it);
//   - no error text holds a token's secret.
func FuzzRegistryOps(f *testing.F) {
	for _, s := range sfzRegistrySeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		keys := sfzKeys(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r, err := OpenRegistry(ctx, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		r.Log = quiet
		m := &sfzModel{now: sfzEpoch, members: map[string]*sfzMember{}, bindings: map[string]sfzBinding{}, tokens: map[string]*sfzToken{}}
		r.now = func() time.Time { return time.UnixMilli(m.now).UTC() }
		r.KeepRemoved = 2 * time.Hour
		r.KeepAudit = 6 * time.Hour
		n := min(max(len(data)/4, 1), 40)
		var trace []string
		auditN := sfzCount(t, r.db, "audit")
		for i := 0; i < n; i++ {
			at := func(k int) byte {
				if 4*i+k < len(data) {
					return data[4*i+k]
				}
				return 0
			}
			st := sfzApply(ctx, t, r, m, keys, at(0), at(1), at(2), at(3))
			trace = append(trace, st.desc+" -> "+sfzClass(st.err))
			fail := func(format string, args ...any) {
				t.Helper()
				t.Fatalf("operations:\n  %s\n"+format, append([]any{strings.Join(trace, "\n  ")}, args...)...)
			}
			if ctx.Err() != nil {
				fail("the operations took more than 30 s")
			}
			if st.want != "" && sfzClass(st.err) != st.want {
				fail("result %q, the model says %q", sfzClass(st.err), st.want)
			}
			if st.bad != "" {
				fail("%s", st.bad)
			}
			if st.err != nil {
				for _, s := range m.secrets() {
					if strings.Contains(st.err.Error(), s) {
						fail("the error holds a token's secret: %v", st.err)
					}
				}
			}
			cnt := sfzCount(t, r.db, "audit")
			switch {
			case st.pruned:
			case st.changed && cnt <= auditN:
				fail("the state changed and the audit did not grow (%d -> %d)", auditN, cnt)
			case !st.changed && cnt != auditN:
				fail("nothing changed and the audit went %d -> %d", auditN, cnt)
			}
			auditN = cnt
			if msg := sfzCheck(ctx, r, m, keys); msg != "" {
				fail("%s", msg)
			}
		}
		// Every API secret ever issued, at the end.
		for _, s := range m.apiSecrets {
			want, wantOK := m.principal(s)
			got, ok := r.TokenPrincipal(ctx, s)
			if ok != wantOK || got != want {
				t.Fatalf("operations:\n  %s\nTokenPrincipal of a secret of %s: %+v %v, the model says %+v %v",
					strings.Join(trace, "\n  "), want.Subject, got, ok, want, wantOK)
			}
		}
	})
}

var (
	sfzNames      = []string{"eu", "us", "ap-1", "new"}
	sfzPeople     = []string{"gitlab:gitlab.example.com/1", "gitlab:gitlab.example.com/2", "gitlab:gitlab.example.com/3"}
	sfzSubjects   = append(append([]string{}, sfzPeople...), "gitlab:GitLab.example.com/1", "gitlab:gitlab.example.com/02", " gitlab:gitlab.example.com/3", "gitlab:gitlab.example.com/1/")
	sfzActors     = append(append([]string{}, sfzPeople...), OriginLocal, "token:ci")
	sfzTokenNames = []string{"ci", "tf", "bootstrap"}
	sfzRoles      = []Role{Viewer, Operator, Owner, Role("admin")}
	sfzSteps      = []time.Duration{0, time.Millisecond, 30 * time.Second, time.Minute, time.Hour - time.Millisecond, time.Hour, 2 * time.Hour, 25 * time.Hour, 7 * 24 * time.Hour}
	sfzJoinTTLs   = []time.Duration{0, time.Minute, 30 * time.Minute, 7 * 24 * time.Hour, 59 * time.Second, 7*24*time.Hour + time.Millisecond}
	sfzTokenTTLs  = []time.Duration{0, time.Minute, 2 * time.Hour, -time.Second, time.Millisecond}
)

type sfzMember struct {
	fp      string
	removed int64
}

type sfzJoinRow struct {
	id                     int64
	name, secret           string
	expires, used, revoked int64
	pruned                 bool
}

type sfzBinding struct {
	role   Role
	origin string
}

type sfzToken struct {
	secret           string
	role             Role
	expires, revoked int64
}

type sfzModel struct {
	now        int64
	members    map[string]*sfzMember
	joins      []*sfzJoinRow
	bindings   map[string]sfzBinding
	tokens     map[string]*sfzToken
	apiSecrets []string
}

func (m *sfzModel) active(name string) bool {
	mm, ok := m.members[name]
	return ok && mm.removed == 0
}

// holder is the member not removed that holds fp, "" when none does.
func (m *sfzModel) holder(fp string) string {
	for n, mm := range m.members {
		if mm.removed == 0 && mm.fp == fp {
			return n
		}
	}
	return ""
}

// register is a successful Add or Join: a removed record of the name and
// a removed member holding the key go.
func (m *sfzModel) register(name, fp string) {
	delete(m.members, name)
	for n, mm := range m.members {
		if mm.removed != 0 && mm.fp == fp {
			delete(m.members, n)
		}
	}
	m.members[name] = &sfzMember{fp: fp}
}

func (m *sfzModel) findJoin(secret, name string) *sfzJoinRow {
	for _, j := range m.joins {
		if !j.pruned && j.secret == secret && j.name == name {
			return j
		}
	}
	return nil
}

func (m *sfzModel) otherOwners(subject string) int {
	n := 0
	for s, b := range m.bindings {
		if s != subject && b.role == Owner {
			n++
		}
	}
	return n
}

func (m *sfzModel) secrets() []string {
	out := append([]string{}, m.apiSecrets...)
	for _, j := range m.joins {
		out = append(out, j.secret)
	}
	return out
}

func (m *sfzModel) principal(secret string) (Principal, bool) {
	for name, tk := range m.tokens {
		if tk.secret == secret && tk.revoked == 0 && (tk.expires == 0 || m.now < tk.expires) {
			return Principal{Subject: "token:" + name, Role: tk.role}, true
		}
	}
	return Principal{}, false
}

// revokeBinding is Revoke (and Grant with Viewer) in the model.
func (m *sfzModel) revokeBinding(actor, subject string) (want string, changed bool) {
	b, ok := m.bindings[subject]
	switch {
	case !ok:
		return "ok", false
	case b.origin == OriginValues:
		return "locked", false
	case b.role == Owner && actor != OriginLocal && m.otherOwners(subject) == 0:
		return "locked", false
	}
	delete(m.bindings, subject)
	return "ok", true
}

// sfzStep is one operation's outcome: what the model expects ("" when
// the call returns no error to compare) and whether the state changed.
type sfzStep struct {
	desc    string
	want    string
	err     error
	changed bool
	pruned  bool
	bad     string // a broken invariant found inside the operation
}

// sfzVariant is a secret as a caller may send it: exact or close to it.
func sfzVariant(s string, v byte, other string) string {
	switch v % 6 {
	case 1:
		if s == "" {
			return "x"
		}
		last := s[len(s)-1]
		if last == 'A' {
			last = 'B'
		} else {
			last = 'A'
		}
		return s[:len(s)-1] + string(last)
	case 2:
		return s + "A"
	case 3:
		if len(s) > 0 {
			return s[:len(s)-1]
		}
		return ""
	case 4:
		return other
	case 5:
		return ""
	}
	return s
}

func sfzApply(ctx context.Context, t *testing.T, r *Registry, m *sfzModel, keys sfzKeySet, op, a, b, c byte) (st sfzStep) {
	actor := sfzActors[int(a)%len(sfzActors)]
	step := sfzSteps[int(a)%len(sfzSteps)]
	switch op % 16 {
	case 0: // Add
		origin := []string{OriginUI, OriginAPI}[(a>>4)&1]
		name, k := sfzNames[int(b)%len(sfzNames)], int(c)%5
		st.desc = fmt.Sprintf("Add(%s, %s, %s, key%d)", actor, origin, name, k)
		fp := keys.fp[k]
		st.want = "ok"
		switch {
		case name == "new" || fp == "":
			st.want = "invalid"
		case m.active(name), m.holder(fp) != "":
			st.want = "exists"
		}
		var got Member
		got, st.err = r.Add(ctx, actor, origin, name, keys.pem[k])
		if st.want == "ok" && st.err == nil {
			m.register(name, fp)
			st.changed = true
			if got.Name != name || got.Fingerprint != fp || !got.RemovedAt.IsZero() {
				st.bad = fmt.Sprintf("Add returned %+v", got)
			}
		}
	case 1, 2: // Remove, RemoveCluster
		name := sfzNames[int(b)%len(sfzNames)]
		member := m.active(name)
		joins := 0
		for _, j := range m.joins {
			if !j.pruned && j.name == name && j.used == 0 && j.revoked == 0 && j.expires > m.now {
				joins++
			}
		}
		st.changed = member || joins > 0
		if op%16 == 1 {
			st.desc = fmt.Sprintf("Remove(%s, %s)", actor, name)
			st.want = "notfound"
			if member {
				st.want = "ok"
			}
			st.err = r.Remove(ctx, actor, name)
		} else {
			st.desc = fmt.Sprintf("RemoveCluster(%s, %s)", actor, name)
			st.want = "notfound"
			if st.changed {
				st.want = "ok"
			}
			var removed bool
			removed, st.err = r.RemoveCluster(ctx, actor, name)
			if removed != member {
				st.bad = fmt.Sprintf("RemoveCluster said removed=%v, the model %v", removed, member)
			}
		}
		if st.changed {
			if member {
				m.members[name].removed = m.now
			}
			for _, j := range m.joins {
				if !j.pruned && j.name == name && j.used == 0 && j.revoked == 0 && j.expires > m.now {
					j.revoked = m.now
				}
			}
		}
	case 3: // IssueJoin
		name, ttl := sfzNames[int(b)%len(sfzNames)], sfzJoinTTLs[int(c)%len(sfzJoinTTLs)]
		st.desc = fmt.Sprintf("IssueJoin(%s, %s, %s)", actor, name, ttl)
		eff := ttl
		if eff == 0 {
			eff = DefaultJoinLifetime
		}
		st.want = "ok"
		switch {
		case name == "new" || eff < time.Minute || eff > MaxJoinLifetime:
			st.want = "invalid"
		case m.active(name):
			st.want = "exists"
		}
		var j JoinToken
		var secret string
		j, secret, st.err = r.IssueJoin(ctx, actor, name, ttl)
		if st.want == "ok" && st.err == nil {
			for _, o := range m.joins {
				if !o.pruned && o.name == name && o.used == 0 && o.revoked == 0 && o.expires > m.now {
					o.revoked = m.now
				}
			}
			nj := &sfzJoinRow{id: j.ID, name: name, secret: secret, expires: m.now + eff.Milliseconds()}
			m.joins = append(m.joins, nj)
			st.changed = true
			if j.ExpiresAt.UnixMilli() != nj.expires || j.Member != name || !strings.HasPrefix(secret, JoinPrefix) {
				st.bad = fmt.Sprintf("IssueJoin returned %+v", j)
			}
		}
	case 4: // Join
		var tok *sfzJoinRow
		secret, own := JoinPrefix+"never-issued", "eu"
		if len(m.joins) > 0 {
			tok = m.joins[int(a)%len(m.joins)]
			secret, own = tok.secret, tok.name
		}
		other := ""
		if len(m.apiSecrets) > 0 {
			other = m.apiSecrets[len(m.apiSecrets)-1]
		}
		secret = sfzVariant(secret, c>>3, other)
		name := own
		if b%2 == 1 {
			name = sfzNames[int(b>>1)%len(sfzNames)]
		}
		k := int(c) % 5
		fp := keys.fp[k]
		st.desc = fmt.Sprintf("Join(%s, secret v%d of token %v, key%d)", name, (c>>3)%6, tok != nil, k)
		row := m.findJoin(secret, name)
		holder := m.holder(fp)
		register := false
		switch {
		case fp == "":
			st.want = "invalid"
		case row == nil:
			st.want = "denied"
		case holder == name:
			st.want = "ok"
		case holder != "":
			st.want = "exists"
		case row.used != 0 || row.revoked != 0 || row.expires <= m.now:
			st.want = "denied"
		case m.active(name):
			st.want = "exists"
		default:
			st.want, register = "ok", true
		}
		var got Member
		got, st.err = r.Join(ctx, name, secret, keys.pem[k])
		if register && st.err == nil {
			row.used = m.now
			m.register(name, fp)
			st.changed = true
		}
		if st.err == nil && (got.Name != name || got.Fingerprint != fp) {
			st.bad = fmt.Sprintf("Join returned %+v", got)
		}
		if st.err != nil && row == nil {
			// Without a token for the name, nothing about the members.
			for n := range m.members {
				if strings.Contains(st.err.Error(), `"`+n+`"`) {
					st.bad = fmt.Sprintf("a join without a token names member %q: %v", n, st.err)
				}
			}
		}
	case 5: // RevokeJoin
		id, name := int64(999), sfzNames[int(b)%len(sfzNames)]
		if len(m.joins) > 0 && a&0x80 == 0 {
			j := m.joins[int(a)%len(m.joins)]
			id = j.id
			if b%2 == 0 {
				name = j.name
			}
		}
		st.desc = fmt.Sprintf("RevokeJoin(%s, %s, #%d)", actor, name, id)
		st.want = "ok"
		st.err = r.RevokeJoin(ctx, actor, name, id)
		for _, j := range m.joins {
			if !j.pruned && j.id == id && j.name == name && j.used == 0 && j.revoked == 0 {
				j.revoked = m.now
				st.changed = true
			}
		}
	case 6: // Grant
		subject := sfzSubjects[int(b)%len(sfzSubjects)]
		role := sfzRoles[int(c)%len(sfzRoles)]
		origin := []string{OriginUI, OriginAPI, OriginLocal}[int(c>>2)%3]
		st.desc = fmt.Sprintf("Grant(%s, %s, %q, %s)", actor, origin, subject, role)
		ex, ok := m.bindings[subject]
		switch {
		case int(b)%len(sfzSubjects) >= len(sfzPeople), role == "admin":
			st.want = "invalid"
		case role == Viewer:
			st.want, st.changed = m.revokeBinding(actor, subject)
		case ok && ex.origin == OriginValues:
			st.want = "locked"
		case ok && ex.role == role:
			st.want = "ok"
		case ok && ex.role == Owner && actor != OriginLocal && m.otherOwners(subject) == 0:
			st.want = "locked"
		default:
			st.want, st.changed = "ok", true
			m.bindings[subject] = sfzBinding{role: role, origin: origin}
		}
		_, st.err = r.Grant(ctx, actor, origin, subject, role)
	case 7: // Revoke
		subject := sfzSubjects[int(b)%len(sfzSubjects)]
		st.desc = fmt.Sprintf("Revoke(%s, %q)", actor, subject)
		st.want, st.changed = m.revokeBinding(actor, subject)
		st.err = r.Revoke(ctx, actor, subject)
	case 8: // SyncOwners
		var subjects []string
		want := map[string]bool{}
		for i, p := range sfzPeople {
			if a&(1<<i) != 0 {
				subjects = append(subjects, p)
				want[p] = true
			}
		}
		st.desc = fmt.Sprintf("SyncOwners(%v)", subjects)
		st.want = "ok"
		for s, bd := range m.bindings {
			if bd.origin == OriginValues && !want[s] {
				delete(m.bindings, s)
				st.changed = true
			}
		}
		for s := range want {
			if bd, ok := m.bindings[s]; !ok || bd.origin != OriginValues {
				m.bindings[s] = sfzBinding{role: Owner, origin: OriginValues}
				st.changed = true
			}
		}
		st.err = r.SyncOwners(ctx, subjects)
	case 9: // IssueToken
		name := sfzTokenNames[int(b)%len(sfzTokenNames)]
		role := sfzRoles[int(c)%len(sfzRoles)]
		ttl := sfzTokenTTLs[int(c>>2)%len(sfzTokenTTLs)]
		st.desc = fmt.Sprintf("IssueToken(%s, %s, %s, %s)", actor, name, role, ttl)
		tk := m.tokens[name]
		switch {
		case name == "bootstrap" || role == "admin" || ttl < 0:
			st.want = "invalid"
		case tk != nil && tk.revoked == 0 && (tk.expires == 0 || tk.expires > m.now):
			st.want = "exists"
		default:
			st.want = "ok"
		}
		var got Token
		var secret string
		got, secret, st.err = r.IssueToken(ctx, actor, name, role, ttl)
		if st.want == "ok" && st.err == nil {
			nt := &sfzToken{secret: secret, role: role}
			if ttl > 0 {
				nt.expires = m.now + ttl.Milliseconds()
			}
			m.tokens[name] = nt
			m.apiSecrets = append(m.apiSecrets, secret)
			st.changed = true
			if got.Name != name || got.Role != role || got.ExpiresAt.UnixMilli() != nt.expires && nt.expires != 0 ||
				!strings.HasPrefix(secret, TokenPrefix) || !strings.HasSuffix(secret, got.Hint) || len(got.Hint) != 4 {
				st.bad = fmt.Sprintf("IssueToken returned %+v", got)
			}
		}
	case 10: // RevokeToken
		name := sfzTokenNames[int(b)%len(sfzTokenNames)]
		st.desc = fmt.Sprintf("RevokeToken(%s, %s)", actor, name)
		st.want = "ok"
		if tk := m.tokens[name]; tk != nil && tk.revoked == 0 {
			tk.revoked = m.now
			st.changed = true
		}
		st.err = r.RevokeToken(ctx, actor, name)
	case 11: // TokenPrincipal
		secret := TokenPrefix + "never-issued"
		if len(m.apiSecrets) > 0 {
			secret = m.apiSecrets[int(a)%len(m.apiSecrets)]
		}
		other := TokenPrefix
		if len(m.joins) > 0 {
			other = m.joins[len(m.joins)-1].secret
		}
		v := b % 8
		switch v {
		case 6:
			secret = strings.TrimPrefix(secret, TokenPrefix)
		case 7:
			secret = strings.ToUpper(secret)
		default:
			secret = sfzVariant(secret, v, other)
		}
		st.desc = fmt.Sprintf("TokenPrincipal(secret v%d)", v)
		want, wantOK := m.principal(secret)
		got, ok := r.TokenPrincipal(ctx, secret)
		if ok != wantOK || got != want {
			st.bad = fmt.Sprintf("TokenPrincipal: %+v %v, the model says %+v %v", got, ok, want, wantOK)
		}
	case 12: // VoidTokens
		st.desc = "VoidTokens"
		st.want, st.changed = "ok", true
		api := 0
		for _, tk := range m.tokens {
			if tk.revoked == 0 && (tk.expires == 0 || tk.expires > m.now) {
				tk.revoked = m.now
				api++
			}
		}
		for _, j := range m.joins {
			if !j.pruned && j.used == 0 && j.revoked == 0 && j.expires > m.now {
				j.revoked = m.now
			}
		}
		var n int
		n, st.err = r.VoidTokens(ctx, actor, "fuzz")
		if st.err == nil && n != api {
			st.bad = fmt.Sprintf("VoidTokens voided %d API tokens, the model %d", n, api)
		}
	case 13: // Refresh
		st.desc = "Refresh"
		st.want = "ok"
		st.err = r.Refresh(ctx)
	case 14: // Prune, after the clock moves
		m.now += step.Milliseconds()
		st.desc = fmt.Sprintf("Prune after +%s", step)
		st.want, st.pruned = "ok", true
		ended := m.now - (2 * time.Hour).Milliseconds()
		for n, mm := range m.members {
			if mm.removed != 0 && mm.removed < ended {
				delete(m.members, n)
			}
		}
		for _, j := range m.joins {
			if j.expires < ended || (j.used != 0 && j.used < ended) || (j.revoked != 0 && j.revoked < ended) {
				j.pruned = true
			}
		}
		for n, tk := range m.tokens {
			if (tk.revoked != 0 && tk.revoked < ended) || (tk.expires != 0 && tk.expires < ended) {
				delete(m.tokens, n)
			}
		}
		st.err = r.Prune(ctx)
	case 15: // the clock moves
		m.now += step.Milliseconds()
		st.desc = fmt.Sprintf("Advance +%s", step)
	}
	return st
}

// sfzCheck compares the file and the registry's maps with the model.
func sfzCheck(ctx context.Context, r *Registry, m *sfzModel, keys sfzKeySet) string {
	ms, err := r.List(ctx)
	if err != nil {
		return "List: " + err.Error()
	}
	activeByFP := map[string]string{}
	seen := map[string]bool{}
	for _, got := range ms {
		seen[got.Name] = true
		want, ok := m.members[got.Name]
		if !ok {
			return fmt.Sprintf("member %q in the file, not in the model", got.Name)
		}
		if got.Fingerprint != want.fp || got.RemovedAt.UnixMilli() != want.removed && want.removed != 0 || got.RemovedAt.IsZero() != (want.removed == 0) {
			return fmt.Sprintf("member %q: file %s removed %v, model %s removed %d", got.Name, got.Fingerprint, got.RemovedAt, want.fp, want.removed)
		}
		if got.RemovedAt.IsZero() {
			if o, ok := activeByFP[got.Fingerprint]; ok {
				return fmt.Sprintf("the key %s is held by %q and %q", got.Fingerprint, o, got.Name)
			}
			activeByFP[got.Fingerprint] = got.Name
		}
	}
	for n := range m.members {
		if !seen[n] {
			return fmt.Sprintf("member %q in the model, not in the file", n)
		}
	}
	for i := 0; i < 4; i++ {
		got, err := r.Holder(keys.fp[i])
		if want := m.holder(keys.fp[i]); got != want || (want == "") != (err != nil) {
			return fmt.Sprintf("Holder(key%d) = %q %v, the model says %q", i, got, err, want)
		}
	}
	if n, err := r.Holder(""); n != "" || err == nil {
		return fmt.Sprintf("Holder of no key: %q %v", n, err)
	}

	js, err := r.queryJoins(ctx, `ORDER BY id`)
	if err != nil {
		return "join tokens: " + err.Error()
	}
	live := map[int64]*sfzJoinRow{}
	for _, j := range m.joins {
		if !j.pruned {
			live[j.id] = j
		}
	}
	if len(js) != len(live) {
		return fmt.Sprintf("%d join tokens in the file, %d in the model", len(js), len(live))
	}
	for _, j := range js {
		w, ok := live[j.ID]
		if !ok || j.Member != w.name || j.ExpiresAt.UnixMilli() != w.expires || sfzMillis(j.UsedAt) != w.used || sfzMillis(j.RevokedAt) != w.revoked {
			return fmt.Sprintf("join token %+v, the model %+v", j, w)
		}
	}

	ts, err := r.queryTokens(ctx, `ORDER BY name`)
	if err != nil {
		return "API tokens: " + err.Error()
	}
	if len(ts) != len(m.tokens) {
		return fmt.Sprintf("%d API tokens in the file, %d in the model", len(ts), len(m.tokens))
	}
	for _, tk := range ts {
		w, ok := m.tokens[tk.Name]
		if !ok || tk.Role != w.role || sfzMillis(tk.ExpiresAt) != w.expires || sfzMillis(tk.RevokedAt) != w.revoked {
			return fmt.Sprintf("API token %+v, the model %+v", tk, w)
		}
	}

	bs, err := r.queryBindings(ctx)
	if err != nil {
		return "bindings: " + err.Error()
	}
	if len(bs) != len(m.bindings) {
		return fmt.Sprintf("%d bindings in the file, %d in the model", len(bs), len(m.bindings))
	}
	for _, b := range bs {
		if w, ok := m.bindings[b.Subject]; !ok || w.role != b.Role || w.origin != b.Origin {
			return fmt.Sprintf("binding %+v, the model %+v", b, w)
		}
	}
	for _, s := range sfzSubjects {
		want := Viewer
		if b, ok := m.bindings[s]; ok {
			want = b.role
		}
		if got := r.RoleOf(s); got != want {
			return fmt.Sprintf("RoleOf(%q) = %s, the model says %s", s, got, want)
		}
	}
	for _, s := range []string{"token:ci", "local", "", "gitlab:gitlab.example.com"} {
		if got := r.RoleOf(s); got != Viewer {
			return fmt.Sprintf("RoleOf(%q) = %s", s, got)
		}
	}
	return ""
}

func sfzMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// sfzOps encodes operations for the seeds: each is {op, a, b, c}.
func sfzOps(ops ...[4]byte) []byte {
	var out []byte
	for _, o := range ops {
		out = append(out, o[:]...)
	}
	return out
}

// Operation codes, as sfzApply reads them.
const (
	sfzAdd = iota
	sfzRemove
	sfzRemoveCluster
	sfzIssueJoin
	sfzJoin
	sfzRevokeJoin
	sfzGrant
	sfzRevoke
	sfzSync
	sfzIssueToken
	sfzRevokeToken
	sfzPrincipal
	sfzVoid
	sfzRefresh
	sfzPrune
	sfzAdvance
)

func sfzRegistrySeeds() [][]byte {
	// Actors: 0..2 people, 3 local, 4 token:ci. Names: 0 eu, 1 us, 2 ap-1, 3 new.
	// Join: a picks the token, b even = its own name, c%5 the key, (c>>3)%6 the secret variant.
	return [][]byte{
		// A join token issued, the name then registered by key and removed:
		// the token must not register the name with another key.
		sfzOps([4]byte{sfzIssueJoin, 0, 0, 0}, [4]byte{sfzAdd, 0, 0, 0}, [4]byte{sfzRemove, 0, 0, 0}, [4]byte{sfzJoin, 0, 0, 1}),
		// Joined, removed, the same token again with another key and the old one.
		sfzOps([4]byte{sfzIssueJoin, 0, 0, 0}, [4]byte{sfzJoin, 0, 0, 0}, [4]byte{sfzRemove, 0, 0, 0}, [4]byte{sfzJoin, 0, 0, 1}, [4]byte{sfzJoin, 0, 0, 0}),
		// A token only waits to join: removal revokes it (and says not found).
		sfzOps([4]byte{sfzIssueJoin, 0, 1, 0}, [4]byte{sfzRemoveCluster, 0, 1, 0}, [4]byte{sfzJoin, 0, 0, 2}),
		// Wrong secrets, a wrong name, another member's key, the key's other spelling.
		sfzOps([4]byte{sfzAdd, 0, 1, 2}, [4]byte{sfzIssueJoin, 0, 0, 0}, [4]byte{sfzJoin, 0, 0, 1 << 3}, [4]byte{sfzJoin, 0, 0, 2 << 3},
			[4]byte{sfzJoin, 0, 0, 3 << 3}, [4]byte{sfzJoin, 0, 3, 0}, [4]byte{sfzJoin, 0, 0, 2}, [4]byte{sfzAdd, 0, 2, 3}, [4]byte{sfzJoin, 0, 0, 3}),
		// An expired join token, then pruned.
		sfzOps([4]byte{sfzIssueJoin, 0, 0, 1}, [4]byte{sfzAdvance, 3, 0, 0}, [4]byte{sfzJoin, 0, 0, 0}, [4]byte{sfzPrune, 7, 0, 0}, [4]byte{sfzJoin, 0, 0, 0}),
		// A new token for a name revokes the unused one; RevokeJoin by a wrong name.
		sfzOps([4]byte{sfzIssueJoin, 0, 0, 0}, [4]byte{sfzIssueJoin, 0, 0, 3}, [4]byte{sfzJoin, 0, 0, 0}, [4]byte{sfzRevokeJoin, 1, 1, 0}, [4]byte{sfzRevokeJoin, 1, 0, 0}, [4]byte{sfzJoin, 1, 0, 0}),
		// The last owner stays; an owner from the values is locked.
		sfzOps([4]byte{sfzGrant, 3, 0, 2}, [4]byte{sfzRevoke, 0, 0, 0}, [4]byte{sfzGrant, 0, 0, 1}, [4]byte{sfzSync, 2, 0, 0}, [4]byte{sfzGrant, 0, 1, 1},
			[4]byte{sfzRevoke, 3, 1, 0}, [4]byte{sfzGrant, 0, 1, 0}, [4]byte{sfzRevoke, 0, 0, 0}, [4]byte{sfzSync, 0, 0, 0}, [4]byte{sfzRevoke, 1, 0, 0}),
		// Other spellings of a person.
		sfzOps([4]byte{sfzGrant, 3, 3, 2}, [4]byte{sfzGrant, 3, 4, 1}, [4]byte{sfzGrant, 3, 5, 1}, [4]byte{sfzGrant, 3, 6, 2}, [4]byte{sfzRevoke, 3, 3, 0}, [4]byte{sfzGrant, 3, 0, 3}),
		// A UI binding taken over by the values, then the values drop it.
		sfzOps([4]byte{sfzGrant, 3, 2, 1}, [4]byte{sfzSync, 4, 0, 0}, [4]byte{sfzGrant, 0, 2, 0}, [4]byte{sfzSync, 0, 0, 0}),
		// API tokens: expiry at the millisecond, revoke, issue again, old secret.
		sfzOps([4]byte{sfzIssueToken, 0, 0, 2 | 4<<2}, [4]byte{sfzPrincipal, 0, 0, 0}, [4]byte{sfzAdvance, 1, 0, 0}, [4]byte{sfzPrincipal, 0, 0, 0},
			[4]byte{sfzIssueToken, 0, 0, 1}, [4]byte{sfzRevokeToken, 0, 0, 0}, [4]byte{sfzIssueToken, 0, 0, 2}, [4]byte{sfzPrincipal, 0, 0, 0}, [4]byte{sfzPrincipal, 1, 0, 0},
			[4]byte{sfzPrincipal, 2, 1, 0}, [4]byte{sfzPrincipal, 2, 6, 0}, [4]byte{sfzPrincipal, 2, 7, 0}, [4]byte{sfzPrincipal, 2, 4, 0}),
		// The values' token name, a negative lifetime, an unknown role.
		sfzOps([4]byte{sfzIssueToken, 0, 2, 2}, [4]byte{sfzIssueToken, 0, 0, 2 | 3<<2}, [4]byte{sfzIssueToken, 0, 1, 3}),
		// VoidTokens ends every token; Prune keeps the audit honest.
		sfzOps([4]byte{sfzIssueToken, 0, 0, 2}, [4]byte{sfzIssueJoin, 0, 0, 0}, [4]byte{sfzVoid, 3, 0, 0}, [4]byte{sfzPrincipal, 0, 0, 0}, [4]byte{sfzJoin, 0, 0, 0},
			[4]byte{sfzPrune, 8, 0, 0}, [4]byte{sfzIssueToken, 0, 0, 2}, [4]byte{sfzPrincipal, 1, 0, 0}),
		// A removed member's key under another name, the removed record pruned.
		sfzOps([4]byte{sfzAdd, 0, 0, 0}, [4]byte{sfzRemove, 0, 0, 0}, [4]byte{sfzAdd, 0, 1, 3}, [4]byte{sfzAdd, 0, 0, 0}, [4]byte{sfzRemove, 0, 1, 0},
			[4]byte{sfzPrune, 7, 0, 0}, [4]byte{sfzAdd, 0, 0, 3}, [4]byte{sfzRefresh, 0, 0, 0}),
		// The name "new" and a non-key.
		sfzOps([4]byte{sfzAdd, 0, 3, 0}, [4]byte{sfzIssueJoin, 0, 3, 0}, [4]byte{sfzAdd, 0, 0, 4}, [4]byte{sfzIssueJoin, 0, 0, 4}, [4]byte{sfzIssueJoin, 0, 0, 5}, [4]byte{sfzJoin, 0, 0, 4}),
		// A token issued, the name added by key: Join with the token and a free key is refused.
		sfzOps([4]byte{sfzIssueJoin, 0, 0, 0}, [4]byte{sfzAdd, 0, 0, 0}, [4]byte{sfzJoin, 0, 0, 1}, [4]byte{sfzJoin, 0, 0, 0}, [4]byte{sfzRemove, 0, 0, 0}, [4]byte{sfzRemove, 0, 0, 0}),
	}
}

// ---- FuzzJoinSecret ----

// sfzJoinHub is a registry with one valid join token (for "eu") and
// tokens that must not work: a superseded one of "eu", a used one (its
// member "old-used" joined with key 1), a revoked one and an expired one;
// "prod-x" is registered by key 0.
type sfzJoinHub struct {
	r       *Registry
	valid   string
	pairs   [4][2]string // name, secret of the tokens that must not work
	secrets []string
}

func sfzNewJoinHub(t *testing.T, keys sfzKeySet) *sfzJoinHub {
	ctx := context.Background()
	r, err := OpenRegistry(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	r.Log = quiet
	now := time.UnixMilli(sfzEpoch).UTC()
	r.now = func() time.Time { return now }
	h := &sfzJoinHub{r: r}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = r.Add(ctx, "token:t", OriginAPI, "prod-x", keys.pem[0])
	must(err)
	_, used, err := r.IssueJoin(ctx, "x", "old-used", 0)
	must(err)
	_, err = r.Join(ctx, "old-used", used, keys.pem[1])
	must(err)
	rj, revoked, err := r.IssueJoin(ctx, "x", "old-rev", 0)
	must(err)
	must(r.RevokeJoin(ctx, "x", "old-rev", rj.ID))
	_, expired, err := r.IssueJoin(ctx, "x", "old-exp", time.Minute)
	must(err)
	now = now.Add(2 * time.Minute)
	_, superseded, err := r.IssueJoin(ctx, "x", "eu", 0)
	must(err)
	_, h.valid, err = r.IssueJoin(ctx, "x", "eu", 0)
	must(err)
	h.pairs = [4][2]string{{"old-used", used}, {"old-rev", revoked}, {"old-exp", expired}, {"eu", superseded}}
	h.secrets = []string{used, revoked, expired, superseded, h.valid}
	return h
}

// sfzSnapshot is the state a refused join must leave as it was.
func sfzSnapshot(t *testing.T, r *Registry, keys sfzKeySet) string {
	ctx := context.Background()
	var b strings.Builder
	ms, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		fmt.Fprintf(&b, "member %s %s %d\n", m.Name, m.Fingerprint, sfzMillis(m.RemovedAt))
	}
	js, err := r.queryJoins(ctx, `ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range js {
		fmt.Fprintf(&b, "join %d %s %d %d\n", j.ID, j.Member, sfzMillis(j.UsedAt), sfzMillis(j.RevokedAt))
	}
	for i := 0; i < 4; i++ {
		n, _ := r.Holder(keys.fp[i])
		fmt.Fprintf(&b, "holder %d %s\n", i, n)
	}
	fmt.Fprintf(&b, "audit %d\n", sfzCount(t, r.db, "audit"))
	return b.String()
}

// FuzzJoinSecret trades arbitrary names, secrets and public keys (or the
// real token's secret changed a little, the other tokens' secrets, the
// members' keys) against sfzJoinHub. Invariants:
//
//   - only the exact valid secret, for "eu", with exactly one acceptable
//     key that no member holds, registers a member (and then "eu" with
//     that key); a key already registered under the name is answered
//     with the member as it is;
//   - every other input leaves the members, the join tokens, the keys'
//     holders and the audit as they were;
//   - without a token issued for the name the answer is denied (or
//     invalid for a bad key), never "exists": it says nothing about who
//     holds a key;
//   - no error holds a token's secret, the join prefix, or a member's name
//     (except, to a caller holding a token for the name, the holder of
//     the key it sent);
//   - no panic, and an answer within 10 s.
func FuzzJoinSecret(f *testing.F) {
	// mode: bits 0-2 the secret, 3-4 the key, 5-6 the name (see below).
	const (
		secFuzz, secValid, secSuffix, secPrefix, secOneByte, secUpper, secOld, secSpaces = 0, 1, 2, 3, 4, 5, 6, 7
		keyFuzz, keyFree, keyHeld, keyFreePlus                                           = 0, 1 << 3, 2 << 3, 3 << 3
		nameFuzz, nameEU, nameProd, nameUsed                                             = 0, 1 << 5, 2 << 5, 3 << 5
	)
	f.Add(byte(secValid|keyFree|nameEU), "", "", "")
	f.Add(byte(secValid|keyHeld|nameEU), "", "", "")
	f.Add(byte(secValid|keyHeld|nameEU), "", "", "x")
	f.Add(byte(secValid|keyFree|nameProd), "", "", "")
	f.Add(byte(secValid|keyHeld|nameProd), "", "", "")
	f.Add(byte(secValid|keyFree|nameFuzz), "EU", "", "")
	f.Add(byte(secValid|keyFree|nameFuzz), "eu ", "", "")
	f.Add(byte(secValid|keyFree|nameFuzz), "eu\x00", "", "")
	f.Add(byte(secSuffix|keyFree|nameEU), "", "=", "")
	f.Add(byte(secSuffix|keyFree|nameEU), "", "\n", "")
	f.Add(byte(secPrefix|keyFree|nameEU), "", " ", "")
	f.Add(byte(secOneByte|keyFree|nameEU), "", "Q", "")
	f.Add(byte(secUpper|keyFree|nameEU), "", "", "")
	f.Add(byte(secSpaces|keyFree|nameEU), "", "", "")
	for i := 0; i < 4; i++ {
		f.Add(byte(secOld|keyFree|nameFuzz), "", strings.Repeat("x", i), "")
		f.Add(byte(secOld|keyHeld|nameFuzz), "", strings.Repeat("x", i), "x")
	}
	f.Add(byte(secOld|keyHeld|nameUsed), "", "", "x")
	f.Add(byte(secFuzz|keyFree|nameEU), "", "", "")
	f.Add(byte(secFuzz|keyFree|nameEU), "", JoinPrefix, "")
	f.Add(byte(secFuzz|keyFree|nameEU), "", "cia_pat_x", "")
	f.Add(byte(secFuzz|keyFuzz|nameFuzz), "secret-name", "' OR 1=1 --", "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEA\n-----END PUBLIC KEY-----\n")
	f.Add(byte(secValid|keyFreePlus|nameEU), "", "", "\n-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAGb9ECWmEzf6FQbrBZ9w7lshQhqowtrbLDFw4rXAxZuE=\n-----END PUBLIC KEY-----\n")
	f.Add(byte(secValid|keyFreePlus|nameEU), "", "", "\n-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")
	f.Add(byte(secValid|keyFuzz|nameEU), "", "", "")
	f.Add(byte(secValid|keyFuzz|nameEU), "", "", "-----BEGIN PUBLIC KEY-----\n\n-----END PUBLIC KEY-----\n")
	f.Fuzz(func(t *testing.T, mode byte, name, secret, pub string) {
		keys := sfzKeys(t)
		h := sfzNewJoinHub(t, keys)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		switch mode >> 5 & 3 {
		case 1:
			name = "eu"
		case 2:
			name = "prod-x"
		case 3:
			name = "old-used"
		}
		fuzzSecret := secret
		switch mode & 7 {
		case secValid:
			secret = h.valid
		case secSuffix:
			secret = h.valid + fuzzSecret
		case secPrefix:
			secret = fuzzSecret + h.valid
		case secOneByte:
			c := byte('A')
			if len(fuzzSecret) > 0 {
				c = fuzzSecret[0]
			}
			i := len(fuzzSecret) % len(h.valid)
			secret = h.valid[:i] + string(c) + h.valid[i+1:]
		case secUpper:
			secret = strings.ToUpper(h.valid)
		case secOld:
			p := h.pairs[len(fuzzSecret)%4]
			secret = p[1]
			if mode>>5&3 == 0 {
				name = p[0]
			}
		case secSpaces:
			secret = " " + h.valid + "\n"
		}
		fuzzPub := pub
		switch mode >> 3 & 3 {
		case 1:
			pub = sfzFree
		case 2:
			pub = keys.pem[len(fuzzPub)%2]
		case 3:
			pub = sfzFree + fuzzPub
		}

		// What the hub must answer.
		fp := ""
		if _, kfp, err := memberKey(pub); err == nil {
			fp = kfp
		}
		tokenFor := secret == h.valid && name == "eu"
		for _, p := range h.pairs {
			if secret == p[1] && name == p[0] {
				tokenFor = true
			}
		}
		holder := map[string]string{keys.fp[0]: "prod-x", keys.fp[1]: "old-used"}[fp]
		registers := fp != "" && secret == h.valid && name == "eu" && holder == ""

		before := sfzSnapshot(t, h.r, keys)
		m, err := h.r.Join(ctx, name, secret, pub)
		if ctx.Err() != nil {
			t.Fatalf("Join took more than 10 s")
		}
		after := sfzSnapshot(t, h.r, keys)
		desc := fmt.Sprintf("Join(%q, %q, %q)", name, secret, pub)
		switch {
		case registers:
			if err != nil || m.Name != "eu" || m.Fingerprint != fp {
				t.Fatalf("%s: %+v %v, want eu registered", desc, m, err)
			}
			if n, _ := h.r.Holder(fp); n != "eu" {
				t.Fatalf("%s: the key's holder is %q", desc, n)
			}
			if before == after {
				t.Fatalf("%s: registered and nothing changed", desc)
			}
		default:
			if before != after {
				t.Fatalf("%s: %v; the state changed:\n%s\n->\n%s", desc, err, before, after)
			}
			if err == nil && !(tokenFor && holder == name) {
				t.Fatalf("%s: %+v, accepted", desc, m)
			}
			if err == nil && m.Name != name {
				t.Fatalf("%s: answered with member %+v", desc, m)
			}
		}
		if !tokenFor && err != nil && !errors.Is(err, ErrDenied) && !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v, want denied or invalid without a token for the name", desc, err)
		}
		if fp == "" && !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v, want invalid for a key that is not one acceptable key", desc, err)
		}
		if err != nil {
			msg := err.Error()
			for _, s := range h.secrets {
				if strings.Contains(msg, s) {
					t.Fatalf("%s: the error holds a token's secret: %s", desc, msg)
				}
			}
			if strings.Contains(msg, JoinPrefix) {
				t.Fatalf("%s: the error holds the join prefix: %s", desc, msg)
			}
			for _, n := range []string{"prod-x", "old-used", "old-rev", "old-exp"} {
				if strings.Contains(msg, n) && !(tokenFor && holder == n) {
					t.Fatalf("%s: the error names %q: %s", desc, n, msg)
				}
			}
		}
	})
}

// ---- FuzzRestoreFile ----

// sfzCrafted* are token secrets that sfzRestoreEdits write into a backup
// (as hashes, the way a hub keeps them).
const (
	sfzCraftedAPI  = TokenPrefix + "crafted0crafted0crafted0crafted0crafted0cra"
	sfzCraftedJoin = JoinPrefix + "crafted0crafted0crafted0crafted0crafted0cra"
)

type sfzEdit struct {
	name string
	sql  []string
}

// sfzRecreate replaces a table by one with the columns given, keeping
// its rows.
func sfzRecreate(table, columns string) []string {
	return []string{
		`ALTER TABLE ` + table + ` RENAME TO sfz_old`,
		`CREATE TABLE ` + table + ` (` + columns + `)`,
		`INSERT INTO ` + table + ` SELECT * FROM sfz_old`,
		`DROP TABLE sfz_old`,
	}
}

const (
	sfzAPICols = `name TEXT PRIMARY KEY, role TEXT NOT NULL, hash TEXT NOT NULL UNIQUE, hint TEXT NOT NULL, created_at INTEGER NOT NULL,
		created_by TEXT NOT NULL, expires_at %s, used_at INTEGER NOT NULL DEFAULT 0, revoked_at %s`
	sfzJoinCols = `id INTEGER PRIMARY KEY, member TEXT NOT NULL, hash TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL,
		created_by TEXT NOT NULL, expires_at %s, used_at %s, revoked_at INTEGER NOT NULL DEFAULT 0`
)

func sfzInsertAPI(expires, revoked string) string {
	return fmt.Sprintf(`INSERT INTO api_tokens (name, role, hash, hint, created_at, created_by, expires_at, revoked_at) VALUES ('crafted', 'owner', '%s', 'xcra', 0, 'x', %s, %s)`,
		tokenHash(sfzCraftedAPI), expires, revoked)
}

func sfzInsertJoin(expires, used string) string {
	return fmt.Sprintf(`INSERT INTO join_tokens (member, hash, created_at, created_by, expires_at, used_at) VALUES ('zz', '%s', 0, 'x', %s, %s)`,
		joinHash(sfzCraftedJoin), expires, used)
}

// sfzRestoreEdits change a real backup the way a damaged or crafted one
// may differ: statements that would run inside the hub's (triggers, a
// view), tables of other shapes, tokens stored as values a hub never
// writes.
var sfzRestoreEdits = []sfzEdit{
	{"trigger on api_tokens", []string{`CREATE TABLE marker (x)`, `CREATE TRIGGER sfz_t AFTER UPDATE ON api_tokens BEGIN INSERT INTO marker VALUES (1); END`}},
	{"trigger on audit", []string{`CREATE TABLE marker (x)`, `CREATE TRIGGER sfz_a AFTER INSERT ON audit BEGIN INSERT INTO marker VALUES (2); END`}},
	{"trigger on members", []string{`CREATE TABLE marker (x)`, `CREATE TRIGGER sfz_m BEFORE INSERT ON members BEGIN INSERT INTO marker VALUES (3); END`}},
	{"trigger on join_tokens", []string{`CREATE TABLE marker (x)`, `CREATE TRIGGER sfz_j BEFORE UPDATE ON join_tokens BEGIN SELECT RAISE(IGNORE); END`}},
	{"view", []string{`CREATE VIEW sfz_v AS SELECT * FROM members`}},
	// Tokens stored as text in a column of TEXT affinity: a far expiry
	// that sorts before the current time as text, and "00".
	{"api expiry as text, far", append(sfzRecreate("api_tokens", fmt.Sprintf(sfzAPICols, "TEXT NOT NULL DEFAULT 0", "INTEGER NOT NULL DEFAULT 0")),
		sfzInsertAPI(`'100000000000000'`, `0`))},
	{"api expiry as text, 00", append(sfzRecreate("api_tokens", fmt.Sprintf(sfzAPICols, "TEXT NOT NULL DEFAULT 0", "INTEGER NOT NULL DEFAULT 0")),
		sfzInsertAPI(`'00'`, `0`))},
	{"api revoked as text", append(sfzRecreate("api_tokens", fmt.Sprintf(sfzAPICols, "INTEGER NOT NULL DEFAULT 0", "TEXT NOT NULL DEFAULT 0")),
		sfzInsertAPI(`0`, `'00'`))},
	{"join used as text", append(sfzRecreate("join_tokens", fmt.Sprintf(sfzJoinCols, "INTEGER NOT NULL", "TEXT NOT NULL DEFAULT 0")),
		sfzInsertJoin(`4102444800000`, `'00'`))},
	{"join expiry as text", append(sfzRecreate("join_tokens", fmt.Sprintf(sfzJoinCols, "TEXT NOT NULL", "INTEGER NOT NULL DEFAULT 0")),
		sfzInsertJoin(`'100000000000000'`, `0`))},
	// Values of no affinity, NULL, a fraction.
	{"api expiry untyped", append(sfzRecreate("api_tokens", fmt.Sprintf(sfzAPICols, "", "INTEGER NOT NULL DEFAULT 0")),
		sfzInsertAPI(`'0'`, `0`))},
	{"api expiry NULL", append(sfzRecreate("api_tokens", fmt.Sprintf(sfzAPICols, "INTEGER", "INTEGER NOT NULL DEFAULT 0")),
		sfzInsertAPI(`NULL`, `0`))},
	{"api expiry fraction", []string{sfzInsertAPI(`0.5`, `0`)}},
	// Plain crafted rows: a hub's own shapes.
	{"api crafted", []string{sfzInsertAPI(`0`, `0`)}},
	{"join crafted", []string{sfzInsertJoin(`4102444800000`, `0`)}},
	{"drop audit", []string{`DROP TABLE audit`}},
	{"drop api_tokens", []string{`DROP TABLE api_tokens`}},
	{"drop join_tokens", []string{`DROP TABLE join_tokens`}},
	{"api CHECK on revoked", sfzRecreate("api_tokens", fmt.Sprintf(sfzAPICols, "INTEGER NOT NULL DEFAULT 0", "INTEGER NOT NULL DEFAULT 0 CHECK (revoked_at = 0)"))},
	{"api revoked renamed", []string{`ALTER TABLE api_tokens RENAME COLUMN revoked_at TO gone`}},
	{"extra table", []string{`CREATE TABLE notes (b BLOB)`, `INSERT INTO notes VALUES (randomblob(3000))`}},
	{"expression index", []string{`CREATE INDEX sfz_ix ON members(lower(name))`}},
	{"generated column", []string{`ALTER TABLE members ADD COLUMN sfz_g TEXT GENERATED ALWAYS AS (upper(name)) VIRTUAL`}},
	{"user version", []string{`PRAGMA user_version = 99`}},
	{"WAL", []string{`PRAGMA journal_mode = WAL`}},
	{"no members", []string{`DELETE FROM members`}},
	{"marker only", []string{`CREATE TABLE marker (x)`}},
}

type sfzBackupFile struct {
	file              []byte
	apiSecret, joinSe string
}

var (
	sfzBackupOnce sync.Once
	sfzBackupVal  sfzBackupFile
	sfzBackupErr  error
)

// sfzBackup is a real backup made by Backup, once per process: a member,
// an owner, an API token that never expires and a join token for "eu"
// valid for a week (at the real time: staging voids at the real time).
func sfzBackup(t testing.TB) sfzBackupFile {
	keys := sfzKeys(t)
	sfzBackupOnce.Do(func() {
		dir, err := os.MkdirTemp("", "sfz-backup-")
		if err != nil {
			sfzBackupErr = err
			return
		}
		defer os.RemoveAll(dir)
		ctx := context.Background()
		r, err := OpenRegistry(ctx, dir)
		if err != nil {
			sfzBackupErr = err
			return
		}
		defer r.Close()
		r.Log = quiet
		steps := []func() error{
			func() error { _, err := r.Add(ctx, "x", OriginAPI, "prod-x", keys.pem[0]); return err },
			func() error {
				_, err := r.Grant(ctx, OriginLocal, OriginLocal, sfzPeople[0], Owner)
				return err
			},
			func() error {
				_, s, err := r.IssueToken(ctx, "x", "ci", Owner, 0)
				sfzBackupVal.apiSecret = s
				return err
			},
			func() error {
				_, s, err := r.IssueJoin(ctx, "x", "eu", MaxJoinLifetime)
				sfzBackupVal.joinSe = s
				return err
			},
			func() error { return r.Backup(ctx, filepath.Join(dir, "hub.db")) },
		}
		for _, s := range steps {
			if err := s(); err != nil {
				sfzBackupErr = err
				return
			}
		}
		sfzBackupVal.file, sfzBackupErr = os.ReadFile(filepath.Join(dir, "hub.db"))
	})
	if sfzBackupErr != nil {
		t.Fatal(sfzBackupErr)
	}
	return sfzBackupVal
}

// sfzOracle reads a built file on its own (not with the code under
// test): whether it holds a trigger or a view, and the hub's two tables.
func sfzOracle(t *testing.T, file string) (runs bool, tables bool, ok bool) {
	db, err := sql.Open("sqlite", sqliteURI(file, map[string][]string{"mode": {"ro"}}))
	if err != nil {
		return false, false, false
	}
	defer db.Close()
	var n, m int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type NOT IN ('table', 'index')`).Scan(&n); err != nil {
		return false, false, false
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name IN ('members', 'audit')`).Scan(&m); err != nil {
		return false, false, false
	}
	return n > 0, m == 2, true
}

// sfzMarker is how many rows a table named marker holds in file (0 when
// there is none or the file cannot be read).
func sfzMarker(file string) int {
	db, err := sql.Open("sqlite", sqliteURI(file, map[string][]string{"mode": {"ro"}}))
	if err != nil {
		return 0
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM marker`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// FuzzRestoreFile stages arbitrary files and mutations of a real backup.
// mode%4: 0 the bytes as the file; 1 the backup with bytes replaced
// (3 input bytes each: a 16-bit offset, a value); 2 the backup changed by
// sfzRestoreEdits (one input byte each, at most 6); 3 both. Invariants:
//
//   - no panic, an answer within 20 s;
//   - a file that is no SQLite database, or one without the hub's members
//     and audit tables, or one holding a trigger or a view, is refused,
//     and a refused file stages nothing;
//   - staging never changes the file given;
//   - what is staged holds no trigger and no view, passes the integrity
//     check and has the hub's tables;
//   - no trigger fires (the marker table stays empty in the file given,
//     the staged copy and the restored hub);
//   - in the restored hub, opened as the hub opens it, before and after
//     its own VoidTokens: no API token of the backup (the real one or one
//     written into it) yields a principal, and no join token of it
//     registers a member.
func FuzzRestoreFile(f *testing.F) {
	idx := func(name string) byte {
		for i, e := range sfzRestoreEdits {
			if e.name == name {
				return byte(i)
			}
		}
		panic(name)
	}
	f.Add(byte(2), []byte{}, []byte{})
	for i := range sfzRestoreEdits {
		f.Add(byte(2), []byte{byte(i)}, []byte{})
	}
	// Known-bad shapes: tokens a hub's VoidTokens does not reach.
	f.Add(byte(2), []byte{idx("api expiry as text, far")}, []byte{})
	f.Add(byte(2), []byte{idx("join used as text")}, []byte{})
	f.Add(byte(2), []byte{idx("drop api_tokens"), idx("api crafted")}, []byte{})
	f.Add(byte(2), []byte{idx("api revoked renamed"), idx("api crafted")}, []byte{})
	f.Add(byte(2), []byte{idx("WAL"), idx("trigger on audit")}, []byte{})
	f.Add(byte(0), []byte{}, []byte("not a database"))
	f.Add(byte(0), []byte{}, []byte{})
	f.Add(byte(0), []byte{}, append([]byte("SQLite format 3\x00"), make([]byte, 84)...))
	f.Add(byte(1), []byte{}, []byte{0, 16, 0xff, 0, 17, 0xff})                  // the page size
	f.Add(byte(1), []byte{}, []byte{0, 28, 0x7f, 0, 100, 0x0d})                 // the page count, the first page's type
	f.Add(byte(1), []byte{}, []byte{0x10, 0x01, 0x00, 0x10, 0x02, 0x00})        // inside page 2
	f.Add(byte(3), []byte{idx("trigger on members")}, []byte{0x00, 0x70, 0x41}) // a trigger, the header changed
	f.Fuzz(func(t *testing.T, mode byte, edits []byte, raw []byte) {
		bk := sfzBackup(t)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		work := t.TempDir()
		file := filepath.Join(work, "hub.db")
		var desc []string
		exact := false
		switch mode % 4 {
		case 0:
			if err := os.WriteFile(file, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			desc = append(desc, fmt.Sprintf("raw %d bytes", len(raw)))
		default:
			if err := os.WriteFile(file, bk.file, 0o600); err != nil {
				t.Fatal(err)
			}
			if mode%4 >= 2 {
				db, err := sql.Open("sqlite", file)
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				for i, e := range edits {
					if i == 6 {
						break
					}
					ed := sfzRestoreEdits[int(e)%len(sfzRestoreEdits)]
					okAll := true
					for _, q := range ed.sql {
						if _, err := db.ExecContext(ctx, q); err != nil {
							okAll = false
							break
						}
					}
					desc = append(desc, fmt.Sprintf("edit %q (%v)", ed.name, okAll))
				}
				// Back to a single file: a WAL edit checkpoints here.
				db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				os.Remove(file + "-wal")
				os.Remove(file + "-shm")
				exact = true
			}
			if mode%4 == 1 || mode%4 == 3 {
				b, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i+2 < len(raw) && i < 3*8; i += 3 {
					off := (int(raw[i])<<8 | int(raw[i+1])) % len(b)
					b[off] = raw[i+2]
					desc = append(desc, fmt.Sprintf("byte %d = %#x", off, raw[i+2]))
				}
				if err := os.WriteFile(file, b, 0o600); err != nil {
					t.Fatal(err)
				}
				exact = exact && len(raw) < 3
			}
		}
		given, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		runs, tables, readable := false, false, false
		if exact {
			runs, tables, readable = sfzOracle(t, file)
		}
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("%s:\n"+format, append([]any{strings.Join(desc, "; ")}, args...)...)
		}

		dir := t.TempDir()
		err = StageRestore(ctx, dir, file)
		if ctx.Err() != nil {
			fail("StageRestore took more than 20 s")
		}
		if now, rerr := os.ReadFile(file); rerr != nil || !bytes.Equal(now, given) {
			fail("staging changed the file given (%v)", rerr)
		}
		if sfzMarker(file) != 0 {
			fail("a trigger fired in the file given")
		}
		staged := filepath.Join(dir, restoreName)
		if err != nil {
			if _, serr := os.Stat(staged); serr == nil {
				fail("refused (%v) and staged all the same", err)
			}
			return
		}
		switch {
		case mode%4 == 0 && !bytes.HasPrefix(raw, []byte("SQLite format 3\x00")):
			fail("a file that is no SQLite database was staged")
		case exact && readable && runs:
			fail("a backup holding a trigger or a view was staged")
		case exact && readable && !tables:
			fail("a backup without the hub's tables was staged")
		}
		if runs2, tables2, ok2 := sfzOracle(t, staged); !ok2 || runs2 || !tables2 {
			fail("the staged copy: readable %v, holds a trigger or view %v, the hub's tables %v", ok2, runs2, tables2)
		}
		if sfzMarker(staged) != 0 {
			fail("a trigger fired in the staged copy")
		}
		{
			db, err := sql.Open("sqlite", sqliteURI(staged, map[string][]string{"mode": {"ro"}}))
			if err != nil {
				fail("opening the staged copy: %v", err)
			}
			var ok string
			if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&ok); err != nil || ok != "ok" {
				db.Close()
				fail("the staged copy is damaged: %v %s", err, ok)
			}
			db.Close()
		}

		// The hub's start: apply, open, look (a hub stopped before its
		// own void), void, look again.
		if _, applied, err := ApplyRestore(dir, time.Now()); err != nil || !applied {
			fail("apply: %v %v", applied, err)
		}
		r, err := OpenRegistry(ctx, dir)
		if err != nil {
			// The hub does not start on this file; nothing of it is used.
			return
		}
		defer r.Close()
		r.Log = quiet
		look := func(when string) {
			t.Helper()
			for _, s := range []string{bk.apiSecret, sfzCraftedAPI} {
				if p, ok := r.TokenPrincipal(ctx, s); ok {
					fail("%s: a backup's API token opens: %+v", when, p)
				}
			}
			for _, j := range [][2]string{{"eu", bk.joinSe}, {"zz", sfzCraftedJoin}} {
				if m, err := r.Join(ctx, j[0], j[1], sfzFree); err == nil {
					fail("%s: a backup's join token registered %+v", when, m)
				}
			}
			if sfzMarker(filepath.Join(dir, "fleet.db")) != 0 {
				fail("%s: a trigger fired in the restored hub", when)
			}
		}
		look("restored, before the hub's VoidTokens")
		if _, err := r.VoidTokens(ctx, OriginLocal, "a backup was restored"); err != nil {
			// The hub stops here: it does not start on this file.
			return
		}
		look("after the hub's VoidTokens")
	})
}
