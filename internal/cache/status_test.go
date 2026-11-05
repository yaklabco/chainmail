package cache

import (
	"reflect"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// acctReadyFolder gives the account a watched folder whose backfill is
// done, and whose window is done when window is set.
func acctReadyFolder(r *acctRig, folder string, window bool) {
	r.t.Helper()
	r.putFolder(watch.FolderRow{Account: "acme", Folder: folder, UIDValidity: 7, UIDNext: 10, Mode: watch.ModeIdle, State: watch.StateWatching, LastSyncAt: acctStart})
	p := Progress{Account: "acme", Folder: folder, UIDValidity: 7, BaselineUIDNext: 10, BackfillLowUID: 1, BackfillDone: true, WindowDone: window}
	if err := r.store.PutProgress(r.t.Context(), p); err != nil {
		r.t.Fatal(err)
	}
}

// The account state follows C5's states, the most pressing first:
// disabled, error, paused, budget_exhausted, backfilling, prefetching,
// incomplete, ready. The daemon applies the file-wide over_cap itself.
func TestStatusStates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(r *acctRig)
		want  string
	}{
		{name: "ready", setup: func(r *acctRig) { acctReadyFolder(r, "INBOX", true) }, want: StateReady},
		{name: "disabled", setup: func(r *acctRig) {
			acctReadyFolder(r, "INBOX", true)
			r.setSettings(func(s *config.CacheSettings) { s.Enabled = false })
		}, want: StateDisabled},
		{name: "a cache fault", setup: func(r *acctRig) {
			acctReadyFolder(r, "INBOX", true)
			r.a.setError(api.ReasonInternal)
		}, want: StateError},
		{name: "a server failure keeps the state", setup: func(r *acctRig) {
			acctReadyFolder(r, "INBOX", true)
			r.a.setError(api.ReasonServerUnavailable)
		}, want: StateReady},
		{name: "paused", setup: func(r *acctRig) {
			acctReadyFolder(r, "INBOX", true)
			r.setState("INBOX", watch.StateAuthRequired)
		}, want: StatePaused},
		{name: "paused before a spent budget", setup: func(r *acctRig) {
			acctReadyFolder(r, "INBOX", true)
			r.setState("INBOX", watch.StateReconnecting)
			if _, err := r.store.AddBudget(r.t.Context(), "acme", "2026-11-02", 1<<30); err != nil {
				r.t.Fatal(err)
			}
		}, want: StatePaused},
		{name: "budget spent", setup: func(r *acctRig) {
			acctReadyFolder(r, "INBOX", true)
			if _, err := r.store.AddBudget(r.t.Context(), "acme", "2026-11-02", 1<<30); err != nil {
				r.t.Fatal(err)
			}
		}, want: StateBudgetExhausted},
		{name: "less than one header of budget left", setup: func(r *acctRig) {
			acctReadyFolder(r, "INBOX", true)
			if _, err := r.store.AddBudget(r.t.Context(), "acme", "2026-11-02", 1<<30-(headerBudgetBytes-1)); err != nil {
				r.t.Fatal(err)
			}
		}, want: StateBudgetExhausted},
		{name: "one header of budget left", setup: func(r *acctRig) {
			acctReadyFolder(r, "INBOX", true)
			if _, err := r.store.AddBudget(r.t.Context(), "acme", "2026-11-02", 1<<30-headerBudgetBytes); err != nil {
				r.t.Fatal(err)
			}
		}, want: StateReady},
		{name: "throttled by the server", setup: func(r *acctRig) {
			acctReadyFolder(r, "INBOX", true)
			r.a.throttled.set(r.clock.Now())
		}, want: StateBudgetExhausted},
		{name: "a throttle that has passed", setup: func(r *acctRig) {
			acctReadyFolder(r, "INBOX", true)
			r.a.throttled.set(r.clock.Now())
			r.clock.Advance(untilUTCMidnight(r.clock.Now()))
		}, want: StateReady},
		{name: "backfilling", setup: func(r *acctRig) {
			r.putFolder(watch.FolderRow{Account: "acme", Folder: "INBOX", UIDValidity: 7, UIDNext: 10, Mode: watch.ModeIdle, State: watch.StateWatching})
		}, want: StateBackfilling},
		{name: "no folders yet", setup: func(*acctRig) {}, want: StateBackfilling},
		{name: "prefetching", setup: func(r *acctRig) {
			r.setSettings(func(s *config.CacheSettings) { s.PrefetchBodies = 90 * 24 * time.Hour })
			acctReadyFolder(r, "INBOX", false)
		}, want: StatePrefetching},
		{name: "unreadable messages", setup: func(r *acctRig) {
			acctReadyFolder(r, "INBOX", true)
			putUnreadable(r, "INBOX", 3)
		}, want: StateIncomplete},
		{name: "attachments unknown is not incomplete", setup: func(r *acctRig) {
			acctReadyFolder(r, "INBOX", true)
			r.putRows(Row{Account: "acme", Folder: "INBOX", UIDValidity: 7, UID: 5, Subject: "a", MessageID: "a@example.com", InternalDate: acctStart, Date: acctStart, AttachmentsUnknown: true})
		}, want: StateReady},
		{name: "prefetching before incomplete", setup: func(r *acctRig) {
			r.setSettings(func(s *config.CacheSettings) { s.PrefetchBodies = 90 * 24 * time.Hour })
			acctReadyFolder(r, "INBOX", false)
			putUnreadable(r, "INBOX", 3)
		}, want: StatePrefetching},
		{name: "backfilling before incomplete", setup: func(r *acctRig) {
			acctReadyFolder(r, "INBOX", true)
			putUnreadable(r, "INBOX", 3)
			r.putFolder(watch.FolderRow{Account: "acme", Folder: "Archive", UIDValidity: 7, UIDNext: 10, Mode: watch.ModePoll, State: watch.StatePolling})
		}, want: StateBackfilling},
		{name: "budget spent before incomplete", setup: func(r *acctRig) {
			acctReadyFolder(r, "INBOX", true)
			putUnreadable(r, "INBOX", 3)
			if _, err := r.store.AddBudget(r.t.Context(), "acme", "2026-11-02", 1<<30); err != nil {
				r.t.Fatal(err)
			}
		}, want: StateBudgetExhausted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newAcctRig(t, fake.New("acme", false), acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
			tt.setup(r)
			st, err := r.a.Status(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tt.want {
				t.Errorf("State = %q, want %q", st.State, tt.want)
			}
		})
	}
}

// An empty prefetch window (prefetch_bodies 0d, bodies only on read) has
// nothing to prefetch, so a folder whose backfill is done is ready even
// though the prefetcher never sets its window_done. A non-zero window with
// bodies still missing keeps the folder and the account prefetching.
func TestStatusEmptyWindowIsComplete(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		window time.Duration
		want   string
	}{
		{name: "empty window", window: 0, want: StateReady},
		{name: "window with bodies missing", window: 90 * 24 * time.Hour, want: StatePrefetching},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newAcctRig(t, fake.New("acme", false), acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
			r.setSettings(func(s *config.CacheSettings) { s.PrefetchBodies = tt.window })
			acctReadyFolder(r, "INBOX", false)
			r.putRows(Row{Account: "acme", Folder: "INBOX", UIDValidity: 7, UID: 5, Subject: "a", MessageID: "a@example.com", InternalDate: acctStart, Date: acctStart})
			st, err := r.a.Status(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tt.want {
				t.Errorf("State = %q, want %q", st.State, tt.want)
			}
			if len(st.Folders) != 1 || st.Folders[0].State != tt.want {
				t.Errorf("Folders = %+v, want INBOX %s", st.Folders, tt.want)
			}
			if p := r.progress("INBOX"); p.WindowDone {
				t.Errorf("window_done = true, want Status to leave it unset")
			}
		})
	}
}

// putUnreadable records uids of the rig account's folder, at UIDVALIDITY
// 7, as unreadable.
func putUnreadable(r *acctRig, folder string, uids ...uint32) {
	r.t.Helper()
	if err := r.store.MarkUnreadable(r.t.Context(), "acme", mailstore.Source{Folder: folder, UIDValidity: 7}, uids); err != nil {
		r.t.Fatal(err)
	}
}

// Each folder reports how many of its messages are recorded as unreadable
// and how many rows were read without their body structure. A folder with
// unreadable messages is incomplete in place of ready. Rows whose
// attachments are unknown alone leave it ready.
func TestStatusReportsIncomplete(t *testing.T) {
	t.Parallel()
	r := newAcctRig(t, fake.New("acme", false), acctAccount(t, "hostinger", []string{"INBOX"}, []string{"Archive", "INBOX"}))
	acctReadyFolder(r, "Archive", true)
	acctReadyFolder(r, "INBOX", true)
	putUnreadable(r, "INBOX", 3, 4)
	r.putRows(
		Row{Account: "acme", Folder: "INBOX", UIDValidity: 7, UID: 5, Subject: "a", MessageID: "a@example.com", InternalDate: acctStart, Date: acctStart, AttachmentsUnknown: true},
		Row{Account: "acme", Folder: "Archive", UIDValidity: 7, UID: 6, Subject: "b", MessageID: "b@example.com", InternalDate: acctStart, Date: acctStart, AttachmentsUnknown: true},
	)
	st, err := r.a.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateIncomplete {
		t.Errorf("State = %q, want %q", st.State, StateIncomplete)
	}
	type folder struct {
		name                         string
		state                        string
		unreadable, unknown, headers uint64
	}
	var got []folder
	for _, f := range st.Folders {
		got = append(got, folder{f.Progress.Folder, f.State, f.Unreadable, f.AttachmentsUnknown, f.Headers})
	}
	want := []folder{{"Archive", StateReady, 0, 1, 1}, {"INBOX", StateIncomplete, 2, 1, 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Folders = %+v, want %+v", got, want)
	}
}

// Status reports the budget, settings and each tracked folder's progress,
// row counts and state.
func TestStatusReportsFolders(t *testing.T) {
	t.Parallel()
	r := newAcctRig(t, fake.New("acme", false), acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.putFolder(watch.FolderRow{Account: "acme", Folder: "INBOX", UIDValidity: 7, UIDNext: 10, Mode: watch.ModeIdle, State: watch.StateWatching, LastSyncAt: acctStart})
	if err := r.store.PutProgress(t.Context(), Progress{Account: "acme", Folder: "INBOX", UIDValidity: 7, BaselineUIDNext: 10, BackfillLowUID: 3}); err != nil {
		t.Fatal(err)
	}
	r.putRows(
		Row{Account: "acme", Folder: "INBOX", UIDValidity: 7, UID: 5, Subject: "a", MessageID: "a@example.com", InternalDate: acctStart, Date: acctStart},
		Row{Account: "acme", Folder: "INBOX", UIDValidity: 7, UID: 6, Subject: "b", MessageID: "b@example.com", InternalDate: acctStart, Date: acctStart},
	)
	if _, err := r.store.AddBudget(t.Context(), "acme", "2026-11-02", 4096); err != nil {
		t.Fatal(err)
	}
	st, err := r.a.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Account != "acme" || !st.Enabled || st.State != StateBackfilling || st.BudgetUsed != 4096 || st.BudgetBytes != 1<<30 || st.LastError != "" {
		t.Errorf("Status = %+v, want acme, enabled, backfilling, 4096 of 1GiB used, no error", st)
	}
	if len(st.Folders) != 1 {
		t.Fatalf("Folders = %+v, want INBOX only", st.Folders)
	}
	f := st.Folders[0]
	if f.Progress.Folder != "INBOX" || f.FolderStats.Folder != "INBOX" || f.BaselineUIDNext != 10 || f.BackfillLowUID != 3 || f.BackfillDone {
		t.Errorf("folder progress = %+v, want INBOX, baseline 10, low UID 3, not done", f.Progress)
	}
	if f.Headers != 2 || f.State != StateBackfilling {
		t.Errorf("folder = %d headers, state %q; want 2, backfilling", f.Headers, f.State)
	}
}
