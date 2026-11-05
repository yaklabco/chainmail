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
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
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
}

// recreateError says why Open deletes the file: "corrupt", "version" or
// "unknown_tables". The reason is a fixed code, safe to log.
type recreateError struct {
	reason string
}

func (e *recreateError) Error() string {
	return "cache.db must be recreated: " + e.reason
}

// Open opens or creates the cache at path. A file with another
// user_version, or one SQLite reports as corrupt or not a database, is
// closed, deleted with its -wal and -shm files, and created again. So is a
// file with a corruption marker (path + ".corrupt"), which a Store leaves
// when an operation finds corruption that Open's checks cannot see; Open
// removes the marker once the new file exists. cache.db and its -wal and
// -shm files are mode 0600 whatever the umask (makePrivate).
func Open(ctx context.Context, path string, opts OpenOptions) (*Store, error) {
	// path goes into a file: URI, where these characters change its meaning.
	if strings.ContainsAny(path, "?#%") {
		return nil, fmt.Errorf("cache path %q must not contain '?', '#' or '%%'", path)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	marked, err := hasMarker(path)
	if err != nil {
		return nil, err
	}
	var conn *sql.DB
	if marked {
		err = &recreateError{reason: "corrupt"}
	} else {
		conn, err = openFile(ctx, path)
	}
	if re, ok := errors.AsType[*recreateError](err); ok {
		opts.Logger.Warn("recreating cache.db", "path", path, "reason", re.reason)
		if err := removeDB(path); err != nil {
			return nil, err
		}
		conn, err = openFile(ctx, path)
	}
	if err != nil {
		return nil, err
	}
	if marked {
		if err := os.Remove(markerPath(path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			_ = conn.Close() // the marker error is the one to report
			return nil, fmt.Errorf("removing the corruption marker: %w", err)
		}
	}
	return &Store{db: conn, path: path, now: opts.Now, logger: opts.Logger}, nil
}

// hasMarker reports whether a Store left the corruption marker for path.
func hasMarker(path string) (bool, error) {
	_, err := os.Stat(markerPath(path))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("checking the corruption marker: %w", err)
	}
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// privateMode is the mode of cache.db and the files beside it: they hold
// mail, so only the daemon's user may read them.
const privateMode fs.FileMode = 0o600

// makePrivate creates cache.db with privateMode when it is absent, before
// SQLite opens it, and sets privateMode on cache.db, its -wal and -shm
// files and the corruption marker when any of them is wider. SQLite
// creates its -wal and -shm files with the database file's mode, so new
// ones are private too. The directory's mode is left as it is: an
// existing directory is the operator's choice.
func makePrivate(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, privateMode)
	switch {
	case err == nil:
		if err := f.Close(); err != nil {
			return fmt.Errorf("creating %s: %w", path, err)
		}
	case !errors.Is(err, fs.ErrExist):
		return fmt.Errorf("creating %s: %w", path, err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm", markerPath(path)} {
		info, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("checking the mode of %s: %w", p, err)
		}
		if info.Mode().Perm()&^privateMode == 0 {
			continue
		}
		if err := os.Chmod(p, privateMode); err != nil {
			return fmt.Errorf("making %s private: %w", p, err)
		}
	}
	return nil
}

// openFile makes path private, opens it with one connection and prepares
// its schema.
func openFile(ctx context.Context, path string) (*sql.DB, error) {
	if err := makePrivate(path); err != nil {
		return nil, err
	}
	params := url.Values{"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)"}}
	conn, err := sql.Open("sqlite", "file:"+path+"?"+params.Encode())
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	conn.SetMaxOpenConns(1)
	if err := prepare(ctx, conn); err != nil {
		_ = conn.Close() // the prepare error is the one to report
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return conn, nil
}

// prepare checks the schema version and creates the schema in a new file.
// It returns a *recreateError when the file must be deleted first.
func prepare(ctx context.Context, conn *sql.DB) error {
	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return openErr("reading user_version", err)
	}
	if version == SchemaVersion {
		return nil
	}
	if version != 0 {
		return &recreateError{reason: "version"}
	}
	var objects int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(&objects); err != nil {
		return openErr("reading the schema", err)
	}
	if objects != 0 {
		return &recreateError{reason: "unknown_tables"}
	}
	return create(ctx, conn)
}

// openErr turns an error from checking an existing file into a
// *recreateError when SQLite reports the file corrupt, and otherwise names
// op and keeps the classification, so ErrDiskFull survives.
func openErr(op string, err error) error {
	err = classify(err)
	if errors.Is(err, ErrCorrupt) {
		return &recreateError{reason: "corrupt"}
	}
	return fmt.Errorf("%s: %w", op, err)
}

// create sets auto_vacuum and creates the schema in an empty file.
func create(ctx context.Context, conn *sql.DB) error {
	// journal_mode(WAL) has already written the file header, so auto_vacuum
	// takes effect only through a VACUUM, which is instant on an empty file.
	if _, err := conn.ExecContext(ctx, "PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
		return fmt.Errorf("setting auto_vacuum: %w", classify(err))
	}
	if _, err := conn.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("applying auto_vacuum: %w", classify(err))
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("creating the schema: %w", classify(err))
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit; an earlier error is the one to report
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("creating the schema: %w", classify(err))
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(SchemaVersion)); err != nil {
		return fmt.Errorf("setting user_version: %w", classify(err))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("creating the schema: %w", classify(err))
	}
	return nil
}

// removeDB deletes the database file and its -wal and -shm files. The
// connection must be closed first, so this works where open files cannot
// be removed. A missing file is not an error.
func removeDB(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", p, err)
		}
	}
	return nil
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

// markerPath is the corruption marker next to the database at path.
func markerPath(path string) string {
	return path + ".corrupt"
}

// markCorrupt creates the corruption marker. Failing to create it only
// costs the recreate on the next Open, so it is logged and otherwise ignored.
func (s *Store) markCorrupt() {
	if err := os.WriteFile(markerPath(s.path), nil, privateMode); err != nil {
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
