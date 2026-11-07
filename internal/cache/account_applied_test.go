package cache

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
)

// waitClosed fails the test unless ch closes within acctWait.
func waitClosed(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(acctWait):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// isClosed reports whether ch is closed now, without waiting.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Applied is -1 until the builder opens its group, then the group's
// acknowledged ID, and it follows every acknowledgement after that.
// AppliedChanged closes on each move and is replaced by a fresh channel.
func TestAccountAppliedFollowsTheBuilder(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.pass("INBOX") // the baseline of an empty folder
	if got := r.a.Applied(); got != -1 {
		t.Fatalf("Applied before Run = %d, want -1", got)
	}
	opened := r.a.AppliedChanged()
	entered, release := make(chan struct{}), make(chan struct{})
	releaseHook := sync.OnceFunc(func() { close(release) })
	var atHook atomic.Int64
	r.a.afterGroup = func() {
		atHook.Store(r.a.Applied())
		close(entered)
		<-release
	}
	r.start()
	t.Cleanup(releaseHook) // runs before the rig's stop, so a failed test never leaves the hook blocked
	waitClosed(t, "the builder to open its group", entered)
	if got := atHook.Load(); got != -1 {
		t.Errorf("Applied while the group opens = %d, want -1", got)
	}
	if isClosed(opened) {
		t.Fatal("AppliedChanged closed before the builder opened its group")
	}
	releaseHook()
	waitClosed(t, "AppliedChanged after the group opened", opened)
	if got, tail := r.a.Applied(), r.tail(); got != tail {
		t.Errorf("Applied after the open = %d, want the tail %d the group opened at", got, tail)
	}

	for _, name := range []string{"one", "two"} {
		moved := r.a.AppliedChanged()
		if isClosed(moved) {
			t.Fatalf("before message %s: AppliedChanged is already closed", name)
		}
		before := r.a.Applied()
		r.add("INBOX", name)
		waitClosed(t, "AppliedChanged after message "+name, moved)
		r.waitApplied()
		got := r.a.Applied()
		if got <= before {
			t.Errorf("after message %s: Applied = %d, want past %d", name, got, before)
		}
		if acked := r.acked(); got != acked {
			t.Errorf("after message %s: Applied = %d, want the group's acknowledged ID %d", name, got, acked)
		}
		if next := r.a.AppliedChanged(); next == moved || isClosed(next) {
			t.Errorf("after message %s: AppliedChanged was not replaced by an open channel", name)
		}
	}
}

// setApplied wakes waiters only when the position moves.
func TestSetAppliedSignalsOnlyMoves(t *testing.T) {
	t.Parallel()
	a := NewAccount(AccountOptions{Account: config.ResolvedAccount{Name: "acme"}})
	steps := []struct {
		n      int64
		signal bool
	}{
		{n: 5, signal: true},
		{n: 5, signal: false},
		{n: 9, signal: true},
		{n: 9, signal: false},
		{n: 12, signal: true},
	}
	for _, st := range steps {
		ch := a.AppliedChanged()
		a.setApplied(st.n)
		if got := isClosed(ch); got != st.signal {
			t.Errorf("setApplied(%d): signalled %v, want %v", st.n, got, st.signal)
		}
		if got := a.Applied(); got != st.n {
			t.Errorf("setApplied(%d): Applied = %d", st.n, got)
		}
	}
}

// PrefetchesBodies is true only for an enabled cache with a body window,
// read from the settings at each call.
func TestAccountPrefetchesBodies(t *testing.T) {
	t.Parallel()
	const window = 90 * 24 * time.Hour
	tests := []struct {
		name    string
		enabled bool
		window  time.Duration
		want    bool
	}{
		{name: "enabled with a window", enabled: true, window: window, want: true},
		{name: "prefetch_bodies 0", enabled: true, window: 0, want: false},
		{name: "cache disabled", enabled: false, window: window, want: false},
		{name: "disabled without a window", enabled: false, window: 0, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			set := acctSettings()
			set.Enabled, set.PrefetchBodies = tt.enabled, tt.window
			a := NewAccount(AccountOptions{
				Account:  config.ResolvedAccount{Name: "acme"},
				Settings: func() config.CacheSettings { return set },
			})
			if got := a.PrefetchesBodies(); got != tt.want {
				t.Errorf("PrefetchesBodies = %v, want %v", got, tt.want)
			}
		})
	}
}
