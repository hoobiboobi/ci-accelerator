package fleet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// The hub keeps its members and its audit trail in fleet.db next to its
// console store. Its tables are created idempotently, never by numbered
// migrations: a hub rolled back to an older release opens the file as it
// is (an older hub ignores columns it does not know).

const schema = `
CREATE TABLE IF NOT EXISTS members (
	name        TEXT PRIMARY KEY,
	public_key  TEXT NOT NULL,
	fingerprint TEXT NOT NULL UNIQUE,
	origin      TEXT NOT NULL,
	added_at    INTEGER NOT NULL,
	added_by    TEXT NOT NULL,
	store       TEXT NOT NULL DEFAULT '',
	store_was   TEXT NOT NULL DEFAULT '',
	store_at    INTEGER NOT NULL DEFAULT 0,
	version     TEXT NOT NULL DEFAULT '',
	seen_at     INTEGER NOT NULL DEFAULT 0,
	removed_at  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS audit (
	id     INTEGER PRIMARY KEY,
	at     INTEGER NOT NULL,
	actor  TEXT NOT NULL,
	action TEXT NOT NULL,
	target TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS audit_at ON audit(at);
CREATE INDEX IF NOT EXISTS audit_actor ON audit(actor, id);
CREATE INDEX IF NOT EXISTS audit_target ON audit(target, id);
`

// Registry is the hub's list of members.
type Registry struct {
	db  *sql.DB
	now func() time.Time
	// KeepRemoved is how long a removed member's record is kept (its name
	// may be registered again at once), and ended tokens are listed.
	KeepRemoved time.Duration
	// KeepAudit is how long audit records are kept.
	KeepAudit time.Duration
	// Log gets what the registry cannot return (nil: slog.Default()).
	Log *slog.Logger

	// reloadMu makes each reload's read and assignment one step, so an
	// older read never replaces a newer one.
	reloadMu sync.Mutex
	mu       sync.Mutex
	byKey    map[string]string // fingerprint -> name, members not removed
	roles    map[string]Binding
}

// Member is one member as the hub knows it.
type Member struct {
	Name        string    `json:"name"`
	PublicKey   string    `json:"public_key"`
	Fingerprint string    `json:"fingerprint"`
	Origin      string    `json:"origin"`
	AddedAt     time.Time `json:"added_at"`
	AddedBy     string    `json:"added_by"`
	// Store is the member's store id as last seen; StoreReplaced: a store
	// other than the first one came under this name (its old builds'
	// addresses lead nowhere now).
	Store         string    `json:"store,omitempty"`
	StoreReplaced time.Time `json:"store_replaced,omitzero"`
	Version       string    `json:"version,omitempty"`
	SeenAt        time.Time `json:"seen_at,omitzero"`
	RemovedAt     time.Time `json:"removed_at,omitzero"`
}

// Event is one audit record.
type Event struct {
	ID     int64     `json:"id"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target"`
	Detail string    `json:"detail,omitempty"`
}

var (
	// ErrExists: a member of that name, or with that key, exists.
	ErrExists = errors.New("exists")
	// ErrNotFound: no member of that name.
	ErrNotFound = errors.New("no such member")
	// ErrInvalid: the request is not acceptable as it is.
	ErrInvalid = errors.New("invalid")
)

// NamePattern is what a member's name may look like: it is part of the
// console's addresses and of Kubernetes object names.
var NamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// newName says why name cannot be given to a new member, nil when it can.
// "new" is the address of the console's Add a cluster form.
func newName(name string) error {
	if !NamePattern.MatchString(name) {
		return fmt.Errorf("%w: a name is lower-case letters, digits and dashes, at most 40, starting and ending with a letter or digit", ErrInvalid)
	}
	if name == "new" {
		return fmt.Errorf(`%w: the name "new" is the address of the Add a cluster form; choose another`, ErrInvalid)
	}
	return nil
}

// OpenRegistry opens (or creates) fleet.db in dir.
func OpenRegistry(ctx context.Context, dir string) (*Registry, error) {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	file := filepath.Join(dir, "fleet.db")
	// The file holds token hashes and sealed secrets: only the hub's user
	// reads it (SQLite gives its journal files the same mode).
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err := os.Chmod(file, 0o600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteURI(file, q))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	if err := createSchema(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	r := &Registry{db: db, now: time.Now, KeepRemoved: 30 * 24 * time.Hour, KeepAudit: 365 * 24 * time.Hour}
	if err := r.Refresh(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return r, nil
}

// Refresh reads again what the registry keeps in memory (the members'
// keys, the roles): after a change made by another process on the same
// file, a command inside the hub's pod.
func (r *Registry) Refresh(ctx context.Context) error {
	if err := r.reload(ctx); err != nil {
		return err
	}
	return r.reloadRoles(ctx)
}

// sqliteURI is a file's SQLite URI: the path escaped, so that a '?', '#'
// or '%' in it stays part of the file name.
func sqliteURI(file string, q url.Values) string {
	u := url.URL{Scheme: "file", Path: file, RawQuery: q.Encode()}
	if !filepath.IsAbs(file) {
		// A relative path: "file:" and the escaped path, no authority.
		u = url.URL{Scheme: "file", Opaque: (&url.URL{Path: file}).EscapedPath(), RawQuery: q.Encode()}
	}
	return u.String()
}

func (r *Registry) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// Close closes the file.
func (r *Registry) Close() error { return r.db.Close() }

func (r *Registry) reload(ctx context.Context) error {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	rows, err := r.db.QueryContext(ctx, `SELECT fingerprint, name FROM members WHERE removed_at = 0`)
	if err != nil {
		return err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var fp, n string
		if err := rows.Scan(&fp, &n); err != nil {
			return err
		}
		m[fp] = n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	r.byKey = m
	r.mu.Unlock()
	return nil
}

// committed brings the maps up to date after a committed change. The
// change is made whatever happens here: a cancelled request does not stop
// the reload, and a failed one is caught up by the next Refresh.
func (r *Registry) committed(ctx context.Context, reload func(context.Context) error) {
	reload(context.WithoutCancel(ctx))
}

// Holder names the member that holds a key; "" and why when none does.
func (r *Registry) Holder(fingerprint string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.byKey[fingerprint]; ok {
		return n, nil
	}
	return "", errors.New("no member holds this key")
}

// Add registers a member by its public key (PEM). origin is "api" or "ui";
// actor is who asked ("gitlab:<instance>/<id>", "token:<name>", ...).
func (r *Registry) Add(ctx context.Context, actor, origin, name, publicKey string) (Member, error) {
	if err := newName(name); err != nil {
		return Member{}, err
	}
	pemKey, fp, err := memberKey(publicKey)
	if err != nil {
		return Member{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Member{}, err
	}
	defer tx.Rollback()
	if err := addTx(ctx, tx, r.now(), actor, origin, name, pemKey, fp); err != nil {
		return Member{}, err
	}
	if err := tx.Commit(); err != nil {
		return Member{}, err
	}
	r.committed(ctx, r.reload)
	return r.Get(context.WithoutCancel(ctx), name)
}

// memberKey checks a member's public key: one key, PEM; it returns the
// key as the hub stores it and its fingerprint.
func memberKey(publicKey string) (pemKey, fp string, err error) {
	keys, err := ParsePublicKeys([]byte(publicKey))
	if err != nil {
		return "", "", fmt.Errorf("%w: public key: %v", ErrInvalid, err)
	}
	if len(keys) != 1 {
		return "", "", fmt.Errorf("%w: give one public key", ErrInvalid)
	}
	b, err := PublicPEM(keys[0])
	if err != nil {
		return "", "", fmt.Errorf("%w: public key: %v", ErrInvalid, err)
	}
	return string(b), Fingerprint(keys[0]), nil
}

// addTx registers a member inside tx (Add, and Join with its token's use).
func addTx(ctx context.Context, tx *sql.Tx, now time.Time, actor, origin, name, pemKey, fp string) error {
	var removed int64
	var oldStore string
	switch err := tx.QueryRowContext(ctx, `SELECT removed_at, store FROM members WHERE name = ?`, name).Scan(&removed, &oldStore); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case removed == 0:
		return fmt.Errorf("%w: a member named %q is registered", ErrExists, name)
	default:
		// A removed member's name comes back: the record starts again,
		// its last store kept so that a different one shows as replaced.
		if _, err := tx.ExecContext(ctx, `DELETE FROM members WHERE name = ?`, name); err != nil {
			return err
		}
	}
	detail := "key " + fp + ", origin " + origin
	var other string
	var otherRemoved int64
	switch err := tx.QueryRowContext(ctx, `SELECT name, removed_at FROM members WHERE fingerprint = ?`, fp).Scan(&other, &otherRemoved); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case otherRemoved == 0:
		return fmt.Errorf("%w: this key is %q's", ErrExists, other)
	default:
		// The key of a removed member (a cluster joined again under
		// another name with its kept key): the new member takes it over.
		if _, err := tx.ExecContext(ctx, `DELETE FROM members WHERE name = ?`, other); err != nil {
			return err
		}
		detail += ", the key of removed " + other
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO members (name, public_key, fingerprint, origin, added_at, added_by, store) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, pemKey, fp, origin, now.UnixMilli(), actor, oldStore); err != nil {
		return err
	}
	return audit(ctx, tx, now, actor, "member.add", name, detail)
}

// Remove takes a member out: its key no longer connects (the caller
// closes a live link), its record is kept KeepRemoved. The name's unused
// join tokens and its kept settings and secrets go with it, also for a
// name that only waits to join (RemoveCluster); ErrNotFound: no member of
// that name was registered (what else the name had is gone all the same).
func (r *Registry) Remove(ctx context.Context, actor, name string) error {
	removed, err := r.RemoveCluster(ctx, actor, name)
	if removed {
		// Committed: a failed or cancelled reload is caught up here.
		r.committed(ctx, r.reload)
		return nil
	}
	if err == nil {
		return ErrNotFound
	}
	return err
}

// Seen records that a member connected with a store and version. It
// returns true when the store differs from the one seen before (the
// cluster's store was replaced: its old builds' addresses lead nowhere).
func (r *Registry) Seen(ctx context.Context, name, store, version string) (replaced bool, err error) {
	now := r.now()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var was string
	if err := tx.QueryRowContext(ctx, `SELECT store FROM members WHERE name = ? AND removed_at = 0`, name).Scan(&was); err != nil {
		return false, err
	}
	replaced = was != "" && store != "" && was != store
	if replaced {
		if _, err := tx.ExecContext(ctx, `UPDATE members SET store_was = store, store_at = ? WHERE name = ?`, now.UnixMilli(), name); err != nil {
			return false, err
		}
		if err := audit(ctx, tx, now, "member:"+name, "member.store-replaced", name, "store "+was+" -> "+store); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE members SET store = CASE WHEN ? = '' THEN store ELSE ? END, version = ?, seen_at = ? WHERE name = ?`,
		store, store, version, now.UnixMilli(), name); err != nil {
		return false, err
	}
	return replaced, tx.Commit()
}

// Get is one member, removed ones included.
func (r *Registry) Get(ctx context.Context, name string) (Member, error) {
	ms, err := r.list(ctx, `WHERE name = ?`, name)
	if err != nil {
		return Member{}, err
	}
	if len(ms) == 0 {
		return Member{}, ErrNotFound
	}
	return ms[0], nil
}

// List is every member, removed ones last.
func (r *Registry) List(ctx context.Context) ([]Member, error) {
	return r.list(ctx, `ORDER BY removed_at != 0, name`)
}

func (r *Registry) list(ctx context.Context, where string, args ...any) ([]Member, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT name, public_key, fingerprint, origin, added_at, added_by, store, store_at, version, seen_at, removed_at FROM members `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	ms := func(v int64) time.Time {
		if v == 0 {
			return time.Time{}
		}
		return time.UnixMilli(v).UTC()
	}
	for rows.Next() {
		var m Member
		var added, storeAt, seen, removed int64
		if err := rows.Scan(&m.Name, &m.PublicKey, &m.Fingerprint, &m.Origin, &added, &m.AddedBy, &m.Store, &storeAt, &m.Version, &seen, &removed); err != nil {
			return nil, err
		}
		m.AddedAt, m.StoreReplaced, m.SeenAt, m.RemovedAt = ms(added), ms(storeAt), ms(seen), ms(removed)
		out = append(out, m)
	}
	return out, rows.Err()
}

// AuditQuery picks audit records.
type AuditQuery struct {
	// Limit: at most this many (100 when 0, 1000 when more).
	Limit int
	// Before: older than this record id (the next page); 0: the newest.
	Before int64
	// Actor, Target: only records of this actor, of this target ("" any).
	Actor, Target string
}

// Audit is the newest audit records first, at most limit.
func (r *Registry) Audit(ctx context.Context, limit int) ([]Event, error) {
	return r.AuditQuery(ctx, AuditQuery{Limit: limit})
}

// AuditQuery is the audit records q picks, newest first.
func (r *Registry) AuditQuery(ctx context.Context, q AuditQuery) ([]Event, error) {
	switch {
	case q.Limit <= 0:
		q.Limit = 100
	case q.Limit > 1000:
		q.Limit = 1000
	}
	where, args := []string{"1 = 1"}, []any{}
	if q.Before > 0 {
		where, args = append(where, "id < ?"), append(args, q.Before)
	}
	if q.Actor != "" {
		where, args = append(where, "actor = ?"), append(args, q.Actor)
	}
	if q.Target != "" {
		where, args = append(where, "target = ?"), append(args, q.Target)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, at, actor, action, target, detail FROM audit WHERE `+strings.Join(where, " AND ")+
		` ORDER BY id DESC LIMIT ?`, append(args, q.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// Record writes an audit record of something done outside the registry.
func (r *Registry) Record(ctx context.Context, actor, action, target, detail string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO audit (at, actor, action, target, detail) VALUES (?, ?, ?, ?, ?)`,
		r.now().UnixMilli(), actor, action, target, detail)
	return err
}

// Prune forgets removed members and audit records past their keep.
func (r *Registry) Prune(ctx context.Context) error {
	now := r.now()
	// A negative keep would reach into the future: tokens still valid.
	ended := now.Add(-max(r.KeepRemoved, 0)).UnixMilli()
	if _, err := r.db.ExecContext(ctx, `DELETE FROM members WHERE removed_at != 0 AND removed_at < ?`, ended); err != nil {
		return err
	}
	// Ended API and join tokens stay listed as long as removed members.
	if _, err := r.db.ExecContext(ctx, `DELETE FROM join_tokens WHERE expires_at < ? OR (used_at != 0 AND used_at < ?) OR (revoked_at != 0 AND revoked_at < ?)`, ended, ended, ended); err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE (revoked_at != 0 AND revoked_at < ?) OR (expires_at != 0 AND expires_at < ?)`, ended, ended); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM audit WHERE at < ?`, now.Add(-max(r.KeepAudit, 0)).UnixMilli())
	return err
}

func audit(ctx context.Context, tx *sql.Tx, at time.Time, actor, action, target, detail string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO audit (at, actor, action, target, detail) VALUES (?, ?, ?, ?, ?)`,
		at.UnixMilli(), actor, action, target, strings.TrimSpace(detail))
	return err
}
