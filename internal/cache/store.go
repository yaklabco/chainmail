// Package cache keeps cache.db, the daemon's local copy of synced mail:
// message headers for every synced folder, bodies for a window plus recent
// reads, and an FTS5 index over both. It is a projection of the watch event
// log and can be deleted at any time; the daemon builds it again.
package cache

import (
	"context"
	"database/sql"
	_ "embed" // schema.sql
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/sqlitefile"
)

// SchemaVersion is cache.db's PRAGMA user_version. A file with any other
// version is deleted and created again: the cache has no migrations.
// Version 2 added the unreadable table and messages.attachments_unknown.
const SchemaVersion = 2

//go:embed schema.sql
var schema string

// OpenOptions configure Open.
type OpenOptions struct {
	// Now stamps cached_at, fetched_at, last_read_at, last_attempt_at and
	// removed_at. Nil means time.Now.
	Now func() time.Time
	// Logger receives a warning when Open recreates the file. Nil discards.
	Logger *slog.Logger
}

// Store reads and writes cache.db. Nothing else issues SQL against the file.
// It holds one connection, so no method runs a query while another's Rows
// are open, and none holds a transaction across network IO.
type Store struct {
	db     *sql.DB
	path   string // the database file, for the corruption marker
	now    func() time.Time
	logger *slog.Logger
	// evict is the size cap state shared by the prefetchers, the read path
	// and the evictor (evict.go).
	evict evictState
	// beforeRepair, when set, runs as a body write starts repairing its
	// row's attachments. Tests set it.
	beforeRepair func()
}

// Open opens or creates the cache at path. A file with another
// user_version, or one SQLite reports as corrupt or not a database, is
// closed, deleted with its -wal and -shm files, and created again, with a
// warning. So is a file with a corruption marker (path + ".corrupt"),
// which a Store leaves when an operation finds corruption that Open's
// checks cannot see; Open removes the marker once the new file exists.
// cache.db and its -wal and -shm files are mode 0600 whatever the umask.
// internal/sqlitefile holds this life cycle, which calendar.db shares.
func Open(ctx context.Context, path string, opts OpenOptions) (*Store, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	conn, err := sqlitefile.Open(ctx, path, sqlitefile.Spec{
		What:     "cache",
		Name:     "cache.db",
		Version:  SchemaVersion,
		Schema:   schema,
		Classify: classify,
		OnRecreate: func(reason string) {
			opts.Logger.Warn("recreating cache.db", "path", path, "reason", reason)
		},
	})
	if err != nil {
		return nil, err
	}
	return &Store{db: conn, path: path, now: opts.Now, logger: opts.Logger}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// SizeBytes is the size of cache.db: page_count * page_size. It counts
// headers, bodies and the FTS index, and needs no file-system call.
func (s *Store) SizeBytes(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		"SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()").Scan(&n)
	if err != nil {
		return 0, s.fail("reading the cache size", err)
	}
	return n, nil
}

// fail names the operation that failed and classifies err. Every Store
// method's database error goes through it, so when err is ErrCorrupt it
// also leaves the marker that makes the next Open recreate the file.
func (s *Store) fail(op string, err error) error {
	err = classify(err)
	if errors.Is(err, ErrCorrupt) {
		s.markCorrupt()
	}
	return fmt.Errorf("%s: %w", op, err)
}

// markCorrupt creates the corruption marker. Failing to create it only
// costs the recreate on the next Open, so it is logged and otherwise ignored.
func (s *Store) markCorrupt() {
	if err := sqlitefile.MarkCorrupt(s.path); err != nil {
		s.logger.Warn("cannot mark cache.db corrupt", "path", s.path, "err", err)
	}
}

// inTx runs fn in one transaction and commits it. Errors name op and carry
// ErrDiskFull or ErrCorrupt when SQLite reports either.
func (s *Store) inTx(ctx context.Context, op string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return s.fail(op, err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit; an earlier error is the one to report
	if err := fn(tx); err != nil {
		return s.fail(op, err)
	}
	if err := tx.Commit(); err != nil {
		return s.fail(op, err)
	}
	return nil
}

// fromUnix turns stored Unix seconds into a UTC time.
func fromUnix(sec int64) time.Time {
	return time.Unix(sec, 0).UTC()
}

// boolInt renders b as the 0 or 1 the flag columns hold.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// placeholders returns n comma-separated "?".
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// nullUnix stores t as Unix seconds, or NULL when t is zero.
func nullUnix(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

// fromNullUnix reads a nullable Unix-seconds column; NULL is the zero time.
func fromNullUnix(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return fromUnix(n.Int64)
}
