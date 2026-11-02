package daemon

import (
	"testing"

	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/limits"
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
func TestWatchBudget(t *testing.T) {
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
			if got := watchBudget(pool, acct, stored, tt.watching); got != tt.want {
				t.Errorf("watchBudget(%d, %s, cap %d, watching %v) = %d, want %d", pool, tt.account, tt.stored, tt.watching, got, tt.want)
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
