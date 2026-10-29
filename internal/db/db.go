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
// migrations. Files are named NNNN_description.sql, numbered from 0001 with no gaps.
func Open(ctx context.Context, filePath string, migrations fs.FS) (*sql.DB, error) {
	params := url.Values{"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)"}}
	conn, err := sql.Open("sqlite", "file:"+filePath+"?"+params.Encode())
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", filePath, err)
	}
	// One connection serializes writers, so SQLite never returns SQLITE_BUSY to us.
	conn.SetMaxOpenConns(1)
	if err := migrate(ctx, conn, migrations); err != nil {
		_ = conn.Close() // the migration error is the one to report
		return nil, fmt.Errorf("migrating %s: %w", filePath, err)
	}
	return conn, nil
}

func migrate(ctx context.Context, conn *sql.DB, migrations fs.FS) error {
	names, err := fs.Glob(migrations, "*.sql")
	if err != nil {
		return fmt.Errorf("listing migrations: %w", err)
	}
	slices.Sort(names)
	var current int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}
	if current > len(names) {
		return fmt.Errorf("schema version %d is newer than this binary (%d)", current, len(names))
	}
	for i, name := range names {
		want := i + 1
		prefix, _, _ := strings.Cut(name, "_")
		if n, convErr := strconv.Atoi(prefix); convErr != nil || n != want {
			return fmt.Errorf("migration %s: want number %04d", name, want)
		}
		if want <= current {
			continue
		}
		body, err := fs.ReadFile(migrations, name)
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		if err := apply(ctx, conn, want, string(body)); err != nil {
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
