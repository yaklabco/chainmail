// Package calcache keeps calendar.db, the daemon's local copy of each
// account's calendars: the calendars, their events (singles, series masters
// and overrides), and the invites found in mail. It lives next to cache.db
// with its own PRAGMA user_version, so neither file's schema change forces
// the other to refill. It is rebuildable: a full sync of every calendar
// fills it again, so a file with another version, or one SQLite reports as
// corrupt, is deleted and created again.
//
// Event text (summaries, locations, descriptions, names and calendar
// display names) is untrusted. No error from this package carries it:
// errors name the operation and keep only the SQLite classification.
package calcache

import (
	"context"
	"database/sql"
	_ "embed" // schema.sql
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// SchemaVersion is calendar.db's PRAGMA user_version. A file with any other
// version is deleted and created again: the cache has no migrations.
const SchemaVersion = 1

// FileName is the cache's file name inside the cache directory.
const FileName = "calendar.db"

// ErrNotFound means the calendar, event or invite is not in the cache.
var ErrNotFound = errors.New("not in the calendar cache")

// errCorrupt marks a SQLite error that reports the file malformed or not a
// database (SQLITE_CORRUPT, SQLITE_NOTADB).
var errCorrupt = errors.New("calendar.db is corrupt")

//go:embed schema.sql
var schema string

// Store reads and writes calendar.db. Nothing else issues SQL against the
// file. It holds one connection, so no method runs a query while another's
// Rows are open, and none holds a transaction across network IO.
type Store struct {
	db        *sql.DB
	path      string // the database file, for the corruption marker
	recreated string
}

// recreateError says why Open deletes the file: "corrupt", "version" or
// "unknown_tables". The reason is a fixed code, safe to log.
type recreateError struct {
	reason string
}

func (e *recreateError) Error() string {
	return "calendar.db must be recreated: " + e.reason
}

// Open opens or creates the cache at path. A file with another
// user_version, or one SQLite reports as corrupt or not a database, is
// closed, deleted with its -wal and -shm files, and created again; so is a
// file with a corruption marker (path + ".corrupt"), which a Store leaves
// when an operation finds corruption that Open's checks cannot see.
// Recreated reports why. The file and its -wal and -shm files are mode
// 0600 whatever the umask.
func Open(ctx context.Context, path string) (*Store, error) {
	// path goes into a file: URI, where these characters change its meaning.
	if strings.ContainsAny(path, "?#%") {
		return nil, fmt.Errorf("calendar cache path %q must not contain '?', '#' or '%%'", path)
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
	var reason string
	if re, ok := errors.AsType[*recreateError](err); ok {
		reason = re.reason
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
	return &Store{db: conn, path: path, recreated: reason}, nil
}

// Recreated reports why Open deleted and created the file again:
// "corrupt", "version" or "unknown_tables", or "" when it kept the file.
// The reason is a fixed code, safe to log. A recreated cache has no
// calendars, so every calendar's next sync is a first, full sync.
func (s *Store) Recreated() string {
	return s.recreated
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
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

// privateMode is the mode of calendar.db and the files beside it: they hold
// calendars, so only the daemon's user may read them.
const privateMode fs.FileMode = 0o600

// makePrivate creates the file with privateMode when it is absent, before
// SQLite opens it, and narrows the file, its -wal and -shm files and the
// corruption marker to privateMode when any of them is wider.
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
// op.
func openErr(op string, err error) error {
	err = classify(err)
	if errors.Is(err, errCorrupt) {
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
// connection must be closed first. A missing file is not an error.
func removeDB(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", p, err)
		}
	}
	return nil
}

// classify wraps err in errCorrupt when SQLite reports the file malformed
// or not a database, and returns it unchanged otherwise. The driver turns
// extended result codes on, so the primary code is the low byte.
func classify(err error) error {
	se, ok := errors.AsType[*sqlite.Error](err)
	if !ok {
		return err
	}
	switch se.Code() & 0xff {
	case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
		if errors.Is(err, errCorrupt) {
			return err
		}
		return fmt.Errorf("%w: %w", errCorrupt, err)
	default:
		return err
	}
}

// fail names the operation that failed and classifies err. Every Store
// method's database error goes through it, so when err reports corruption
// it also leaves the marker that makes the next Open recreate the file.
func (s *Store) fail(op string, err error) error {
	err = classify(err)
	if errors.Is(err, errCorrupt) {
		// Failing to write the marker only costs the recreate on the next
		// Open; the error below is the one to report.
		_ = os.WriteFile(markerPath(s.path), nil, privateMode)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// markerPath is the corruption marker next to the database at path.
func markerPath(path string) string {
	return path + ".corrupt"
}

// inTx runs fn in one transaction and commits it. Errors name op.
func (s *Store) inTx(ctx context.Context, op string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return s.fail(op, err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit; an earlier error is the one to report
	if err := fn(tx); err != nil {
		if errors.Is(err, ErrNotFound) {
			return err
		}
		return s.fail(op, err)
	}
	if err := tx.Commit(); err != nil {
		return s.fail(op, err)
	}
	return nil
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
