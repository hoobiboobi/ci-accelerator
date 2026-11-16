package fleet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// A hub's configuration lives in fleet.db: members, roles, tokens, the
// clusters' settings and sealed secrets, the audit. Backup copies it while
// the hub runs; a restore is staged next to it and applied when the hub
// starts. It brings the whole file back as it was: members removed since
// the backup, roles taken away since and secrets changed since return too.
// Only the tokens are voided, in the staged copy and again at that start,
// as they are bearer secrets.

// restoreName is the staged backup the next start applies.
const restoreName = "fleet.db.restore"

// Backup writes a consistent copy of fleet.db to path (which must not
// exist), while the hub runs. Only the hub's user may read the copy.
func (r *Registry) Backup(ctx context.Context, path string) error {
	// Made empty first, with its mode: VACUUM INTO writes into an empty
	// file and keeps the mode.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s exists: give a new file", path)
	}
	if err != nil {
		return err
	}
	f.Close()
	if _, err := r.db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		os.Remove(path)
		return err
	}
	return os.Chmod(path, 0o600)
}

// StageRestore checks that file is a backup of a hub's fleet.db and puts a
// copy of it in dir, to be applied at the hub's next start. The copy is
// made by SQLite (a file still in use gives what its journal holds too),
// checked again, and its tokens are voided before it is staged: no crash
// between the restore and the hub's start brings a revoked token back.
func StageRestore(ctx context.Context, dir, file string) error {
	if err := checkBackup(ctx, file, file); err != nil {
		return err
	}
	tmp := filepath.Join(dir, restoreName+".part")
	for _, f := range []string{tmp, tmp + "-journal", tmp + "-wal", tmp + "-shm"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := stageCopy(ctx, file, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, restoreName))
}

// stageCopy copies the backup file into tmp and voids the copy's tokens.
func stageCopy(ctx context.Context, file, tmp string) error {
	// Made empty first, with its mode: VACUUM INTO keeps it.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	src, err := sql.Open("sqlite", sqliteURI(file, url.Values{"mode": {"ro"}}))
	if err != nil {
		return err
	}
	_, err = src.ExecContext(ctx, `VACUUM INTO ?`, tmp)
	src.Close()
	if err != nil {
		return fmt.Errorf("copying %s: %w", file, err)
	}
	if err := checkBackup(ctx, tmp, file); err != nil {
		return err
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(DELETE)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", sqliteURI(tmp, q))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// An older hub's backup may lack a newer table: VoidTokens needs them
	// (the hub's start makes them all the same).
	if err := createSchema(ctx, db); err != nil {
		return err
	}
	staged := &Registry{db: db, now: time.Now}
	if _, err := staged.VoidTokens(ctx, OriginLocal, "a backup was staged for restore"); err != nil {
		return fmt.Errorf("voiding the tokens of the staged copy: %w", err)
	}
	if err := db.Close(); err != nil {
		return err
	}
	return os.Chmod(tmp, 0o600)
}

// checkBackup opens file read-only and checks it has a hub's tables and
// nothing that runs inside the hub's own statements (triggers, views);
// errors name the backup as given.
func checkBackup(ctx context.Context, file, given string) error {
	if _, err := os.Stat(file); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", sqliteURI(file, url.Values{"mode": {"ro"}}))
	if err != nil {
		return err
	}
	defer db.Close()
	var foreign string
	switch err := db.QueryRowContext(ctx, `SELECT type || ' ' || name FROM sqlite_master WHERE type IN ('trigger', 'view') LIMIT 1`).Scan(&foreign); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("%s is not a hub's backup: %w", given, err)
	default:
		return fmt.Errorf("%s is not a hub's backup: it holds %s, which a hub never makes", given, foreign)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('members', 'audit')`).Scan(&n); err != nil {
		return fmt.Errorf("%s is not a hub's backup: %w", given, err)
	}
	if n != 2 {
		return fmt.Errorf("%s is not a hub's backup (no members and audit tables)", given)
	}
	var ok string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&ok); err != nil || ok != "ok" {
		return fmt.Errorf("%s is damaged: %v %s", given, err, ok)
	}
	return nil
}

// ApplyRestore replaces dir's fleet.db with a staged backup, if one is
// there, before the hub opens it; the replaced file is kept beside it.
// StageRestore voided the copy's tokens; the caller voids them again
// (VoidTokens) once the registry is open.
func ApplyRestore(dir string, now time.Time) (before string, applied bool, err error) {
	staged := filepath.Join(dir, restoreName)
	if _, err := os.Stat(staged); errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	cur := filepath.Join(dir, "fleet.db")
	before = filepath.Join(dir, "fleet.db.before-restore-"+now.UTC().Format("20060102-150405"))
	if _, err := os.Stat(cur); err == nil {
		if err := os.Rename(cur, before); err != nil {
			return "", false, err
		}
	} else {
		before = ""
	}
	for _, f := range []string{"fleet.db-wal", "fleet.db-shm"} {
		p := filepath.Join(dir, f)
		if before != "" {
			// The replaced file's journal goes with it.
			os.Rename(p, before+f[len("fleet.db"):])
		} else {
			os.Remove(p)
		}
	}
	if err := os.Rename(staged, cur); err != nil {
		return before, false, err
	}
	return before, true, nil
}
