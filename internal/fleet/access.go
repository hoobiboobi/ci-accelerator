package fleet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Who may do what at a hub. Roles live in fleet.db as bindings of a
// subject to a role; every action is checked by the name of a permission,
// so another role later is a row of the roles table below, not a sweep.
//
// Subjects: "gitlab:<host>/<user id>" for a person (ids, never usernames:
// GitLab gives a freed username to another account), "token:<name>" for an
// API token, "local" for a command run inside the hub's pod.

// Role is a role at the hub.
type Role string

const (
	// Viewer is everyone signed in without a binding: what GitLab lets
	// them read, cluster by cluster.
	Viewer Role = "viewer"
	// Operator manages clusters: members, join tokens, their settings; reads
	// the audit.
	Operator Role = "operator"
	// Owner may do everything: also roles and API tokens.
	Owner Role = "owner"
)

// Permissions by name.
const (
	PermMembersRead  = "members.read"
	PermMembersWrite = "members.write"
	PermAuditRead    = "audit.read"
	PermRolesRead    = "roles.read"
	PermRolesWrite   = "roles.write"
	PermTokensWrite  = "tokens.write"
	// PermAllProjects: sees every project of every cluster, as an admin of
	// a box does.
	PermAllProjects = "projects.all"
)

var permissions = map[Role]map[string]bool{
	Viewer: {},
	Operator: {PermMembersRead: true, PermMembersWrite: true, PermAuditRead: true, PermRolesRead: true,
		PermAllProjects: true},
	Owner: {PermMembersRead: true, PermMembersWrite: true, PermAuditRead: true, PermRolesRead: true,
		PermRolesWrite: true, PermTokensWrite: true, PermAllProjects: true},
}

// Roles lists the roles, least first.
func Roles() []Role { return []Role{Viewer, Operator, Owner} }

// ParseRole checks a role's name.
func ParseRole(s string) (Role, error) {
	if _, ok := permissions[Role(s)]; ok {
		return Role(s), nil
	}
	return "", fmt.Errorf("%w: role %q is none of viewer, operator, owner", ErrInvalid, s)
}

// Allows says whether a role has a permission.
func (r Role) Allows(perm string) bool { return permissions[r][perm] }

// Principal is who a request comes from, as the hub's roles see it.
type Principal struct {
	// Subject: "gitlab:<host>/<id>", "token:<name>", "local"; "" for
	// nobody (no sign-in and no token).
	Subject string
	Role    Role
}

// Can says whether the principal has a permission.
func (p Principal) Can(perm string) bool { return p.Subject != "" && p.Role.Allows(perm) }

// Origins of a binding or a token.
const (
	OriginValues = "values" // the hub's Helm values: locked here
	OriginUI     = "ui"
	OriginAPI    = "api"
	OriginLocal  = "local" // a command inside the hub's pod
)

// ErrLocked: the binding comes from the values and changes only there.
var ErrLocked = errors.New("locked")

const accessSchema = `
CREATE TABLE IF NOT EXISTS role_bindings (
	subject  TEXT PRIMARY KEY,
	role     TEXT NOT NULL,
	origin   TEXT NOT NULL,
	label    TEXT NOT NULL DEFAULT '',
	added_at INTEGER NOT NULL,
	added_by TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS api_tokens (
	name       TEXT PRIMARY KEY,
	role       TEXT NOT NULL,
	hash       TEXT NOT NULL UNIQUE,
	hint       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	created_by TEXT NOT NULL,
	expires_at INTEGER NOT NULL DEFAULT 0,
	used_at    INTEGER NOT NULL DEFAULT 0,
	revoked_at INTEGER NOT NULL DEFAULT 0
);
`

// PersonSubject is a person's subject.
func PersonSubject(host string, userID uint64) string {
	return "gitlab:" + strings.ToLower(host) + "/" + strconv.FormatUint(userID, 10)
}

var personSubject = regexp.MustCompile(`^gitlab:([a-z0-9.-]+(:[0-9]+)?)/([1-9][0-9]{0,19})$`)

// ParsePerson checks a person's subject: "gitlab:<host>/<id>", exactly as
// PersonSubject writes it (no white space, a lower-case host, no leading
// zeros), the only form a signed-in person's subject is compared with.
func ParsePerson(s string) (host string, id uint64, err error) {
	m := personSubject.FindStringSubmatch(s)
	if m == nil {
		return "", 0, fmt.Errorf("%w: %q is not gitlab:<host>/<user id> (the number on the person's GitLab profile page)", ErrInvalid, s)
	}
	id, err = strconv.ParseUint(m[3], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %q: %v", ErrInvalid, s, err)
	}
	return m[1], id, nil
}

// Binding is a subject's role.
type Binding struct {
	Subject string    `json:"subject"`
	Role    Role      `json:"role"`
	Origin  string    `json:"origin"`
	Label   string    `json:"label,omitempty"`
	AddedAt time.Time `json:"added_at"`
	AddedBy string    `json:"added_by"`
}

// RoleOf is a subject's role: Viewer when it has no binding.
func (r *Registry) RoleOf(subject string) Role {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.roles[subject]; ok {
		return b.Role
	}
	return Viewer
}

// Label is what the pages call a subject: the person's name as last seen,
// else the subject itself.
func (r *Registry) Label(subject string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.roles[subject]; ok && b.Label != "" {
		return b.Label
	}
	return subject
}

func (r *Registry) reloadRoles(ctx context.Context) error {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	bs, err := r.queryBindings(ctx)
	if err != nil {
		return err
	}
	m := make(map[string]Binding, len(bs))
	for _, b := range bs {
		m[b.Subject] = b
	}
	r.mu.Lock()
	r.roles = m
	r.mu.Unlock()
	return nil
}

// Bindings is every binding, owners first.
func (r *Registry) Bindings(ctx context.Context) ([]Binding, error) {
	bs, err := r.queryBindings(ctx)
	if err != nil {
		return nil, err
	}
	rank := map[Role]int{Owner: 0, Operator: 1, Viewer: 2}
	sort.SliceStable(bs, func(i, j int) bool {
		if rank[bs[i].Role] != rank[bs[j].Role] {
			return rank[bs[i].Role] < rank[bs[j].Role]
		}
		return bs[i].Subject < bs[j].Subject
	})
	return bs, nil
}

func (r *Registry) queryBindings(ctx context.Context) ([]Binding, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT subject, role, origin, label, added_at, added_by FROM role_bindings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Binding{}
	for rows.Next() {
		var b Binding
		var at int64
		if err := rows.Scan(&b.Subject, &b.Role, &b.Origin, &b.Label, &at, &b.AddedBy); err != nil {
			return nil, err
		}
		b.AddedAt = time.UnixMilli(at).UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

// Grant gives a person a role (a viewer binding is no binding: Grant
// with Viewer takes the binding away). A binding from the values is
// locked, and so is the only owner's (see lastOwner).
func (r *Registry) Grant(ctx context.Context, actor, origin, subject string, role Role) (Binding, error) {
	host, id, err := ParsePerson(subject)
	if err != nil {
		return Binding{}, err
	}
	subject = PersonSubject(host, id)
	if _, err := ParseRole(string(role)); err != nil {
		return Binding{}, err
	}
	if role == Viewer {
		return Binding{Subject: subject, Role: Viewer}, r.Revoke(ctx, actor, subject)
	}
	now := r.now()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Binding{}, err
	}
	defer tx.Rollback()
	var was Role
	var wasOrigin, label, wasBy string
	var wasAt int64
	switch err := tx.QueryRowContext(ctx, `SELECT role, origin, label, added_at, added_by FROM role_bindings WHERE subject = ?`, subject).
		Scan(&was, &wasOrigin, &label, &wasAt, &wasBy); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return Binding{}, err
	case wasOrigin == OriginValues:
		return Binding{}, fmt.Errorf("%w: %s is an owner from the hub's values; change it there", ErrLocked, subject)
	case was == role:
		// As the file has it: the binding may come from another process
		// (hub grant) that the maps have not read yet.
		return Binding{Subject: subject, Role: was, Origin: wasOrigin, Label: label, AddedAt: time.UnixMilli(wasAt).UTC(), AddedBy: wasBy}, nil
	}
	if err := lastOwner(ctx, tx, actor, subject, was); err != nil {
		return Binding{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO role_bindings (subject, role, origin, label, added_at, added_by) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(subject) DO UPDATE SET role = excluded.role, origin = excluded.origin, added_at = excluded.added_at, added_by = excluded.added_by`,
		subject, role, origin, label, now.UnixMilli(), actor); err != nil {
		return Binding{}, err
	}
	detail := "role " + string(role)
	if was != "" {
		detail = "role " + string(was) + " -> " + string(role)
	}
	if err := audit(ctx, tx, now, actor, "role.grant", subject, detail+", origin "+origin); err != nil {
		return Binding{}, err
	}
	if err := tx.Commit(); err != nil {
		return Binding{}, err
	}
	r.committed(ctx, r.reloadRoles)
	return Binding{Subject: subject, Role: role, Origin: origin, Label: label, AddedAt: time.UnixMilli(now.UnixMilli()).UTC(), AddedBy: actor}, nil
}

// lastOwner refuses to take the owner role from the hub's only owner: no
// one could manage roles and tokens then. A command inside the hub's pod
// (hub grant, the way back when no owner is left) may.
func lastOwner(ctx context.Context, tx *sql.Tx, actor, subject string, was Role) error {
	if was != Owner || actor == OriginLocal {
		return nil
	}
	var others int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM role_bindings WHERE role = ? AND subject != ?`, Owner, subject).Scan(&others); err != nil {
		return err
	}
	if others == 0 {
		return fmt.Errorf("%w: %s is the hub's only owner; make someone else an owner first", ErrLocked, subject)
	}
	return nil
}

func (r *Registry) binding(ctx context.Context, subject string) (Binding, error) {
	r.mu.Lock()
	b, ok := r.roles[subject]
	r.mu.Unlock()
	if !ok {
		return Binding{}, ErrNotFound
	}
	return b, nil
}

// Revoke takes a person's binding away (they are a viewer then). A binding
// from the values is locked; none is no error.
func (r *Registry) Revoke(ctx context.Context, actor, subject string) error {
	now := r.now()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var role Role
	var origin string
	switch err := tx.QueryRowContext(ctx, `SELECT role, origin FROM role_bindings WHERE subject = ?`, subject).Scan(&role, &origin); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	case origin == OriginValues:
		return fmt.Errorf("%w: %s is an owner from the hub's values; change it there", ErrLocked, subject)
	}
	if err := lastOwner(ctx, tx, actor, subject, role); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM role_bindings WHERE subject = ?`, subject); err != nil {
		return err
	}
	if err := audit(ctx, tx, now, actor, "role.revoke", subject, "role "+string(role)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.committed(ctx, r.reloadRoles)
	return nil
}

// SyncOwners makes the owners from the values (subjects) the bindings of
// origin values: new ones added, ones no longer there removed, a person
// bound in the UI before becoming an owner in the values taken over.
func (r *Registry) SyncOwners(ctx context.Context, subjects []string) error {
	want := map[string]bool{}
	for _, s := range subjects {
		if _, _, err := ParsePerson(s); err != nil {
			return err
		}
		want[s] = true
	}
	now := r.now()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT subject FROM role_bindings WHERE origin = ?`, OriginValues)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return err
		}
		have[s] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for s := range have {
		if want[s] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM role_bindings WHERE subject = ?`, s); err != nil {
			return err
		}
		if err := audit(ctx, tx, now, OriginValues, "role.revoke", s, "role owner, no longer in the values"); err != nil {
			return err
		}
	}
	for s := range want {
		if have[s] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO role_bindings (subject, role, origin, added_at, added_by) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(subject) DO UPDATE SET role = excluded.role, origin = excluded.origin, added_at = excluded.added_at, added_by = excluded.added_by`,
			s, Owner, OriginValues, now.UnixMilli(), OriginValues); err != nil {
			return err
		}
		if err := audit(ctx, tx, now, OriginValues, "role.grant", s, "role owner, origin values"); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.committed(ctx, r.reloadRoles)
	return nil
}

// NoteName records the name a bound person goes by now (shown on the pages and
// in the audit instead of the subject); a person without a binding is
// not recorded.
func (r *Registry) NoteName(ctx context.Context, subject, label string) error {
	r.mu.Lock()
	b, ok := r.roles[subject]
	r.mu.Unlock()
	if !ok || b.Label == label || label == "" {
		return nil
	}
	if _, err := r.db.ExecContext(ctx, `UPDATE role_bindings SET label = ? WHERE subject = ?`, label, subject); err != nil {
		return err
	}
	r.mu.Lock()
	if b, ok := r.roles[subject]; ok {
		b.Label = label
		r.roles[subject] = b
	}
	r.mu.Unlock()
	return nil
}

// ---- API tokens ----

// TokenPrefix starts every API token of a hub: secret scanners find it.
const TokenPrefix = "cia_pat_"

// Token is an API token as the hub keeps it: never its secret.
type Token struct {
	Name      string    `json:"name"`
	Role      Role      `json:"role"`
	Hint      string    `json:"hint"` // the secret's last characters
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
	// ExpiresAt zero: never.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	UsedAt    time.Time `json:"used_at,omitzero"`
	RevokedAt time.Time `json:"revoked_at,omitzero"`
}

// Active says whether the token opens anything at t.
func (t Token) Active(at time.Time) bool {
	return t.RevokedAt.IsZero() && (t.ExpiresAt.IsZero() || at.Before(t.ExpiresAt))
}

// TokenNamePattern is what a token's name may look like.
var TokenNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`)

// usedEvery bounds how often a token's last use is written.
const usedEvery = time.Minute

// IssueToken makes an API token; its secret is returned once and kept
// only as a hash. ttl 0: it never expires. A name of a revoked or expired
// token is taken again.
func (r *Registry) IssueToken(ctx context.Context, actor, name string, role Role, ttl time.Duration) (Token, string, error) {
	if !TokenNamePattern.MatchString(name) || name == "bootstrap" {
		return Token{}, "", fmt.Errorf("%w: a token's name is lower-case letters, digits, dots, dashes and underscores, at most 64; \"bootstrap\" is the values' token", ErrInvalid)
	}
	if _, err := ParseRole(string(role)); err != nil {
		return Token{}, "", err
	}
	if ttl < 0 {
		return Token{}, "", fmt.Errorf("%w: a token's lifetime is positive, or 0 for never", ErrInvalid)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return Token{}, "", err
	}
	secret := TokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	now := r.now()
	var expires int64
	if ttl > 0 {
		expires = now.Add(ttl).UnixMilli()
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Token{}, "", err
	}
	defer tx.Rollback()
	var revoked, exp int64
	switch err := tx.QueryRowContext(ctx, `SELECT revoked_at, expires_at FROM api_tokens WHERE name = ?`, name).Scan(&revoked, &exp); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return Token{}, "", err
	case revoked == 0 && (exp == 0 || exp > now.UnixMilli()):
		return Token{}, "", fmt.Errorf("%w: a token named %q is active; revoke it first", ErrExists, name)
	default:
		if _, err := tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE name = ?`, name); err != nil {
			return Token{}, "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO api_tokens (name, role, hash, hint, created_at, created_by, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, role, tokenHash(secret), secret[len(secret)-4:], now.UnixMilli(), actor, expires); err != nil {
		return Token{}, "", err
	}
	detail := "role " + string(role) + ", never expires"
	if ttl > 0 {
		detail = "role " + string(role) + ", expires " + now.Add(ttl).UTC().Format("2006-01-02")
	}
	if err := audit(ctx, tx, now, actor, "token.issue", "token:"+name, detail); err != nil {
		return Token{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return Token{}, "", err
	}
	t, err := r.token(ctx, name)
	return t, secret, err
}

func tokenHash(secret string) string {
	sum := sha256.Sum256([]byte("ci-accelerator api token\n" + secret))
	return hex.EncodeToString(sum[:])
}

// Tokens is every API token, active ones first, newest first.
func (r *Registry) Tokens(ctx context.Context) ([]Token, error) {
	ts, err := r.queryTokens(ctx, `ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	now := r.now()
	sort.SliceStable(ts, func(i, j int) bool { return ts[i].Active(now) && !ts[j].Active(now) })
	return ts, nil
}

func (r *Registry) token(ctx context.Context, name string) (Token, error) {
	ts, err := r.queryTokens(ctx, `WHERE name = ?`, name)
	if err != nil {
		return Token{}, err
	}
	if len(ts) == 0 {
		return Token{}, ErrNotFound
	}
	return ts[0], nil
}

func (r *Registry) queryTokens(ctx context.Context, where string, args ...any) ([]Token, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT name, role, hint, created_at, created_by, expires_at, used_at, revoked_at FROM api_tokens `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		var t Token
		var created, expires, used, revoked int64
		if err := rows.Scan(&t.Name, &t.Role, &t.Hint, &created, &t.CreatedBy, &expires, &used, &revoked); err != nil {
			return nil, err
		}
		t.CreatedAt, t.ExpiresAt, t.UsedAt, t.RevokedAt = millis(created), millis(expires), millis(used), millis(revoked)
		out = append(out, t)
	}
	return out, rows.Err()
}

func millis(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

// RevokeToken ends an API token at once; an unknown or ended one is no
// error.
func (r *Registry) RevokeToken(ctx context.Context, actor, name string) error {
	now := r.now()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE name = ? AND revoked_at = 0`, now.UnixMilli(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if err := audit(ctx, tx, now, actor, "token.revoke", "token:"+name, ""); err != nil {
		return err
	}
	return tx.Commit()
}

// TokenPrincipal is the principal of an API token's secret; false when no
// active token has it, or when the file cannot be read (logged: the caller
// answers as for an unknown token).
func (r *Registry) TokenPrincipal(ctx context.Context, secret string) (Principal, bool) {
	if !strings.HasPrefix(secret, TokenPrefix) {
		return Principal{}, false
	}
	var name string
	var role Role
	var expires, used int64
	// Only integers count: a restored file may hold other values (text
	// '00', a far date as text) that SQL and Go read differently.
	err := r.db.QueryRowContext(ctx, `SELECT name, role, expires_at, CASE typeof(used_at) WHEN 'integer' THEN used_at ELSE 0 END FROM api_tokens
		WHERE hash = ? AND revoked_at = 0 AND typeof(revoked_at) = 'integer' AND typeof(expires_at) = 'integer'`, tokenHash(secret)).
		Scan(&name, &role, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, false
	}
	if err != nil {
		if ctx.Err() == nil {
			r.log().Error("fleet: reading an API token", "err", err)
		}
		return Principal{}, false
	}
	now := r.now()
	if expires != 0 && now.UnixMilli() >= expires {
		return Principal{}, false
	}
	if now.Sub(millis(used)) > usedEvery {
		// Best effort: a token that reads keeps working when the write fails.
		if _, err := r.db.ExecContext(context.WithoutCancel(ctx), `UPDATE api_tokens SET used_at = ? WHERE name = ?`, now.UnixMilli(), name); err != nil {
			r.log().Warn("fleet: recording an API token's use", "token", name, "err", err)
		}
	}
	return Principal{Subject: "token:" + name, Role: role}, true
}

// VoidTokens revokes every API and join token (after a restore: a backup brings
// back tokens revoked since it was taken); it returns how many were
// active.
func (r *Registry) VoidTokens(ctx context.Context, actor, why string) (int, error) {
	now := r.now()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// A row whose time columns are not integers (a restored file can hold
	// any value) is voided too, whatever SQL makes of its values.
	res, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE (revoked_at = 0 AND (expires_at = 0 OR expires_at > ?))
		OR typeof(revoked_at) != 'integer' OR typeof(expires_at) != 'integer'`, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	res, err = tx.ExecContext(ctx, `UPDATE join_tokens SET revoked_at = ? WHERE (used_at = 0 AND revoked_at = 0 AND expires_at > ?)
		OR typeof(used_at) != 'integer' OR typeof(revoked_at) != 'integer' OR typeof(expires_at) != 'integer'`, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return 0, err
	}
	nj, _ := res.RowsAffected()
	if err := audit(ctx, tx, now, actor, "token.void", "hub", fmt.Sprintf("%d API tokens and %d join tokens voided: %s", n, nj, why)); err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}
