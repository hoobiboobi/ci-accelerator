// Package store keeps what the console shows: builds, their steps and
// reasons, the job data around them, and the steps' output.
//
// The main database is one SQLite file. Step output goes into one SQLite
// file per UTC day (see output.go). One process writes; the files must be on
// a local or block volume, never on a shared file system.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Options configure a store. Zero values take the defaults.
type Options struct {
	// Keep is how long builds are kept (default 30 days).
	Keep time.Duration
	// KeepOutput is how many days of step output are kept (default 7).
	KeepOutput int
	// KeepDays is how many days the images' daily figures are kept
	// (default 365).
	KeepDays int
	// MaxBytes bounds what the database holds (its pages in use; the
	// output files have their own days, KeepOutput): above it Prune
	// deletes the oldest builds of days already summed up before their
	// retention ends, down to nine tenths of it. 0: no bound.
	MaxBytes int64
	// Now replaces the clock in tests.
	Now func() time.Time
	// TrustGitComparison counts a rebuild as the build farm's when GitLab's
	// comparison of the commits shows no change in the files its first
	// rebuilt step copies (or the image's previous build built the same
	// commit). Off by default: the comparison sees only the repository,
	// while a build context also holds what the job made or fetched (CI
	// artifacts, generated files), which can change with the same commit.
	TrustGitComparison bool
}

// TrustGitComparisonEnv switches Options.TrustGitComparison on in the
// controller ("true").
const TrustGitComparisonEnv = "CI_ACCELERATOR_TRUST_GIT_COMPARISON"

// Store is an open store directory.
type Store struct {
	dir  string
	db   *sql.DB
	opts Options
	key  []byte

	mu      sync.Mutex
	outputs map[int]*outputFile // by day, yyyymmdd

	changes Changes
}

// ErrNewerSchema is returned for a database written by a newer version.
var ErrNewerSchema = errors.New("store: the database has a newer schema than this version knows")

// Open opens or creates the store in dir.
func Open(ctx context.Context, dir string, opts Options) (*Store, error) {
	if opts.KeepDays == 0 {
		opts.KeepDays = 365
	}
	if opts.Keep == 0 {
		opts.Keep = 30 * 24 * time.Hour
	}
	if opts.KeepOutput == 0 {
		opts.KeepOutput = 7
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := openSQLite(filepath.Join(dir, "console.db"))
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, db: db, opts: opts, outputs: map[int]*outputFile{}}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if s.key, err = s.secretKey(ctx); err != nil {
		db.Close()
		return nil, err
	}
	// The query planner needs table statistics: without them it reads the
	// jobs and builds tables through the wrong index (a pipeline search took
	// seconds at a few hundred thousand jobs). 0x10002: analyze every table
	// that needs it, bounded, as SQLite recommends at open; Prune refreshes
	// them. They are only statistics: on a full volume the store still
	// opens without them, so that its pages stay readable and Prune can
	// free room.
	if _, err := s.db.ExecContext(ctx, "PRAGMA optimize=0x10002"); err != nil && !isFull(err) {
		db.Close()
		return nil, err
	}
	if err := s.applyGitTrust(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// openSQLite opens a database file with the settings every file here uses:
// WAL (readers do not block the writer), NORMAL sync (durable at
// checkpoints; a power cut can lose the last transactions, which the
// collector reads again), foreign keys, a busy timeout, write transactions
// that take the lock when they begin, and a write-ahead log cut back to
// walLimit after a checkpoint (it otherwise keeps its largest size until
// the file is closed). The file and its log are the owner's only.
func openSQLite(path string) (*sql.DB, error) {
	ownerOnly(path)
	q := url.Values{}
	// auto_vacuum first: it takes effect only before the file is set up,
	// which switching to WAL already does.
	for _, p := range []string{"auto_vacuum(INCREMENTAL)", "busy_timeout(10000)", "foreign_keys(1)", "journal_mode(WAL)",
		"synchronous(NORMAL)", fmt.Sprintf("journal_size_limit(%d)", walLimit)} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	return db, nil
}

// walLimit is the size a database's write-ahead log is cut back to.
const walLimit = 64 << 20

// ownerOnly makes a database file (created empty when missing: SQLite
// takes an empty file for a new database) and its log and shared-memory
// files readable by their owner only. They hold build output, Dockerfiles
// and sealed tokens; SQLite creates the log files with the database
// file's mode. Errors are left to SQLite's open.
func ownerOnly(path string) {
	if f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600); err == nil {
		f.Close()
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().Perm()&0o077 != 0 {
			_ = os.Chmod(p, fi.Mode().Perm()&0o700)
		}
	}
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("%w (schema %d, known %d)", ErrNewerSchema, version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, repairs); err != nil {
		return fmt.Errorf("store: repairs: %w", err)
	}
	for _, c := range toleratedColumns {
		var n, tables int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info(?) WHERE name = ?", c.table, c.column).Scan(&n); err != nil {
			return err
		}
		// A table a later migration of this schema creates is not there yet.
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", c.table).Scan(&tables); err != nil {
			return err
		}
		if n == 0 && tables > 0 {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.def)); err != nil {
				return fmt.Errorf("store: adding %s.%s: %w", c.table, c.column, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, toleratedIndexes); err != nil {
		return fmt.Errorf("store: tolerated indexes: %w", err)
	}
	return tx.Commit()
}

// secretKey returns the installation's key for hashing secret values,
// created at the first start. It never changes: hashes made with another
// key would not match the stored ones.
func (s *Store) secretKey(ctx context.Context) ([]byte, error) {
	var key []byte
	err := s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE name = 'secret_key'").Scan(&key)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO meta (name, value) VALUES ('secret_key', ?) ON CONFLICT (name) DO NOTHING", key); err != nil {
		return nil, err
	}
	return s.secretKey(ctx)
}

// SecretKey is the key history.Config needs for this store.
func (s *Store) SecretKey() []byte { return s.key }

// Close closes the store and its output files.
func (s *Store) Close() error {
	s.mu.Lock()
	files := s.outputs
	s.outputs = map[int]*outputFile{}
	s.mu.Unlock()
	var errs []error
	for _, f := range files {
		errs = append(errs, f.close())
	}
	errs = append(errs, s.db.Close())
	return errors.Join(errs...)
}
