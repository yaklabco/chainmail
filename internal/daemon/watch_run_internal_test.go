package daemon

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/limits"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// budgetConfig has one account per budget case: the preset's cap with one,
// two, three or no watchers, sync off, configured pools, and Gmail.
const budgetConfig = `
[accounts.host]
provider = "hostinger"
address = "host@example.com"

[accounts.two]
provider = "hostinger"
address = "two@example.com"
watch = ["INBOX", "Sent"]

[accounts.three]
provider = "hostinger"
address = "three@example.com"
watch = ["INBOX", "Sent", "Archive"]

[accounts.none]
provider = "hostinger"
address = "none@example.com"
watch = []

[accounts.off]
provider = "hostinger"
address = "off@example.com"
sync = { enabled = false }

[accounts.fixed]
provider = "hostinger"
address = "fixed@example.com"
pool = 4

[accounts.gmail]
provider = "gmail"
address = "me@gmail.example"
auth = "password"

[accounts.gbig]
provider = "gmail"
address = "big@gmail.example"
auth = "password"
pool = 10
`

// The pool gets what the account's cap leaves after its watchers: the
// probed cap when one is stored, else the preset's MaxConnections. It never
// grows, never drops below 1, and is left alone when nothing watches.
func TestPoolAfterWatchers(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(budgetConfig))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		account  string
		stored   int // probed cap; 0 for none
		watching bool
		want     int
	}{
		{name: "hostinger cap 3 with 1 watcher", account: "host", watching: true, want: 2},
		{name: "no supervisors leaves the pool", account: "host", want: 3},
		{name: "sync disabled has no watchers", account: "off", watching: true, want: 3},
		{name: "an empty watch list has no watchers", account: "none", watching: true, want: 3},
		{name: "two watchers leave 1", account: "two", watching: true, want: 1},
		{name: "never below 1", account: "three", watching: true, want: 1},
		{name: "a configured pool shrinks too", account: "fixed", watching: true, want: 2},
		{name: "the probed cap replaces the preset's", account: "fixed", stored: 6, watching: true, want: 4},
		{name: "the probed cap shrinks a configured pool", account: "fixed", stored: 4, watching: true, want: 3},
		{name: "gmail preset default fits under cap 10", account: "gmail", watching: true, want: 6},
		{name: "gmail probed cap 10", account: "gmail", stored: 10, watching: true, want: 8},
		{name: "gmail configured pool 10 under preset cap 10", account: "gbig", watching: true, want: 9},
		{name: "gmail configured pool 10 under probed cap 10", account: "gbig", stored: 10, watching: true, want: 9},
		{name: "gmail probed cap 1 floors at 1", account: "gmail", stored: 1, watching: true, want: 1},
		{name: "gmail without supervisors", account: "gbig", stored: 10, want: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			acct, err := cfg.Account(tt.account)
			if err != nil {
				t.Fatal(err)
			}
			var stored *limits.Limit
			if tt.stored > 0 {
				stored = &limits.Limit{Account: tt.account, IMAPCap: tt.stored}
			}
			pool := limits.PoolSize(acct.Pool, stored, acct.Provider.DefaultPool)
			watchers := 0
			if tt.watching {
				watchers = acct.WatchCount()
			}
			if got := poolAfterWatchers(pool, acct, stored, watchers); got != tt.want {
				t.Errorf("poolAfterWatchers(%d, %s, cap %d, %d watchers) = %d, want %d", pool, tt.account, tt.stored, watchers, got, tt.want)
			}
		})
	}
}

// A store built on an account's first use is sized to leave room for the
// account's watchers when the daemon watches.
func TestStorePoolLeavesRoomForWatchers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		account string
		stored  int // 0 for none
		want    int
	}{
		{name: "hostinger preset", account: "host", want: 2},
		{name: "configured pool over the preset cap", account: "fixed", want: 2},
		{name: "configured pool under a probed cap", account: "fixed", stored: 6, want: 4},
		{name: "gmail preset", account: "gmail", want: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Cap 0 keeps the background probe from storing anything.
			s, log := newLimitsServerWatch(t, limitsConfig, probeable(&capServer{cap: 0}), true)
			if tt.stored > 0 {
				if err := s.lim.store.Put(t.Context(), limits.Limit{Account: tt.account, IMAPCap: tt.stored}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.stores.get(mustAccount(t, s, tt.account)).ListFolders(t.Context()); err != nil {
				t.Fatal(err)
			}
			stores := log.of(tt.account)
			if len(stores) == 0 || stores[0].params.poolSize != tt.want {
				t.Fatalf("stores = %+v, want the first sized %d", stores, tt.want)
			}
		})
	}
}

// The pool a probe reports and installs leaves room for the watchers too.
// The probed cap is the server's whole cap, watch connections included.
func TestProbeLeavesRoomForWatchers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		watching bool
		want     int
	}{
		{name: "watching", watching: true, want: 3},
		{name: "not watching", want: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, log := newLimitsServerWatch(t, limitsConfig, probeable(&capServer{cap: 4}), tt.watching)
			got := probeLimits(t, s, "fixed").GetLimits()
			if len(got) != 1 || got[0].GetImapCap() != 4 || got[0].GetPoolSize() != uint32(tt.want) {
				t.Fatalf("limits = %v, want cap 4 and pool %d", got, tt.want)
			}
			stores := log.of("fixed")
			if len(stores) == 0 || stores[len(stores)-1].params.poolSize != tt.want {
				t.Errorf("stores = %+v, want the last sized %d", stores, tt.want)
			}
		})
	}
}

// watchRunConfig has two synced accounts and one with sync off.
const watchRunConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"

[accounts.gm]
provider = "gmail"
address = "me@gmail.example"
auth = "password"

[accounts.off]
provider = "hostinger"
address = "off@example.com"
sync = { enabled = false }
`

// supervised returns the accounts with a supervisor, in start order.
func supervised(s *Server) []string {
	s.supervisors.mu.Lock()
	defer s.supervisors.mu.Unlock()
	return slices.Clone(s.supervisors.accounts)
}

// startWatch runs one supervisor per account with sync enabled, in name
// order, and none with DisableWatch. Once stopWatch has run it starts no
// more, as drain needs.
func TestStartWatchSupervisesSyncedAccounts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		disable bool
		want    []string
	}{
		{name: "every account with sync enabled", want: []string{"gm", "host"}},
		{name: "none with DisableWatch", disable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _ := newTestServerOptions(t, watchRunConfig, Options{
				DisableWatch: tt.disable,
				NewStore:     func(a config.ResolvedAccount) mailstore.Store { return fake.New(a.Name, a.Provider.Gmail) },
			})
			s.startWatch(t.Context())
			got := supervised(s)
			s.stopWatch()
			if !slices.Equal(got, tt.want) {
				t.Errorf("supervised accounts = %v, want %v", got, tt.want)
			}
			s.startWatch(t.Context())
			if again := supervised(s); !slices.Equal(again, got) {
				t.Errorf("after stopWatch, startWatch supervised %v, want %v unchanged", again, got)
			}
		})
	}
}

// Each supervisor gets the daemon's engine, log, state and sweep floor
// (5 minutes unless Options.WatchSweepFloor shortens it).
func TestSupervisorOptionsWiring(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		set, want time.Duration
	}{
		{name: "zero means 5 minutes", want: watch.ExpungeSweepFloor},
		{name: "tests shorten it", set: 10 * time.Millisecond, want: 10 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _ := newTestServerOptions(t, watchRunConfig, Options{WatchSweepFloor: tt.set})
			acct, err := s.opts.Config.Account("host")
			if err != nil {
				t.Fatal(err)
			}
			o := s.supervisorOptions(acct, s.opts.Config.WatchSettings())
			if o.SweepFloor != tt.want {
				t.Errorf("SweepFloor = %v, want %v", o.SweepFloor, tt.want)
			}
			if o.Account.Name != "host" || o.Engine != s.engine || o.Log != s.watchLog || o.State != s.watchState {
				t.Errorf("options = %+v, want host with the daemon's engine, log and state", o)
			}
			if o.Store == nil || o.AuthRequired == nil || o.Clock != nil || o.Logger != s.opts.Logger {
				t.Errorf("options = %+v, want Store, AuthRequired and the daemon's logger set, and no Clock, which means the system clock", o)
			}
		})
	}
}

// startBackground trims the event log every trimInterval: an event older
// than the retention window goes.
func TestStartBackgroundTrimsTheEventLog(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	s.trimInterval = time.Millisecond
	retention := s.opts.Config.WatchSettings().Retention
	old := s.opts.Now().Add(-retention - time.Hour).UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	if _, err := stateDB.ExecContext(t.Context(),
		"INSERT INTO events (at, account, folder, kind) VALUES (?, 'host', 'INBOX', 'expunged')", old); err != nil {
		t.Fatal(err)
	}
	stop := s.startBackground(t.Context())
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := stateDB.QueryRowContext(t.Context(), "SELECT count(*) FROM events").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the expired event is still there after 5s (%d events)", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A refusal while the account watches records the connections the server
// accepted, pool and watchers together, so the store rebuilt from it keeps
// pool plus watchers within that, a configured pool included.
func TestRefusalLeavesRoomForWatchers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		account  string
		newMax   int // the pool after the refusal shrank it
		wantCap  int
		wantPool int
	}{
		{name: "configured pool shrunk to 1", account: "fixed", newMax: 1, wantCap: 2, wantPool: 1},
		{name: "configured pool shrunk to 3", account: "fixed", newMax: 3, wantCap: 4, wantPool: 3},
		{name: "preset pool shrunk to 2", account: "host", newMax: 2, wantCap: 3, wantPool: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Cap 0 keeps the background probe from storing anything.
			s, log := newLimitsServerWatch(t, limitsConfig, probeable(&capServer{cap: 0}), true)
			acct := mustAccount(t, s, tt.account)
			if _, err := s.stores.get(acct).ListFolders(t.Context()); err != nil {
				t.Fatal(err)
			}
			s.lim.wait()
			log.of(tt.account)[0].params.onConnLimit(tt.newMax)
			l, ok := storedLimit(t, s, tt.account)
			if !ok || l.IMAPCap != tt.wantCap {
				t.Errorf("stored = %+v (ok %v), want cap %d", l, ok, tt.wantCap)
			}
			// The server accepted the shrunk pool and the watchers.
			watchers := acct.WatchCount()
			observed := tt.newMax + watchers
			pool := s.storeParams(acct).poolSize
			if pool != tt.wantPool || pool+watchers > observed {
				t.Errorf("rebuilt pool = %d with %d watchers, want %d, within the %d connections the server accepted", pool, watchers, tt.wantPool, observed)
			}
		})
	}
}

// The event log logs its failed trims through the daemon's logger.
func TestEventLogLogsThroughTheDaemonLogger(t *testing.T) {
	t.Parallel()
	var logs lockedBuffer
	s, _ := newTestServerOptions(t, watchRunConfig, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { s.watchLog.Run(ctx, time.Hour, 0, 1000) }) // a zero retention fails every trim
	defer func() {
		cancel()
		wg.Wait()
	}()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logs.String(), "trimming the event log failed"); {
		if time.Now().After(deadline) {
			t.Fatalf("no trim failure in the daemon's log: %q", logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// watcherConns counts the account's watchers, or none when the daemon runs
// no supervisors, for both the pool size and the cap a refusal records.
func TestWatcherConns(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		account      string
		disableWatch bool
		want         int
	}{
		{account: "two", want: 2},
		{account: "two", disableWatch: true, want: 0},
		{account: "off", want: 0},
		{account: "gmail", want: 1},
	} {
		s, _ := newTestServerOptions(t, budgetConfig, Options{DisableWatch: tt.disableWatch})
		acct, err := s.opts.Config.Account(tt.account)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.watcherConns(acct); got != tt.want {
			t.Errorf("watcherConns(%s) with DisableWatch %v = %d, want %d", tt.account, tt.disableWatch, got, tt.want)
		}
	}
}
