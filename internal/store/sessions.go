package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Session is a person signed in through a GitLab instance. ID is a hash of
// the session cookie; Token is the person's GitLab token, sealed by the
// caller with a key the store does not have.
type Session struct {
	ID       []byte
	Instance string
	UserID   uint64
	Username string
	Name     string
	Token    []byte
	Created  time.Time
	Seen     time.Time
}

// SaveSession stores a new session.
func (s *Store) SaveSession(ctx context.Context, x Session) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, instance, user_id, username, name, token, created_at, seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, x.ID, x.Instance, x.UserID, x.Username, x.Name, x.Token, x.Created.UnixMilli(), x.Seen.UnixMilli())
	return err
}

// Session returns the session with this id, or ErrNotFound.
func (s *Store) Session(ctx context.Context, id []byte) (Session, error) {
	x := Session{ID: id}
	var created, seen int64
	err := s.db.QueryRowContext(ctx, `SELECT instance, user_id, username, name, token, created_at, seen_at FROM sessions WHERE id = ?`, id).
		Scan(&x.Instance, &x.UserID, &x.Username, &x.Name, &x.Token, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	x.Created, x.Seen = time.UnixMilli(created).UTC(), time.UnixMilli(seen).UTC()
	return x, err
}

// TouchSession records that a session was used at t.
func (s *Store) TouchSession(ctx context.Context, id []byte, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET seen_at = ? WHERE id = ?`, t.UnixMilli(), id)
	return err
}

// SetSessionToken replaces a session's sealed token (after a refresh).
func (s *Store) SetSessionToken(ctx context.Context, id, token []byte) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET token = ? WHERE id = ?`, token, id)
	return err
}

// DeleteSession removes a session; removing one that is gone is no error.
func (s *Store) DeleteSession(ctx context.Context, id []byte) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// DeleteIdleSessions removes the sessions not used since before and says
// how many. Their tokens are not revoked: GitLab keeps the person's grant,
// so signing in again needs no new consent.
func (s *Store) DeleteIdleSessions(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE seen_at < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
