// Package db opens chainmail's SQLite databases and applies their migrations.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"slices"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/state/*.sql
var stateMigrations embed.FS

// OpenState opens state.db at path and brings its schema up to date.
func OpenState(ctx context.Context, path string) (*sql.DB, error) {
	sub, err := fs.Sub(stateMigrations, "migrations/state")
	if err != nil {
		return nil, fmt.Errorf("state migrations: %w", err)
	}
	return Open(ctx, path, sub)
}

// Open opens the SQLite database at filePath and applies the migrations in
// migrations. Files are named NNNN_description.sql, numbered from 0001 with no
// gaps. The whole set is validated before any migration is applied.
//
// The returned pool holds a single connection. A caller must not run another
// query while it still holds Rows open from an earlier one: the second query
// waits for the only connection, which the open Rows never release, and deadlocks.
func Open(ctx context.Context, filePath string, migrations fs.FS) (*sql.DB, error) {
	// filePath goes into a file: URI, where these characters change its meaning.
	if strings.ContainsAny(filePath, "?#%") {
		return nil, fmt.Errorf("database path %q must not contain '?', '#' or '%%'", filePath)
	}
	names, err := migrationNames(migrations)
	if err != nil {
		return nil, err
	}
	params := url.Values{"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)"}}
	conn, err := sql.Open("sqlite", "file:"+filePath+"?"+params.Encode())
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", filePath, err)
	}
	// One connection serializes writers within this process. busy_timeout covers
	// contention with other processes that open the same file.
	conn.SetMaxOpenConns(1)
	if err := migrate(ctx, conn, migrations, names); err != nil {
		_ = conn.Close() // the migration error is the one to report
		return nil, fmt.Errorf("migrating %s: %w", filePath, err)
	}
	return conn, nil
}

// migrationNames lists the migrations in order and checks that they are
// numbered 0001, 0002, ... with no gaps.
func migrationNames(migrations fs.FS) ([]string, error) {
	names, err := fs.Glob(migrations, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("listing migrations: %w", err)
	}
	slices.Sort(names)
	for i, name := range names {
		want := fmt.Sprintf("%04d", i+1)
		if prefix, _, _ := strings.Cut(name, "_"); prefix != want {
			return nil, fmt.Errorf("migration %s: want number %s", name, want)
		}
	}
	return names, nil
}

func migrate(ctx context.Context, conn *sql.DB, migrations fs.FS, names []string) error {
	var current int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}
	if current < 0 {
		return fmt.Errorf("invalid schema version %d", current)
	}
	if current > len(names) {
		return fmt.Errorf("schema version %d is newer than this binary (%d)", current, len(names))
	}
	for i, name := range names[current:] {
		version := current + i + 1
		body, err := fs.ReadFile(migrations, name)
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		if err := apply(ctx, conn, version, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

func apply(ctx context.Context, conn *sql.DB, version int, body string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning: %w", err)
	}
	if _, err := tx.ExecContext(ctx, body); err != nil {
		_ = tx.Rollback() // the exec error is the one to report
		return fmt.Errorf("applying: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(version)); err != nil {
		_ = tx.Rollback() // the exec error is the one to report
		return fmt.Errorf("setting user_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing: %w", err)
	}
	return nil
}
