package cache

import (
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// Freshness is spec 6.1 check 1 with amendment 8: every folder synced (a
// sync row with a UIDVALIDITY), watching at any age or polling within twice
// sync.poll, backfill done under the row's UIDVALIDITY, and no event of the
// account older than fresh_lag left unapplied. A sync row last synced
// before the Account started is from a previous run, so it is stale. The
// reason is the first failure in folder order; syncedAt is the oldest last
// sync.
func TestFreshness(t *testing.T) {
	t.Parallel()
	now := acctStart
	watching := func(folder string, synced time.Time) watch.FolderRow {
		return watch.FolderRow{Account: "acme", Folder: folder, UIDValidity: 7, UIDNext: 10, Mode: watch.ModeIdle, State: watch.StateWatching, LastSyncAt: synced}
	}
	polling := func(folder string, synced time.Time) watch.FolderRow {
		r := watching(folder, synced)
		r.Mode, r.State = watch.ModePoll, watch.StatePolling
		return r
	}
	inState := func(folder string, s watch.Status) watch.FolderRow {
		r := watching(folder, now)
		r.State = s
		return r
	}
	done := func(folder string) Progress {
		return Progress{Account: "acme", Folder: folder, UIDValidity: 7, BaselineUIDNext: 10, BackfillLowUID: 1, BackfillDone: true}
	}
	walking := func(folder string) Progress {
		p := done(folder)
		p.BackfillDone, p.BackfillLowUID = false, 5
		return p
	}
	oldValidity := func(folder string) Progress {
		p := done(folder)
		p.UIDValidity = 6
		return p
	}
	lag := func(account string, at time.Time) watch.Event {
		return watch.Event{At: at, Kind: watch.KindNew, Account: account, Folder: "INBOX", Ref: "cm1:lag", Message: &watch.Message{Subject: "lag"}}
	}
	day := 24 * time.Hour
	ranFrom := now.Add(-31 * day) // when the Account started, unless ranFor says otherwise
	tests := []struct {
		name       string
		rows       []watch.FolderRow
		progress   []Progress
		lag        []watch.Event // published after the builder's position
		notStarted bool          // the builder has not opened its group
		ranFor     time.Duration // how long the Account has run, when not since ranFrom
		folders    []string
		wantOK     bool
		wantReason string
		wantSynced time.Time
	}{
		{
			name: "watching at any age", rows: []watch.FolderRow{watching("INBOX", now.Add(-30*day))}, progress: []Progress{done("INBOX")},
			folders: []string{"INBOX"}, wantOK: true, wantSynced: now.Add(-30 * day),
		},
		{
			name: "polling within twice the poll", rows: []watch.FolderRow{polling("Archive", now.Add(-9*time.Minute))}, progress: []Progress{done("Archive")},
			folders: []string{"Archive"}, wantOK: true, wantSynced: now.Add(-9 * time.Minute),
		},
		{
			name: "polling at exactly twice the poll", rows: []watch.FolderRow{polling("Archive", now.Add(-10*time.Minute))}, progress: []Progress{done("Archive")},
			folders: []string{"Archive"}, wantOK: true, wantSynced: now.Add(-10 * time.Minute),
		},
		{
			name: "polling too long ago", rows: []watch.FolderRow{polling("Archive", now.Add(-10*time.Minute-time.Second))}, progress: []Progress{done("Archive")},
			folders: []string{"Archive"}, wantReason: ReasonStale, wantSynced: now.Add(-10*time.Minute - time.Second),
		},
		{
			name: "reconnecting", rows: []watch.FolderRow{inState("INBOX", watch.StateReconnecting)}, progress: []Progress{done("INBOX")},
			folders: []string{"INBOX"}, wantReason: ReasonStale, wantSynced: now,
		},
		{
			name: "baselined but not yet watching", rows: []watch.FolderRow{inState("INBOX", watch.StateBaselined)}, progress: []Progress{done("INBOX")},
			folders: []string{"INBOX"}, wantReason: ReasonStale, wantSynced: now,
		},
		{
			name: "backfill not done", rows: []watch.FolderRow{watching("INBOX", now)}, progress: []Progress{walking("INBOX")},
			folders: []string{"INBOX"}, wantReason: ReasonBackfilling, wantSynced: now,
		},
		{
			name: "no progress yet", rows: []watch.FolderRow{watching("INBOX", now)},
			folders: []string{"INBOX"}, wantReason: ReasonBackfilling, wantSynced: now,
		},
		{
			name: "progress under an old UIDVALIDITY", rows: []watch.FolderRow{watching("INBOX", now)}, progress: []Progress{oldValidity("INBOX")},
			folders: []string{"INBOX"}, wantReason: ReasonBackfilling, wantSynced: now,
		},
		{
			name: "a folder not synced", rows: []watch.FolderRow{watching("INBOX", now.Add(-time.Hour))}, progress: []Progress{done("INBOX")},
			folders: []string{"Sent", "INBOX"}, wantReason: ReasonFolderNotSynced, wantSynced: now.Add(-time.Hour),
		},
		{
			name: "a sync row without UIDVALIDITY", rows: []watch.FolderRow{{Account: "acme", Folder: "INBOX", Mode: watch.ModeIdle, State: watch.StateError}},
			folders: []string{"INBOX"}, wantReason: ReasonFolderNotSynced,
		},
		{
			name: "the first failing folder decides",
			rows: []watch.FolderRow{polling("Archive", now), inState("INBOX", watch.StateReconnecting)}, progress: []Progress{walking("Archive"), done("INBOX")},
			folders: []string{"Archive", "INBOX"}, wantReason: ReasonBackfilling, wantSynced: now,
		},
		{
			name: "the oldest last sync",
			rows: []watch.FolderRow{watching("INBOX", now.Add(-time.Hour)), watching("Archive", now.Add(-2*time.Hour))}, progress: []Progress{done("INBOX"), done("Archive")},
			folders: []string{"INBOX", "Archive"}, wantOK: true, wantSynced: now.Add(-2 * time.Hour),
		},
		{
			name: "builder lag beyond fresh_lag", rows: []watch.FolderRow{watching("INBOX", now)}, progress: []Progress{done("INBOX")},
			lag: []watch.Event{lag("acme", now.Add(-31*time.Second))}, folders: []string{"INBOX"}, wantReason: ReasonStale, wantSynced: now,
		},
		{
			name: "builder lag within fresh_lag", rows: []watch.FolderRow{watching("INBOX", now)}, progress: []Progress{done("INBOX")},
			lag: []watch.Event{lag("acme", now.Add(-29*time.Second))}, folders: []string{"INBOX"}, wantOK: true, wantSynced: now,
		},
		{
			name: "another account's old event", rows: []watch.FolderRow{watching("INBOX", now)}, progress: []Progress{done("INBOX")},
			lag: []watch.Event{lag("other", now.Add(-time.Hour))}, folders: []string{"INBOX"}, wantOK: true, wantSynced: now,
		},
		{
			name: "builder not started", rows: []watch.FolderRow{watching("INBOX", now)}, progress: []Progress{done("INBOX")},
			notStarted: true, folders: []string{"INBOX"}, wantReason: ReasonStale, wantSynced: now,
		},
		{
			name: "watching, last synced before this run", rows: []watch.FolderRow{watching("INBOX", ranFrom.Add(-time.Second))}, progress: []Progress{done("INBOX")},
			folders: []string{"INBOX"}, wantReason: ReasonStale, wantSynced: ranFrom.Add(-time.Second),
		},
		{
			name: "polling within twice the poll, last synced before this run", rows: []watch.FolderRow{polling("Archive", now.Add(-6*time.Minute))}, progress: []Progress{done("Archive")},
			ranFor: 5 * time.Minute, folders: []string{"Archive"}, wantReason: ReasonStale, wantSynced: now.Add(-6 * time.Minute),
		},
		{
			name: "watching, never synced", rows: []watch.FolderRow{watching("INBOX", time.Time{})}, progress: []Progress{done("INBOX")},
			folders: []string{"INBOX"}, wantReason: ReasonStale,
		},
		{name: "no folders", wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newAcctRig(t, fake.New("acme", false), acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX", "Archive"}))
			// The Account has run since ranFrom, or for ranFor when set, so
			// a row synced since then is from this run.
			r.a.started = ranFrom
			if tt.ranFor > 0 {
				r.a.started = now.Add(-tt.ranFor)
			}
			for _, row := range tt.rows {
				r.putFolder(row)
			}
			for _, p := range tt.progress {
				if err := r.store.PutProgress(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			}
			if !tt.notStarted {
				r.a.applied.Store(r.tail())
			}
			r.publish(tt.lag...)
			ok, reason, synced, err := r.a.Freshness(t.Context(), tt.folders)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tt.wantOK || reason != tt.wantReason || !synced.Equal(tt.wantSynced) {
				t.Errorf("Freshness = %v, %q, %v; want %v, %q, %v", ok, reason, synced, tt.wantOK, tt.wantReason, tt.wantSynced)
			}
		})
	}
}

// WindowCovers holds when the query's lower bound is inside the prefetch
// window and every folder has its window bodies.
func TestWindowCovers(t *testing.T) {
	t.Parallel()
	window := 90 * 24 * time.Hour
	start := acctStart.Add(-window)
	withWindow := func(folder string, done bool) Progress {
		return Progress{Account: "acme", Folder: folder, UIDValidity: 7, BaselineUIDNext: 10, BackfillDone: true, WindowDone: done}
	}
	inbox := []Progress{withWindow("INBOX", true)}
	tests := []struct {
		name     string
		prefetch time.Duration
		progress []Progress
		folders  []string
		since    time.Time
		want     bool
	}{
		{name: "inside the window", prefetch: window, progress: inbox, folders: []string{"INBOX"}, since: acctStart.Add(-24 * time.Hour), want: true},
		{name: "at the window's start", prefetch: window, progress: inbox, folders: []string{"INBOX"}, since: start, want: true},
		{name: "before the window", prefetch: window, progress: inbox, folders: []string{"INBOX"}, since: start.Add(-time.Second)},
		{name: "no lower bound", prefetch: window, progress: inbox, folders: []string{"INBOX"}},
		{
			name: "a folder still prefetching", prefetch: window, progress: []Progress{withWindow("INBOX", true), withWindow("Archive", false)},
			folders: []string{"INBOX", "Archive"}, since: acctStart.Add(-24 * time.Hour),
		},
		{name: "a folder not tracked", prefetch: window, progress: inbox, folders: []string{"INBOX", "Archive"}, since: acctStart.Add(-24 * time.Hour)},
		{name: "no window", prefetch: 0, progress: inbox, folders: []string{"INBOX"}, since: acctStart},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newAcctRig(t, fake.New("acme", false), acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX", "Archive"}))
			r.setSettings(func(s *config.CacheSettings) { s.PrefetchBodies = tt.prefetch })
			for _, p := range tt.progress {
				if err := r.store.PutProgress(t.Context(), p); err != nil {
					t.Fatal(err)
				}
			}
			got, err := r.a.WindowCovers(t.Context(), tt.folders, tt.since)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("WindowCovers = %v, want %v", got, tt.want)
			}
		})
	}
}

// acctFreshRig is a rig whose INBOX is watching with its backfill done, and
// whose builder has applied the log up to its current tail, so only the
// events published after it decide Freshness.
func acctFreshRig(t *testing.T) *acctRig {
	t.Helper()
	r := newAcctRig(t, fake.New("acme", false), acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.putFolder(watch.FolderRow{Account: "acme", Folder: "INBOX", UIDValidity: 7, UIDNext: 10, Mode: watch.ModeIdle, State: watch.StateWatching, LastSyncAt: acctStart})
	if err := r.store.PutProgress(t.Context(), Progress{Account: "acme", Folder: "INBOX", UIDValidity: 7, BaselineUIDNext: 10, BackfillLowUID: 1, BackfillDone: true}); err != nil {
		t.Fatal(err)
	}
	r.a.applied.Store(r.tail())
	return r
}

// acctFreshEvent is a new event of account in INBOX at at.
func acctFreshEvent(account string, at time.Time) watch.Event {
	return watch.Event{At: at, Kind: watch.KindNew, Account: account, Folder: "INBOX", Ref: "cm1:lag", Message: &watch.Message{Subject: "lag"}}
}

// A trimmed gap still to apply means the builder lost events, so the
// account is stale at once, though Read stamps the gap with the current
// time.
func TestFreshnessStaleWithATrimmedGapPending(t *testing.T) {
	t.Parallel()
	r := acctFreshRig(t)
	r.publish(acctFreshEvent("acme", acctStart), acctFreshEvent("acme", acctStart), acctFreshEvent("acme", acctStart))
	if _, err := r.log.Trim(t.Context(), 168*time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	ok, reason, _, err := r.a.Freshness(t.Context(), []string{"INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	if ok || reason != ReasonStale {
		t.Errorf("Freshness = %v, %q; want false, %q with a trimmed gap pending", ok, reason, ReasonStale)
	}
}

// An old event of the account behind more than one Read's scan of other
// accounts' events still makes the account stale.
func TestFreshnessFindsALagBehindOtherAccounts(t *testing.T) {
	t.Parallel()
	r := acctFreshRig(t)
	others := make([]watch.Event, 5001)
	for i := range others {
		others[i] = acctFreshEvent("other", acctStart)
	}
	r.publish(others...)
	r.publish(acctFreshEvent("acme", acctStart.Add(-time.Hour)))
	ok, reason, _, err := r.a.Freshness(t.Context(), []string{"INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	if ok || reason != ReasonStale {
		t.Errorf("Freshness = %v, %q; want false, %q with an old event past one scan", ok, reason, ReasonStale)
	}
}

// A folder's sync row survives a daemon restart, but the mail that arrived
// while the daemon was down reaches the cache only through this run's
// passes. So a watching or polling folder is fresh only once a pass of this
// run has stamped its last_sync_at: the row a previous run left, however
// recent its state, is stale until then.
func TestFreshnessNeedsASyncInThisRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state watch.Status
		poll  bool
	}{
		{name: "watching", state: watch.StateWatching},
		{name: "polling", state: watch.StatePolling, poll: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := fake.New("acme", false)
			r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
			fs.Add(acctMsg("INBOX", "one"))
			r.pass("INBOX") // the previous run's last pass, at acctStart
			row := r.syncRow("INBOX")
			p := Progress{Account: "acme", Folder: "INBOX", UIDValidity: row.UIDValidity, BaselineUIDNext: row.UIDNext, BackfillLowUID: 1, BackfillDone: true}
			if err := r.store.PutProgress(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			if tt.poll {
				r.setPolling("INBOX")
			}
			fresh := func() (bool, string) {
				t.Helper()
				r.a.applied.Store(r.tail())
				ok, reason, _, err := r.a.Freshness(t.Context(), []string{"INBOX"})
				if err != nil {
					t.Fatal(err)
				}
				return ok, reason
			}
			if ok, reason := fresh(); !ok {
				t.Fatalf("Freshness before the restart = %v, %q; want fresh", ok, reason)
			}

			// The daemon restarts a minute later, with the row still saying
			// watching or polling from the previous run.
			r.clock.Advance(time.Minute)
			r.renew()
			if ok, reason := fresh(); ok || reason != ReasonStale {
				t.Errorf("Freshness after a restart, before a pass = %v, %q; want stale", ok, reason)
			}
			r.pass("INBOX")
			if tt.poll {
				r.setPolling("INBOX")
			}
			if ok, reason := fresh(); !ok {
				t.Errorf("Freshness after this run's first pass = %v, %q; want fresh", ok, reason)
			}
		})
	}
}
