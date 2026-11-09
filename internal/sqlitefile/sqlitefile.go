// Package sqlitefile opens and checks the daemon's rebuildable SQLite cache
// files, cache.db and calendar.db, which share one life cycle: the file and
// its -wal and -shm files are private to the daemon's user, a file with
// another PRAGMA user_version or one SQLite reports as corrupt is deleted
// and created again, and a Store that finds corruption later leaves a
// marker so the next Open recreates the file.
//
// Errors carry file paths, operation names and SQLite's own error, never
// row contents.
package sqlitefile

import (
	"context"
	"database/sql"
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

// PrivateMode is the mode of a cache file and the files beside it: they
// hold mail and calendars, so only the daemon's user may read them.
const PrivateMode fs.FileMode = 0o600

// Spec describes one cache file.
type Spec struct {
	// What names the cache in the error for a refused path, such as
	// "cache" or "calendar cache".
	What string
	// Name names the file in a recreate error, such as "cache.db".
	Name string
	// Version is the file's PRAGMA user_version. A file with any other
	// version is deleted and created again: the caches have no migrations.
	Version int
	// Schema creates the tables of a new file.
	Schema string
	// Classify wraps a SQLite error in the caller's sentinels, such as a
	// disk-full error. Nil leaves errors unchanged.
	Classify func(error) error
	// OnRecreate, when set, runs with the reason ("corrupt", "version" or
	// "unknown_tables", a fixed code safe to log) before Open deletes the
	// file.
	OnRecreate func(reason string)
}

// recreateError says why Open deletes the file. The reason is a fixed
// code, safe to log.
type recreateError struct {
	name, reason string
}

func (e *recreateError) Error() string {
	return e.name + " must be recreated: " + e.reason
}

// Open opens or creates the cache file at path with one connection,
// busy_timeout(5000), WAL and foreign keys. A file with another
// user_version, or one SQLite reports as corrupt or not a database, is
// closed, deleted with its -wal and -shm files, and created again. So is a
// file with a corruption marker (MarkerPath), which Open removes once the
// new file exists. The file and its -wal and -shm files are PrivateMode
// whatever the umask (MakePrivate).
func Open(ctx context.Context, path string, spec Spec) (*sql.DB, error) {
	// path goes into a file: URI, where these characters change its meaning.
	if strings.ContainsAny(path, "?#%") {
		return nil, fmt.Errorf("%s path %q must not contain '?', '#' or '%%'", spec.What, path)
	}
	marked, err := hasMarker(path)
	if err != nil {
		return nil, err
	}
	var conn *sql.DB
	if marked {
		err = &recreateError{name: spec.Name, reason: "corrupt"}
	} else {
		conn, err = spec.openFile(ctx, path)
	}
	if re, ok := errors.AsType[*recreateError](err); ok {
		if spec.OnRecreate != nil {
			spec.OnRecreate(re.reason)
		}
		if err := RemoveDB(path); err != nil {
			return nil, err
		}
		conn, err = spec.openFile(ctx, path)
	}
	if err != nil {
		return nil, err
	}
	if marked {
		if err := os.Remove(MarkerPath(path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			_ = conn.Close() // the marker error is the one to report
			return nil, fmt.Errorf("removing the corruption marker: %w", err)
		}
	}
	return conn, nil
}

// MarkerPath is the corruption marker next to the database at path.
func MarkerPath(path string) string {
	return path + ".corrupt"
}

// MarkCorrupt leaves the corruption marker for path, so the next Open
// recreates the file. A Store calls it when an operation finds corruption
// that Open's checks cannot see.
func MarkCorrupt(path string) error {
	return os.WriteFile(MarkerPath(path), nil, PrivateMode)
}

// hasMarker reports whether a Store left the corruption marker for path.
func hasMarker(path string) (bool, error) {
	_, err := os.Stat(MarkerPath(path))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("checking the corruption marker: %w", err)
	}
}

// IsCorrupt reports whether err carries a SQLite error that reports the
// file malformed or not a database (SQLITE_CORRUPT, SQLITE_NOTADB). The
// driver turns extended result codes on, so the primary code is the low
// byte.
func IsCorrupt(err error) bool {
	se, ok := errors.AsType[*sqlite.Error](err)
	if !ok {
		return false
	}
	switch se.Code() & 0xff {
	case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
		return true
	default:
		return false
	}
}

// MakePrivate creates the file at path with PrivateMode when it is absent,
// before SQLite opens it, and sets PrivateMode on the file, its -wal and
// -shm files and the corruption marker when any of them is wider. SQLite
// creates its -wal and -shm files with the database file's mode, so new
// ones are private too. The directory's mode is left as it is: an
// existing directory is the operator's choice.
func MakePrivate(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, PrivateMode)
	switch {
	case err == nil:
		if err := f.Close(); err != nil {
			return fmt.Errorf("creating %s: %w", path, err)
		}
	case !errors.Is(err, fs.ErrExist):
		return fmt.Errorf("creating %s: %w", path, err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm", MarkerPath(path)} {
		info, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("checking the mode of %s: %w", p, err)
		}
		if info.Mode().Perm()&^PrivateMode == 0 {
			continue
		}
		if err := os.Chmod(p, PrivateMode); err != nil {
			return fmt.Errorf("making %s private: %w", p, err)
		}
	}
	return nil
}

// RemoveDB deletes the database file and its -wal and -shm files. The
// connection must be closed first, so this works where open files cannot
// be removed. A missing file is not an error.
func RemoveDB(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", p, err)
		}
	}
	return nil
}

// openFile makes path private, opens it with one connection and prepares
// its schema.
func (spec Spec) openFile(ctx context.Context, path string) (*sql.DB, error) {
	if err := MakePrivate(path); err != nil {
		return nil, err
	}
	params := url.Values{"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)"}}
	conn, err := sql.Open("sqlite", "file:"+path+"?"+params.Encode())
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	conn.SetMaxOpenConns(1)
	if err := spec.prepare(ctx, conn); err != nil {
		_ = conn.Close() // the prepare error is the one to report
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return conn, nil
}

// prepare checks the schema version and creates the schema in a new file.
// It returns a *recreateError when the file must be deleted first.
func (spec Spec) prepare(ctx context.Context, conn *sql.DB) error {
	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return spec.openErr("reading user_version", err)
	}
	if version == spec.Version {
		return nil
	}
	if version != 0 {
		return &recreateError{name: spec.Name, reason: "version"}
	}
	var objects int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(&objects); err != nil {
		return spec.openErr("reading the schema", err)
	}
	if objects != 0 {
		return &recreateError{name: spec.Name, reason: "unknown_tables"}
	}
	return spec.create(ctx, conn)
}

// openErr turns an error from checking an existing file into a
// *recreateError when SQLite reports the file corrupt, and otherwise names
// op and keeps the caller's classification, so a disk-full error survives.
func (spec Spec) openErr(op string, err error) error {
	if IsCorrupt(err) {
		return &recreateError{name: spec.Name, reason: "corrupt"}
	}
	return fmt.Errorf("%s: %w", op, spec.classify(err))
}

// create sets auto_vacuum and creates the schema in an empty file.
func (spec Spec) create(ctx context.Context, conn *sql.DB) error {
	// journal_mode(WAL) has already written the file header, so auto_vacuum
	// takes effect only through a VACUUM, which is instant on an empty file.
	if _, err := conn.ExecContext(ctx, "PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
		return fmt.Errorf("setting auto_vacuum: %w", spec.classify(err))
	}
	if _, err := conn.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("applying auto_vacuum: %w", spec.classify(err))
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("creating the schema: %w", spec.classify(err))
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit; an earlier error is the one to report
	if _, err := tx.ExecContext(ctx, spec.Schema); err != nil {
		return fmt.Errorf("creating the schema: %w", spec.classify(err))
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(spec.Version)); err != nil {
		return fmt.Errorf("setting user_version: %w", spec.classify(err))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("creating the schema: %w", spec.classify(err))
	}
	return nil
}

// classify applies spec.Classify, or returns err unchanged without one.
func (spec Spec) classify(err error) error {
	if spec.Classify == nil {
		return err
	}
	return spec.Classify(err)
}
