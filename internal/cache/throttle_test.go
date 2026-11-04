package cache

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
)

// acctThrottledAt is 15:00 UTC on the rig clock's first day, nine hours
// before the pause a throttle records ends.
const acctThrottledAt = 6 * time.Hour

func (r *acctRig) cacheState() string {
	r.t.Helper()
	st, err := r.a.Status(r.t.Context())
	if err != nil {
		r.t.Fatal(err)
	}
	return st.State
}

// A backfill fetch the server refuses for a quota or bandwidth limit
// pauses the account's background work until 00:00 UTC: the backfill
// fetches nothing more until then, status reports budget_exhausted, and
// the builder keeps applying new mail. On the next day the backfill
// finishes.
func TestThrottledBackfillPausesUntilUTCMidnight(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	old := fs.Add(acctMsg("INBOX", "old"))
	r.pass("INBOX")
	fs.Fail("FetchHeaders", fmt.Errorf("%w: acme", mailstore.ErrThrottled))
	r.clock.Advance(acctThrottledAt)
	r.start()
	r.clock.expectAfter(t, 9*time.Hour) // the backfill waits for 00:00 UTC
	if n := fs.Calls("FetchHeaders"); n != 1 {
		t.Errorf("FetchHeaders calls = %d, want 1", n)
	}
	if got := r.cacheState(); got != StateBudgetExhausted {
		t.Errorf("State while throttled = %q, want %q", got, StateBudgetExhausted)
	}

	fs.Fail("FetchHeaders", nil)
	newer := r.add("INBOX", "newer")[0]
	r.waitApplied()
	if _, ok := r.row(newer); !ok {
		t.Error("the builder did not apply new mail while background work was throttled")
	}
	if _, ok := r.row(old); ok {
		t.Error("the backfill fetched while throttled")
	}
	if got := r.cacheState(); got != StateBudgetExhausted {
		t.Errorf("State after the builder's fetch = %q, want %q", got, StateBudgetExhausted)
	}

	r.clock.Advance(9 * time.Hour)
	r.waitBackfilled("INBOX")
	if _, ok := r.row(old); !ok {
		t.Error("the backfill did not fetch the old header on the next day")
	}
	if got := r.cacheState(); got != StateReady {
		t.Errorf("State on the next day = %q, want %q", got, StateReady)
	}
}

// A body fetch the server refuses for a quota or bandwidth limit pauses
// the account the same way: status reports budget_exhausted, the
// backfill's admission waits for 00:00 UTC too, and the body arrives on
// the next day.
func TestThrottledPrefetchPausesTheAccountUntilUTCMidnight(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.setSettings(func(s *config.CacheSettings) { s.PrefetchBodies = acctWindow })
	m := fs.Add(acctMsg("INBOX", "m"))
	r.pass("INBOX")
	fs.Fail("FetchMessages", fmt.Errorf("%w: acme", mailstore.ErrThrottled))
	r.clock.Advance(acctThrottledAt)
	r.start()
	r.waitBackfilled("INBOX")
	r.clock.expectAfter(t, 9*time.Hour) // the prefetcher waits for 00:00 UTC
	if n := fs.Calls("FetchMessages"); n != 1 {
		t.Errorf("FetchMessages calls = %d, want 1", n)
	}
	if got := r.cacheState(); got != StateBudgetExhausted {
		t.Errorf("State while throttled = %q, want %q", got, StateBudgetExhausted)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	admitted := make(chan error, 1)
	go func() {
		_, err := r.a.waitAllowance(ctx)
		admitted <- err
	}()
	r.clock.expectAfter(t, 9*time.Hour) // the backfill's admission waits too
	select {
	case err := <-admitted:
		t.Fatalf("waitAllowance returned %v while throttled", err)
	default:
	}

	fs.Fail("FetchMessages", nil)
	r.clock.Advance(9 * time.Hour)
	if err := <-admitted; err != nil {
		t.Fatalf("waitAllowance on the next day: %v", err)
	}
	r.eventually("the body on the next day", func() (bool, error) { return hasBody(t, r.store, m), nil })
	if n := fs.Calls("FetchMessages"); n != 2 {
		t.Errorf("FetchMessages calls = %d, want 2", n)
	}
}
