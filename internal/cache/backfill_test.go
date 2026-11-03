package cache

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// First tracking creates the group at the log's tail, then records the
// baseline. A message that arrives between the two is both an event after
// the group's position and a UID below the baseline, and ends up cached
// once.
func TestBackfillFirstTrackingOrder(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	fs.Add(acctMsg("INBOX", "one"))
	fs.Add(acctMsg("INBOX", "two"))
	r.pass("INBOX")

	type seen struct {
		groupAtTail, tracked bool
		err                  error
	}
	hook := make(chan seen, 1)
	r.a.afterGroup = func() {
		ctx := t.Context()
		var s seen
		tail, terr := r.log.Tail(ctx)
		groups, gerr := r.log.Groups(ctx)
		for _, g := range groups {
			if g.Name == watch.CacheGroup("acme") && g.AckedID == tail {
				s.groupAtTail = true
			}
		}
		_, tracked, perr := r.store.Progress(ctx, "acme", "INBOX")
		s.tracked = tracked
		fs.Add(acctMsg("INBOX", "three"))
		_, xerr := r.engine.Pass(ctx, "acme", "INBOX", acctSource{fs: fs, folder: "INBOX"}, watch.PassOptions{Mode: watch.ModeIdle})
		s.err = errors.Join(terr, gerr, perr, xerr)
		hook <- s
	}
	r.start()
	var s seen
	select {
	case s = <-hook:
	case <-time.After(acctWait):
		t.Fatal("the builder never opened its group")
	}
	if s.err != nil {
		t.Fatalf("hook: %v", s.err)
	}
	if !s.groupAtTail || s.tracked {
		t.Errorf("at the hook: group at tail %v, folder tracked %v; want the group first, at the tail, and no baseline yet", s.groupAtTail, s.tracked)
	}
	r.waitApplied()
	r.waitBackfilled("INBOX")
	if p := r.progress("INBOX"); p.BaselineUIDNext != 4 {
		t.Errorf("baseline = %d, want 4, recorded after the third message arrived", p.BaselineUIDNext)
	}
	if n := r.count("INBOX", 1); n != 3 {
		t.Errorf("cached %d rows, want 3, each once", n)
	}
}

// Backfill records its low UID after each batch, so a restart resumes
// below it instead of fetching the folder again.
func TestBackfillResumesAfterRestart(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	for _, n := range []string{"one", "two", "three", "four", "five"} {
		fs.Add(acctMsg("INBOX", n))
	}
	r.pass("INBOX")
	r.setSettings(func(s *config.CacheSettings) {
		s.BackfillBatch = 2
		s.BackfillPause = 250 * time.Millisecond
	})
	r.start()
	r.clock.expectAfter(t, 250*time.Millisecond) // the pause after the first batch
	r.stop()
	if p := r.progress("INBOX"); p.BackfillLowUID != 4 || p.BackfillDone {
		t.Fatalf("after one batch: progress = %+v, want low UID 4, not done", p)
	}
	if n := r.count("INBOX", 1); n != 2 {
		t.Fatalf("after one batch: %d rows, want 2", n)
	}

	r.setSettings(func(s *config.CacheSettings) { s.BackfillPause = 0 })
	r.renew()
	r.start()
	r.waitBackfilled("INBOX")
	if n := r.count("INBOX", 1); n != 5 {
		t.Errorf("%d rows, want 5", n)
	}
	if got := fs.Calls("FetchHeaders"); got != 3 {
		t.Errorf("FetchHeaders ran %d times, want 3: one batch, then two more below UID 4", got)
	}
}

// Watched folders are backfilled first, then the rest by name.
func TestBackfillWatchedFoldersFirst(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	order := &acctOrderMail{Store: fs}
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX", "Archive"}))
	r.mail = order
	fs.Add(acctMsg("Archive", "old"))
	fs.Add(acctMsg("INBOX", "new"))
	r.pass("INBOX")
	r.pass("Archive")
	r.start()
	r.waitBackfilled("INBOX", "Archive")
	if got, want := order.folders(), []string{"INBOX", "Archive"}; !slices.Equal(got, want) {
		t.Errorf("fetch order = %q, want %q", got, want)
	}
}

// A search refused with ErrBusy is retried after backfill_pause.
func TestBackfillRetriesBusy(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	fs.Add(acctMsg("INBOX", "one"))
	fs.Add(acctMsg("INBOX", "two"))
	r.pass("INBOX")
	r.setSettings(func(s *config.CacheSettings) { s.BackfillPause = 250 * time.Millisecond })
	fs.SetBusy(true)
	r.start()
	r.clock.expectAfter(t, 250*time.Millisecond)
	if n := fs.BackgroundCalls("Search"); n < 1 {
		t.Errorf("background searches = %d, want the refused one", n)
	}
	if n := r.count("INBOX", 1); n != 0 {
		t.Errorf("%d rows cached while the pool was busy", n)
	}
	fs.SetBusy(false)
	r.advanceUntil("backfill to finish after the pause", 250*time.Millisecond, func() (bool, error) {
		p, ok, err := r.store.Progress(t.Context(), "acme", "INBOX")
		return ok && p.BackfillDone, err
	})
	if n := r.count("INBOX", 1); n != 2 {
		t.Errorf("%d rows, want 2", n)
	}
}

// A spent budget stops backfill until 00:00 UTC; the next day's budget
// lets it finish.
func TestBackfillBudgetWaitsForTheNextUTCDay(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	for _, n := range []string{"one", "two", "three"} {
		fs.Add(acctMsg("INBOX", n))
	}
	r.pass("INBOX")
	r.setSettings(func(s *config.CacheSettings) {
		s.BackfillBatch = 2
		s.DailyDownload = 2 * headerBudgetBytes
	})
	r.clock.Advance(14 * time.Hour) // 23:00 UTC
	r.start()
	r.clock.expectAfter(t, time.Hour)
	if p := r.progress("INBOX"); p.BackfillLowUID != 2 || p.BackfillDone {
		t.Errorf("progress while the budget is spent = %+v, want low UID 2, not done", p)
	}
	if got := r.budget("2026-11-02"); got != 2*headerBudgetBytes {
		t.Errorf("budget used = %d, want %d", got, 2*headerBudgetBytes)
	}
	r.clock.Advance(time.Hour)
	r.waitBackfilled("INBOX")
	if n := r.count("INBOX", 1); n != 3 {
		t.Errorf("%d rows, want 3", n)
	}
	if got := r.budget("2026-11-03"); got != headerBudgetBytes {
		t.Errorf("next day's budget = %d, want %d", got, headerBudgetBytes)
	}
}

// Both workers pause while a folder of the account is auth_required,
// checking again every second, and resume once it is not.
func TestWorkersPauseWhileAuthRequired(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	fs.Add(acctMsg("INBOX", "one"))
	r.pass("INBOX")
	r.setState("INBOX", watch.StateAuthRequired)
	r.start()
	r.clock.expectAfter(t, time.Second) // the backfill
	if n := fs.Calls("Search"); n != 0 {
		t.Errorf("backfill searched %d times while paused", n)
	}
	rf := fs.Add(acctMsg("INBOX", "two"))
	r.passWith("INBOX", false)          // the folder stays auth_required
	r.clock.expectAfter(t, time.Second) // the builder
	if n := fs.Calls("FetchHeaders"); n != 0 {
		t.Errorf("FetchHeaders ran %d times while paused", n)
	}
	r.setState("INBOX", watch.StateWatching)
	r.advanceUntil("both workers to resume", time.Second, func() (bool, error) {
		_, cached := r.row(rf)
		p, ok, err := r.store.Progress(t.Context(), "acme", "INBOX")
		return cached && ok && p.BackfillDone, err
	})
	if n := r.count("INBOX", 1); n != 2 {
		t.Errorf("%d rows, want 2", n)
	}
}

// After a uidvalidity gap, backfill fetches the folder again under the
// new UIDVALIDITY.
func TestBackfillRefillsAfterUIDValidityGap(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	fs.Add(acctMsg("INBOX", "one"))
	fs.Add(acctMsg("INBOX", "two"))
	r.pass("INBOX")
	r.start()
	r.waitBackfilled("INBOX")
	fs.NewUIDValidity("INBOX")
	r.pass("INBOX")
	r.eventually("backfill under the new UIDVALIDITY", func() (bool, error) {
		p, ok, err := r.store.Progress(t.Context(), "acme", "INBOX")
		return ok && p.UIDValidity == 2 && p.BackfillDone, err
	})
	if n := r.count("INBOX", 2); n != 2 {
		t.Errorf("%d rows under UIDVALIDITY 2, want 2", n)
	}
	if n := r.count("INBOX", 1); n != 0 {
		t.Errorf("%d rows left under UIDVALIDITY 1", n)
	}
}

// A folder whose sync_folders row is dropped is dropped from the cache at
// the builder's next step.
func TestBuilderUntracksDroppedFolder(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX", "Archive"}))
	fs.Add(acctMsg("Archive", "old"))
	r.pass("INBOX")
	r.pass("Archive")
	r.start()
	r.waitBackfilled("INBOX", "Archive")
	if n := r.count("Archive", 1); n != 1 {
		t.Fatalf("%d Archive rows, want 1", n)
	}
	if err := r.state.DropFolder(t.Context(), "acme", "Archive"); err != nil {
		t.Fatal(err)
	}
	r.publish(watch.Event{Kind: watch.KindStatus, Account: "acme", Folder: "INBOX", State: watch.StateWatching})
	r.waitApplied()
	if _, ok, err := r.store.Progress(t.Context(), "acme", "Archive"); err != nil || ok {
		t.Errorf("Archive progress kept (%v, %v) after its sync row was dropped", ok, err)
	}
	if n := r.count("Archive", 1); n != 0 {
		t.Errorf("%d Archive rows kept", n)
	}
}

// A batch fetched before a folder drop is discarded with its walk.
func TestCommitBatchDiscardsAfterReset(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	fs.Add(acctMsg("INBOX", "one"))
	r.pass("INBOX")
	if err := r.a.track(t.Context()); err != nil {
		t.Fatal(err)
	}
	w := &walk{folder: "INBOX", uidValidity: 1, epoch: r.a.epoch, uids: []uint32{1}}
	r.a.epoch++ // a drop ran while the batch was fetched
	row := Row{Account: "acme", Folder: "INBOX", UIDValidity: 1, UID: 1, Subject: "one", MessageID: "one@example.com", InternalDate: acctStart, Date: acctStart}
	next, more, err := r.a.commitBatch(t.Context(), w, []uint32{1}, []Row{row})
	if err != nil || next != nil || !more {
		t.Fatalf("commitBatch = %v, %v, %v; want nil, true, nil after a reset", next, more, err)
	}
	if _, ok := r.row(ref.Ref{Account: "acme", Folder: "INBOX", UIDValidity: 1, UID: 1}); ok {
		t.Error("a batch fetched before the reset was written")
	}
	if p := r.progress("INBOX"); p.BackfillLowUID != 0 || p.BackfillDone {
		t.Errorf("progress = %+v, want it untouched", p)
	}
}

// A folder in error, as one deleted on the server leaves it, is skipped
// alone: backfill and the builder keep working on the account's other
// folders (integration note 12). Its events are acknowledged without a
// fetch, and its backfill is left undone.
func TestWorkersSkipAFolderInError(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	fs.AddFolder("Accounts", mailstore.RoleNone)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", nil, []string{"Accounts", "INBOX"}))
	fs.Add(acctMsg("Accounts", "ledger"))
	fs.Add(acctMsg("INBOX", "one"))
	r.pass("Accounts")
	r.pass("INBOX")
	gone := r.syncRow("Accounts")
	if err := fs.DeleteFolder(t.Context(), "Accounts", true); err != nil {
		t.Fatal(err)
	}
	r.setState("Accounts", watch.StateError)
	r.start()
	r.waitApplied()

	// An event of the folder from before it went, whose fetch now fails.
	late := ref.Ref{Account: "acme", Folder: "Accounts", UIDValidity: gone.UIDValidity, UID: gone.UIDNext}
	r.publish(watch.Event{Kind: watch.KindNew, Account: "acme", Folder: "Accounts", Ref: late.String(), Message: &watch.Message{Subject: "late"}})
	two := r.add("INBOX", "two")[0]
	r.waitApplied()
	r.waitBackfilled("INBOX")
	if _, ok := r.row(two); !ok {
		t.Error("new INBOX mail was not cached while another folder is in error")
	}
	if n := r.count("INBOX", 1); n != 2 {
		t.Errorf("%d INBOX rows, want 2", n)
	}
	if _, ok := r.row(late); ok {
		t.Error("cached a row of the folder in error")
	}
	if p := r.progress("Accounts"); p.BackfillDone {
		t.Errorf("Accounts progress = %+v, want its backfill left undone", p)
	}
	if got := r.a.lastError(); got != "" {
		t.Errorf("lastError = %q, want none: a folder in error is not a cache failure", got)
	}
}

// A folder skipped in error is backfilled once it leaves error: the status
// event its recovery publishes wakes the idle backfill.
func TestBackfillResumesAFolderLeavingError(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	order := &acctOrderMail{Store: fs}
	fs.AddFolder("Accounts", mailstore.RoleNone)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", nil, []string{"Accounts", "INBOX"}))
	r.mail = order
	fs.Add(acctMsg("Accounts", "ledger"))
	fs.Add(acctMsg("INBOX", "one"))
	r.pass("Accounts")
	r.pass("INBOX")
	r.setState("Accounts", watch.StateError)
	r.start()
	r.waitBackfilled("INBOX")
	r.waitApplied()
	if got := order.folders(); slices.Contains(got, "Accounts") {
		t.Fatalf("fetched %q, want nothing of the folder in error", got)
	}

	r.setState("Accounts", watch.StatePolling)
	r.publish(watch.Event{Kind: watch.KindStatus, Account: "acme", Folder: "Accounts", State: watch.StatePolling})
	r.waitBackfilled("Accounts")
	if n := r.count("Accounts", 1); n != 1 {
		t.Errorf("%d Accounts rows, want 1", n)
	}
}

// A flags or expunged event the builder applies while the backfill is
// fetching a batch that holds its UID makes the backfill fetch that batch
// again: the header it fetched may predate the change, so writing it would
// bring back an expunged message as a ghost row or lose a flag change,
// with no later event to repair either.
func TestBackfillRefetchesABatchChangedDuringItsFetch(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	stall := newAcctStallMail(fs)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.mail = stall
	gone := fs.Add(acctMsg("INBOX", "gone"))
	seen := fs.Add(acctMsg("INBOX", "seen"))
	r.pass("INBOX")
	r.start()
	select {
	case <-stall.entered:
	case <-time.After(acctWait):
		t.Fatal("the backfill never fetched its first batch")
	}

	fs.ExternalExpunge("INBOX", gone.UID)
	fs.ExternalFlags("INBOX", seen.UID, []string{`\Seen`}, nil)
	r.sweep("INBOX")
	r.waitApplied()
	close(stall.release)
	r.waitBackfilled("INBOX")

	if _, ok := r.row(gone); ok {
		t.Error("the message expunged during the batch's fetch is cached")
	}
	row, ok := r.row(seen)
	if !ok {
		t.Fatal("the flagged message is not cached")
	}
	if want := fs.Flags(seen); !slices.Equal(row.Flags, want) {
		t.Errorf("row flags = %q, want the server's %q", row.Flags, want)
	}
	// Both fetches count: the discarded one's two headers and the second
	// one's single header.
	if got, want := r.budget("2026-11-02"), int64(3*headerBudgetBytes); got != want {
		t.Errorf("budget used = %d, want %d for both fetches", got, want)
	}
}
