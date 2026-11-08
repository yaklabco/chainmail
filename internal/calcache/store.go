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
	"strings"

	"github.com/yaklabco/chainmail/internal/sqlitefile"
)

// SchemaVersion is calendar.db's PRAGMA user_version. A file with any other
// version is deleted and created again: the cache has no migrations.
// Version 2 added mail_invites.charset and mail_invites.announced, version
// 3 mail_invites.settled and invite_fetches, and version 4 cal_raws, which
// holds each raw object once in place of a copy on every row.
const SchemaVersion = 4

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

// Open opens or creates the cache at path. A file with another
// user_version, or one SQLite reports as corrupt or not a database, is
// closed, deleted with its -wal and -shm files, and created again; so is a
// file with a corruption marker (path + ".corrupt"), which a Store leaves
// when an operation finds corruption that Open's checks cannot see.
// Recreated reports why. The file and its -wal and -shm files are mode
// 0600 whatever the umask. internal/sqlitefile holds this life cycle,
// which cache.db shares.
func Open(ctx context.Context, path string) (*Store, error) {
	var reason string
	conn, err := sqlitefile.Open(ctx, path, sqlitefile.Spec{
		What:       "calendar cache",
		Name:       FileName,
		Version:    SchemaVersion,
		Schema:     schema,
		Classify:   classify,
		OnRecreate: func(r string) { reason = r },
	})
	if err != nil {
		return nil, err
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

// classify wraps err in errCorrupt when SQLite reports the file malformed
// or not a database, and returns it unchanged otherwise.
func classify(err error) error {
	if !sqlitefile.IsCorrupt(err) || errors.Is(err, errCorrupt) {
		return err
	}
	return fmt.Errorf("%w: %w", errCorrupt, err)
}

// fail names the operation that failed and classifies err. Every Store
// method's database error goes through it, so when err reports corruption
// it also leaves the marker that makes the next Open recreate the file.
func (s *Store) fail(op string, err error) error {
	err = classify(err)
	if errors.Is(err, errCorrupt) {
		// Failing to write the marker only costs the recreate on the next
		// Open; the error below is the one to report.
		_ = sqlitefile.MarkCorrupt(s.path)
	}
	return fmt.Errorf("%s: %w", op, err)
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
