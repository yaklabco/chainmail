package cache

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// workBackoff starts at 1s, doubles, and caps at 5 minutes.
func TestWorkBackoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		n    int
		want time.Duration
	}{
		{n: -1, want: time.Second},
		{n: 0, want: time.Second},
		{n: 1, want: 2 * time.Second},
		{n: 8, want: 256 * time.Second},
		{n: 9, want: 5 * time.Minute},
		{n: 1000, want: 5 * time.Minute},
	}
	for _, tt := range tests {
		if got := workBackoff(tt.n); got != tt.want {
			t.Errorf("workBackoff(%d) = %v, want %v", tt.n, got, tt.want)
		}
	}
}

// untilUTCMidnight measures to the next 00:00 UTC whatever the time's zone.
func TestUntilUTCMidnight(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		{name: "an hour before", now: time.Date(2026, 11, 2, 23, 0, 0, 0, time.UTC), want: time.Hour},
		{name: "at midnight a whole day", now: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), want: 24 * time.Hour},
		{name: "another zone", now: time.Date(2026, 11, 2, 20, 0, 0, 0, time.FixedZone("EST", -5*3600)), want: 23 * time.Hour},
		{name: "month end", now: time.Date(2026, 11, 30, 23, 59, 0, 0, time.UTC), want: time.Minute},
	}
	for _, tt := range tests {
		if got := untilUTCMidnight(tt.now); got != tt.want {
			t.Errorf("%s: untilUTCMidnight(%v) = %v, want %v", tt.name, tt.now, got, tt.want)
		}
	}
}

// reasonCode maps failures to api reason codes, never to error text.
func TestReasonCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "canceled", err: context.Canceled, want: api.ReasonTimeout},
		{name: "deadline", err: fmt.Errorf("fetching: %w", context.DeadlineExceeded), want: api.ReasonTimeout},
		{name: "auth required", err: auth.ErrAuthRequired, want: api.ReasonAuthRequired},
		{name: "rejected", err: auth.ErrRejected, want: api.ReasonAuthRejected},
		{name: "stale", err: fmt.Errorf("%w: INBOX changed", mailstore.ErrStaleRef), want: api.ReasonStaleRef},
		{name: "missing folder", err: &mailstore.FolderNotFoundError{Name: "Gone", Requested: "Gone"}, want: api.ReasonNotFound},
		{name: "unavailable", err: fmt.Errorf("dial: %w", mailstore.ErrUnavailable), want: api.ReasonServerUnavailable},
		{name: "busy", err: mailstore.ErrBusy, want: api.ReasonServerUnavailable},
		{name: "malformed", err: fmt.Errorf("fetching: %w", mailstore.ErrMalformed), want: api.ReasonMalformedResponse},
		{name: "disk full", err: fmt.Errorf("writing rows: %w", ErrDiskFull), want: api.ReasonInternal},
		{name: "anything else", err: errors.New("boom"), want: api.ReasonInternal},
	}
	for _, tt := range tests {
		if got := reasonCode(tt.err); got != tt.want {
			t.Errorf("%s: reasonCode = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// Folders lists the account's sync_folders rows that have a UIDVALIDITY,
// in State's order, and nothing of other accounts.
func TestAccountFolders(t *testing.T) {
	t.Parallel()
	r := newAcctRig(t, fake.New("acme", false), acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.putFolder(watch.FolderRow{Account: "acme", Folder: "INBOX", UIDValidity: 7, UIDNext: 3, Mode: watch.ModeIdle, State: watch.StateWatching})
	r.putFolder(watch.FolderRow{Account: "acme", Folder: "Archive", UIDValidity: 9, UIDNext: 1, Mode: watch.ModePoll, State: watch.StatePolling})
	r.setState("Sent", watch.StateError) // a row no pass has written has no UIDVALIDITY
	r.putFolder(watch.FolderRow{Account: "other", Folder: "INBOX", UIDValidity: 3, UIDNext: 1})
	got, err := r.a.Folders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Archive", "INBOX"}; !slices.Equal(got, want) {
		t.Errorf("Folders = %q, want %q", got, want)
	}
}

// View binds the account to the server store it falls back on.
func TestAccountView(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	v := r.a.View(fs)
	if v.a != r.a || v.server != fs {
		t.Errorf("View = {%p, %v}, want {%p, the fake}", v.a, v.server, r.a)
	}
}

// With the cache disabled Run returns at once and opens no group.
func TestAccountRunDisabledReturnsAtOnce(t *testing.T) {
	t.Parallel()
	r := newAcctRig(t, fake.New("acme", false), acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.setSettings(func(s *config.CacheSettings) { s.Enabled = false })
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.a.Run(t.Context())
	}()
	select {
	case <-done:
	case <-time.After(acctWait):
		t.Fatal("Run did not return with the cache disabled")
	}
	groups, err := r.log.Groups(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(groups, func(g watch.GroupInfo) bool { return g.Name == watch.CacheGroup("acme") }) {
		t.Error("a disabled account opened its group")
	}
}

// TestAccountReportsWorkerErrors checks that a worker's failed write
// reaches AccountOptions.OnError, so the daemon turns the cache off when
// the disk is full (spec section 8) without waiting for its hourly probe.
func TestAccountReportsWorkerErrors(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	for i := range 50 {
		m := acctMsg("INBOX", fmt.Sprintf("m%d", i))
		m.Subject = strings.Repeat("long subject ", 200)
		fs.Add(m)
	}
	r.pass("INBOX")
	got := make(chan error, 64)
	r.onError = func(err error) {
		select {
		case got <- err:
		default:
		}
	}
	r.renew()
	var pages int64
	if err := r.store.db.QueryRowContext(t.Context(), "PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	// max_page_count stands in for a full disk: the backfill's headers need
	// pages the file may not grow by. The Store has one connection.
	if _, err := r.store.db.ExecContext(t.Context(), "PRAGMA max_page_count = "+strconv.FormatInt(pages, 10)); err != nil {
		t.Fatal(err)
	}
	r.start()
	deadline := time.After(acctWait)
	for {
		select {
		case err := <-got:
			if errors.Is(err, ErrDiskFull) {
				return
			}
		case <-deadline:
			t.Fatal("no worker reported ErrDiskFull to OnError")
		}
	}
}
