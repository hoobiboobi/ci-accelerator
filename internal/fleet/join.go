package fleet

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Join tokens: how people add a cluster without handling its key. An
// operator issues one for a member name; the box, installed with it,
// makes its own key, keeps it in a Secret of its namespace and trades the
// token for its registration (POST JoinPath). A token is kept as a hash,
// bound to one name, works once and lives an hour by default, at most a
// week. Issuing another for the same name revokes the unused ones.

// JoinPath is where a box trades its join token, on the hub's console
// address (next to ConnectPath).
const JoinPath = "/fleet/v1/join"

// JoinPrefix starts every join token: secret scanners find it.
const JoinPrefix = "cia_join_"

// Join token lifetimes.
const (
	DefaultJoinLifetime = time.Hour
	MaxJoinLifetime     = 7 * 24 * time.Hour
)

// ErrDenied: a join token that is unknown, used, revoked, expired or for
// another name.
var ErrDenied = errors.New("denied")

const joinSchema = `
CREATE TABLE IF NOT EXISTS join_tokens (
	id         INTEGER PRIMARY KEY,
	member     TEXT NOT NULL,
	hash       TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	created_by TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at    INTEGER NOT NULL DEFAULT 0,
	revoked_at INTEGER NOT NULL DEFAULT 0
);
`

// JoinToken is a join token as the hub keeps it: never its secret.
type JoinToken struct {
	ID        int64     `json:"id"`
	Member    string    `json:"member"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
	ExpiresAt time.Time `json:"expires_at"`
	UsedAt    time.Time `json:"used_at,omitzero"`
	RevokedAt time.Time `json:"revoked_at,omitzero"`
}

// Active says whether the token can still be traded at t.
func (j JoinToken) Active(at time.Time) bool {
	return j.UsedAt.IsZero() && j.RevokedAt.IsZero() && at.Before(j.ExpiresAt)
}

func joinHash(secret string) string { return tokenHash("join\n" + secret) }

// IssueJoin makes a join token for a member name not registered now; ttl
// 0: an hour. Its secret is returned once.
func (r *Registry) IssueJoin(ctx context.Context, actor, name string, ttl time.Duration) (JoinToken, string, error) {
	if err := newName(name); err != nil {
		return JoinToken{}, "", err
	}
	if ttl == 0 {
		ttl = DefaultJoinLifetime
	}
	if ttl < time.Minute || ttl > MaxJoinLifetime {
		return JoinToken{}, "", fmt.Errorf("%w: a join token lives a minute to 7 days", ErrInvalid)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return JoinToken{}, "", err
	}
	secret := JoinPrefix + base64.RawURLEncoding.EncodeToString(buf)
	now := r.now()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return JoinToken{}, "", err
	}
	defer tx.Rollback()
	var removed int64
	switch err := tx.QueryRowContext(ctx, `SELECT removed_at FROM members WHERE name = ?`, name).Scan(&removed); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return JoinToken{}, "", err
	case removed == 0:
		return JoinToken{}, "", fmt.Errorf("%w: a member named %q is registered; remove it first to join it again with a new key", ErrExists, name)
	}
	res, err := tx.ExecContext(ctx, `UPDATE join_tokens SET revoked_at = ? WHERE member = ? AND used_at = 0 AND revoked_at = 0 AND expires_at > ?`,
		now.UnixMilli(), name, now.UnixMilli())
	if err != nil {
		return JoinToken{}, "", err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if err := audit(ctx, tx, now, actor, "join-token.revoke", name, fmt.Sprintf("%d unused, replaced by a new one", n)); err != nil {
			return JoinToken{}, "", err
		}
	}
	res, err = tx.ExecContext(ctx, `INSERT INTO join_tokens (member, hash, created_at, created_by, expires_at) VALUES (?, ?, ?, ?, ?)`,
		name, joinHash(secret), now.UnixMilli(), actor, now.Add(ttl).UnixMilli())
	if err != nil {
		return JoinToken{}, "", err
	}
	id, _ := res.LastInsertId()
	if err := audit(ctx, tx, now, actor, "join-token.issue", name, "valid "+shortDuration(ttl)); err != nil {
		return JoinToken{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return JoinToken{}, "", err
	}
	j, err := r.joinToken(ctx, id)
	return j, secret, err
}

func (r *Registry) joinToken(ctx context.Context, id int64) (JoinToken, error) {
	js, err := r.queryJoins(ctx, `WHERE id = ?`, id)
	if err != nil {
		return JoinToken{}, err
	}
	if len(js) == 0 {
		return JoinToken{}, ErrNotFound
	}
	return js[0], nil
}

// ActiveJoins are the join tokens of every name that can still be traded,
// by name, newest first.
func (r *Registry) ActiveJoins(ctx context.Context) ([]JoinToken, error) {
	return r.queryJoins(ctx, `WHERE used_at = 0 AND revoked_at = 0 AND expires_at > ? ORDER BY member, id DESC`, r.now().UnixMilli())
}

// JoinTokens are a member name's join tokens, newest first.
func (r *Registry) JoinTokens(ctx context.Context, name string) ([]JoinToken, error) {
	return r.queryJoins(ctx, `WHERE member = ? ORDER BY id DESC`, name)
}

func (r *Registry) queryJoins(ctx context.Context, where string, args ...any) ([]JoinToken, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, member, created_at, created_by, expires_at, used_at, revoked_at FROM join_tokens `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JoinToken{}
	for rows.Next() {
		var j JoinToken
		var created, expires, used, revoked int64
		if err := rows.Scan(&j.ID, &j.Member, &created, &j.CreatedBy, &expires, &used, &revoked); err != nil {
			return nil, err
		}
		j.CreatedAt, j.ExpiresAt, j.UsedAt, j.RevokedAt = millis(created), millis(expires), millis(used), millis(revoked)
		out = append(out, j)
	}
	return out, rows.Err()
}

// RevokeJoin ends a member name's join token; an unknown or ended one is
// no error.
func (r *Registry) RevokeJoin(ctx context.Context, actor, name string, id int64) error {
	now := r.now()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE join_tokens SET revoked_at = ? WHERE id = ? AND member = ? AND used_at = 0 AND revoked_at = 0`, now.UnixMilli(), id, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if err := audit(ctx, tx, now, actor, "join-token.revoke", name, "token #"+strconv.FormatInt(id, 10)); err != nil {
		return err
	}
	return tx.Commit()
}

// Join trades a join token for a member's registration with publicKey.
// The token is checked first: without one issued for the name, the
// answer is ErrDenied whatever the key (it never says who holds a key). A
// key registered under the name already is no error, also with a token
// used, expired or revoked since (the box asks again after a restart):
// the member is returned as it is. The token's use, the registration and
// their audit records are one transaction.
func (r *Registry) Join(ctx context.Context, name, secret, publicKey string) (Member, error) {
	pemKey, fp, err := memberKey(publicKey)
	if err != nil {
		return Member{}, fmt.Errorf("%w: give one public key (PEM)", ErrInvalid)
	}
	denied := fmt.Errorf("%w: the join token is unknown, used, revoked, expired or for another name: issue a new one at the hub", ErrDenied)
	now := r.now()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Member{}, err
	}
	defer tx.Rollback()
	var id, expires, used, revoked int64
	var by string
	// Only integers count: a restored file may hold other values (text
	// '00', a far date as text) that SQL and Go read differently.
	switch err := tx.QueryRowContext(ctx, `SELECT id, created_by, expires_at, used_at, revoked_at FROM join_tokens WHERE hash = ? AND member = ?
		AND typeof(expires_at) = 'integer' AND typeof(used_at) = 'integer' AND typeof(revoked_at) = 'integer'`,
		joinHash(secret), name).Scan(&id, &by, &expires, &used, &revoked); {
	case errors.Is(err, sql.ErrNoRows):
		return Member{}, denied
	case err != nil:
		return Member{}, err
	}
	var holder string
	switch err := tx.QueryRowContext(ctx, `SELECT name FROM members WHERE fingerprint = ? AND removed_at = 0`, fp).Scan(&holder); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return Member{}, err
	case holder == name:
		tx.Rollback()
		return r.Get(ctx, name)
	default:
		return Member{}, fmt.Errorf("%w: this key is %q's", ErrExists, holder)
	}
	if used != 0 || revoked != 0 || expires <= now.UnixMilli() {
		return Member{}, denied
	}
	if _, err := tx.ExecContext(ctx, `UPDATE join_tokens SET used_at = ? WHERE id = ?`, now.UnixMilli(), id); err != nil {
		return Member{}, err
	}
	actor := "member:" + name
	if err := addTx(ctx, tx, now, actor, OriginJoin, name, pemKey, fp); err != nil {
		return Member{}, err
	}
	if err := audit(ctx, tx, now, actor, "member.join", name, "join token #"+strconv.FormatInt(id, 10)+" issued by "+by); err != nil {
		return Member{}, err
	}
	if err := tx.Commit(); err != nil {
		return Member{}, err
	}
	r.committed(ctx, r.reload)
	return r.Get(context.WithoutCancel(ctx), name)
}

// OriginJoin: a member registered by trading a join token.
const OriginJoin = "join"

// JoinHandler serves JoinPath: a box trades its join token.
func (r *Registry) JoinHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var in struct {
			Name      string `json:"name"`
			Token     string `json:"token"`
			PublicKey string `json:"public_key"`
		}
		dec := json.NewDecoder(io.LimitReader(req.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			fail(w, http.StatusBadRequest, "invalid", "a JSON object with name, token and public_key: "+err.Error())
			return
		}
		m, err := r.Join(req.Context(), in.Name, in.Token, in.PublicKey)
		if errors.Is(err, ErrDenied) {
			fail(w, http.StatusForbidden, "denied", err.Error())
			return
		}
		if err != nil {
			failErr(w, err)
			return
		}
		reply(w, http.StatusOK, map[string]string{"name": m.Name, "fingerprint": m.Fingerprint})
	})
}

// shortDuration is a token's lifetime as the audit says it: 1h, 7d, 45m.
func shortDuration(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/time.Minute)) + "m"
}
