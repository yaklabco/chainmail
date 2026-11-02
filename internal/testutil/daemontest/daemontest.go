// Package daemontest runs an in-process chainmail daemon for tests.
package daemontest

import (
	"context"
	"crypto/x509"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/client"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/daemon"
	"github.com/jamesainslie/chainmail/internal/db"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/vault"
)

// Options tune the harness. The zero value works.
type Options struct {
	// RootCAs is trusted for IMAP and SMTP. Pass the tlstest CA's Pool.
	RootCAs *x509.CertPool
	// AuthOptions are passed through to the auth manager.
	AuthOptions []auth.Option
	// Authorizer replaces the default, which allows only the current user.
	Authorizer *daemon.Authorizer
	// NewStore builds each account's mail store. Nil means IMAP.
	NewStore func(config.ResolvedAccount) mailstore.Store
	// Now replaces the daemon's clock.
	Now func() time.Time
	// OutcomeSearchInterval replaces the wait between the tries of the
	// Sent search for a send whose outcome is unknown.
	OutcomeSearchInterval time.Duration
	// Logger receives the daemon's logs. Nil discards them.
	Logger *slog.Logger
	// WatchHeartbeat replaces the 30 second heartbeat of Subscribe streams.
	WatchHeartbeat time.Duration
	// Watch starts the watch supervisors. It is off by default, so tests
	// that count mail store calls see no sync traffic.
	Watch bool
	// WatchSweepFloor replaces the 5 minute floor between expunge sweeps
	// after a server EXPUNGE. Zero keeps 5 minutes.
	WatchSweepFloor time.Duration
	// Secrets are stored in the vault before the daemon starts, by secret
	// name such as "accounts/host". A watching test sets the account's
	// password, so its supervisor does not park the account as needing
	// authentication before the test could set it.
	Secrets map[string]string
}

// Harness is a running daemon and a client connected to it.
type Harness struct {
	Client *client.Client
	Vault  *vault.Vault
	DB     *sql.DB
	Socket string

	stop func() error
}

// AuditRow is one row of the audit table, as text.
type AuditRow struct {
	Client  string
	Account string
	Action  string
	Targets string
	Result  string
	Reason  string
}

// Start runs a dev-mode daemon with configTOML until the test ends.
func Start(t testing.TB, configTOML string, opts Options) *Harness {
	t.Helper()
	cfg, err := config.Parse([]byte(configTOML))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	// Unix socket paths are limited to 108 bytes, so avoid t.TempDir's long paths.
	dir, err := os.MkdirTemp("", "cmd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open(filepath.Join(dir, "vault.age"), id)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range opts.Secrets {
		if err := v.Set(name, []byte(value)); err != nil {
			t.Fatalf("secret %s: %v", name, err)
		}
	}
	stateDB, err := db.OpenState(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stateDB.Close() })
	socket := filepath.Join(dir, "d.sock")
	ln, err := daemon.Listen(t.Context(), socket, -1)
	if err != nil {
		t.Fatal(err)
	}
	authz := opts.Authorizer
	if authz == nil {
		authz = daemon.NewAuthorizer(uint32(os.Getuid()), -1)
	}
	srv, err := daemon.New(daemon.Options{
		Config:                cfg,
		Vault:                 v,
		StateDB:               stateDB,
		Listener:              ln,
		Authorizer:            authz,
		DevMode:               true,
		RootCAs:               opts.RootCAs,
		AuthOptions:           opts.AuthOptions,
		NewStore:              opts.NewStore,
		Now:                   opts.Now,
		StateDir:              dir,
		OutcomeSearchInterval: opts.OutcomeSearchInterval,
		Logger:                opts.Logger,
		WatchHeartbeat:        opts.WatchHeartbeat,
		DisableWatch:          !opts.Watch,
		WatchSweepFloor:       opts.WatchSweepFloor,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	c := client.New(socket, "test")
	stop := sync.OnceValue(func() error {
		// Idle client connections would hold up the graceful shutdown.
		c.Close()
		cancel()
		return <-done
	})
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("daemon: %v", err)
		}
	})
	return &Harness{Client: c, Vault: v, DB: stateDB, Socket: socket, stop: stop}
}

// Stop shuts the daemon down as a signal would and returns what Serve
// returned. The database stays open until the test ends, so tests can read
// the audit rows afterwards. The test's cleanup calls Stop too, and later
// calls return the first result.
func (h *Harness) Stop() error { return h.stop() }

// AuditRows returns every audit row in insertion order.
func (h *Harness) AuditRows(t testing.TB) []AuditRow {
	t.Helper()
	rows, err := h.DB.QueryContext(t.Context(), "SELECT client, account, action, targets, result, reason FROM audit ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports any iteration error
	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.Client, &r.Account, &r.Action, &r.Targets, &r.Result, &r.Reason); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
