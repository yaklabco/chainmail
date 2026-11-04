package cache

import (
	"errors"
	"fmt"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	// ErrDiskFull means SQLite could not write cache.db because the disk, or
	// the database's page limit, is full (SQLITE_FULL).
	ErrDiskFull = errors.New("cache.db is full")
	// ErrCorrupt means SQLite reports cache.db as malformed or as not a
	// database (SQLITE_CORRUPT, SQLITE_NOTADB). Closing the Store and
	// opening the file again recreates it.
	ErrCorrupt = errors.New("cache.db is corrupt")
	// ErrRead marks a View read of cache.db that failed while serving a
	// search, as opposed to the server the View also asks. The daemon
	// answers an automatic search from the server instead.
	ErrRead = errors.New("reading the cache failed")
)

// codeErr returns the sentinel for a SQLite result code, primary or
// extended, or nil when the code is neither full nor corrupt. The driver
// turns extended result codes on, so the primary code is the low byte.
func codeErr(code int) error {
	switch code & 0xff {
	case sqlite3.SQLITE_FULL:
		return ErrDiskFull
	case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
		return ErrCorrupt
	default:
		return nil
	}
}

// classify wraps err in ErrDiskFull or ErrCorrupt when it carries the
// matching SQLite result code, and returns it unchanged otherwise.
func classify(err error) error {
	se, ok := errors.AsType[*sqlite.Error](err)
	if !ok {
		return err
	}
	sentinel := codeErr(se.Code())
	if sentinel == nil || errors.Is(err, sentinel) {
		return err
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}

// readErr marks err, a Store failure on the View's search path, as
// ErrRead, keeping its classification.
func readErr(err error) error {
	return fmt.Errorf("%w: %w", ErrRead, err)
}
