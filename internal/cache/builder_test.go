package cache

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// A new event's message is fetched with FetchHeaders, so its row has what
// the event lacks (Cc, Bcc, Message-ID, In-Reply-To, References,
// attachments), and on Gmail its labels and thread. The folder's
// LastAppliedAt is stamped and the header counts 2048 bytes of budget.
func TestBuilderCachesNewMail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		gmail  bool
		preset string
		watch  []string
		folder string
		labels []string
		thread uint64
	}{
		{name: "standard", preset: "hostinger", watch: []string{"INBOX"}, folder: "INBOX"},
		{name: "gmail", gmail: true, preset: "gmail", folder: "[Gmail]/All Mail", labels: []string{`\Inbox`, "work"}, thread: 0x5150},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := fake.New("acme", tt.gmail)
			r := newAcctRig(t, fs, acctAccount(t, tt.preset, tt.watch, tt.watch))
			r.pass(tt.folder) // the baseline of an empty folder
			r.start()
			r.waitApplied()

			m := acctMsg(tt.folder, "one")
			m.Cc = []mailstore.Address{{Name: "Cy", Email: "cy@example.com"}}
			m.Bcc = []mailstore.Address{{Email: "hidden@example.com"}}
			m.InReplyTo = []string{"zero@example.com"}
			m.References = []string{"zero@example.com"}
			m.Flags = []string{`\Seen`}
			m.Labels = tt.labels
			m.GmailThreadID = tt.thread
			m.Attachments = []fake.Attachment{{Part: "2", Filename: "a.pdf", ContentType: "application/pdf", Data: []byte("%PDF")}}
			rf := fs.Add(m)
			r.pass(tt.folder)
			r.waitApplied()

			row, ok := r.row(rf)
			if !ok {
				t.Fatalf("no cached row for %v", rf)
			}
			if row.Subject != "one" || row.MessageID != "one@example.com" || row.UID != rf.UID || row.UIDValidity != rf.UIDValidity {
				t.Errorf("row = %+v, want subject one, Message-ID one@example.com, ref %v", row, rf)
			}
			if !row.InternalDate.Equal(m.Date) || !row.Date.Equal(m.Date) {
				t.Errorf("row dates = %v, %v, want %v", row.InternalDate, row.Date, m.Date)
			}
			if !slices.Equal(row.From, m.From) || !slices.Equal(row.To, m.To) || !slices.Equal(row.Cc, m.Cc) || !slices.Equal(row.Bcc, m.Bcc) {
				t.Errorf("row addresses = %v %v %v %v, want %v %v %v %v", row.From, row.To, row.Cc, row.Bcc, m.From, m.To, m.Cc, m.Bcc)
			}
			if !slices.Equal(row.InReplyTo, m.InReplyTo) || !slices.Equal(row.References, m.References) {
				t.Errorf("row threading = %q %q, want %q %q", row.InReplyTo, row.References, m.InReplyTo, m.References)
			}
			if !slices.Equal(row.Flags, m.Flags) || !slices.Equal(row.Labels, tt.labels) {
				t.Errorf("row flags and labels = %q %q, want %q %q", row.Flags, row.Labels, m.Flags, tt.labels)
			}
			if !row.HasAttachment || len(row.Attachments) != 1 || row.Attachments[0].Filename != "a.pdf" {
				t.Errorf("row attachments = %v %+v, want one named a.pdf", row.HasAttachment, row.Attachments)
			}
			if row.GmailMsgID != rf.GmailMsgID || row.GmailThrID != tt.thread {
				t.Errorf("row Gmail IDs = %d %d, want %d %d", row.GmailMsgID, row.GmailThrID, rf.GmailMsgID, tt.thread)
			}
			if tt.gmail && row.ThreadID != strconv.FormatUint(tt.thread, 16) {
				t.Errorf("row thread = %q, want %x", row.ThreadID, tt.thread)
			}
			if p := r.progress(tt.folder); !p.LastAppliedAt.Equal(acctStart) {
				t.Errorf("LastAppliedAt = %v, want %v", p.LastAppliedAt, acctStart)
			}
			if got := r.budget("2026-11-02"); got != headerBudgetBytes {
				t.Errorf("budget = %d, want %d for one header", got, headerBudgetBytes)
			}
		})
	}
}

// A flags event updates the cached row. A flags event for a UID the cache
// has not fetched, and any event of a folder it does not track, change
// nothing and fetch nothing, and are still acknowledged.
func TestBuilderAppliesFlags(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.pass("INBOX")
	r.start()
	r.waitApplied()
	rf := r.add("INBOX", "one")[0]
	r.waitApplied()

	fs.ExternalFlags("INBOX", rf.UID, []string{`\Flagged`}, nil)
	r.pass("INBOX")
	r.waitApplied()
	if row, ok := r.row(rf); !ok || !slices.Contains(row.Flags, `\Flagged`) {
		t.Errorf("row flags = %q (cached %v), want \\Flagged added", row.Flags, ok)
	}

	ghost := ref.Ref{Account: "acme", Folder: "INBOX", UIDValidity: rf.UIDValidity, UID: 99}
	other := ref.Ref{Account: "acme", Folder: "Archive", UIDValidity: 1, UID: 1}
	r.publish(
		watch.Event{Kind: watch.KindFlags, Account: "acme", Folder: "INBOX", Ref: ghost.String(), Flags: []string{`\Seen`}},
		watch.Event{Kind: watch.KindNew, Account: "acme", Folder: "Archive", Ref: other.String(), Message: &watch.Message{Subject: "x"}},
	)
	r.waitApplied()
	if _, ok := r.row(ghost); ok {
		t.Error("a flags event created a row for an uncached UID")
	}
	if _, ok := r.row(other); ok {
		t.Error("an event of an untracked folder was applied")
	}
	if got := fs.Calls("FetchHeaders"); got != 1 {
		t.Errorf("FetchHeaders ran %d times, want 1 (for the one new message)", got)
	}
	if got, tail := r.acked(), r.tail(); got != tail {
		t.Errorf("acked %d, want the tail %d", got, tail)
	}
}

// An expunged event deletes the cached row.
func TestBuilderDeletesExpunged(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.pass("INBOX")
	r.start()
	r.waitApplied()
	rf := r.add("INBOX", "one")[0]
	r.waitApplied()
	if _, ok := r.row(rf); !ok {
		t.Fatal("the new message was not cached")
	}
	fs.ExternalExpunge("INBOX", rf.UID)
	r.sweep("INBOX")
	r.waitApplied()
	if _, ok := r.row(rf); ok {
		t.Error("the expunged message's row is still cached")
	}
}

// The builder acknowledges a segment only after its cache writes commit:
// a failed write leaves the group where it was, the step is retried after
// the backoff, and the event is read and fetched again.
func TestBuilderAcksOnlyAfterCommit(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.pass("INBOX")
	r.start()
	r.waitApplied()
	before := r.acked()

	heal := r.failInserts()
	rf := r.add("INBOX", "one")[0]
	r.eventually("the builder's apply to fail", func() (bool, error) {
		return r.a.lastError() == api.ReasonInternal, nil
	})
	if got := r.acked(); got != before {
		t.Errorf("acked %d after a failed apply, want %d", got, before)
	}
	if _, ok := r.row(rf); ok {
		t.Error("a row was cached through a failing insert")
	}

	heal()
	r.advanceUntil("the builder to retry", time.Second, func() (bool, error) {
		_, ok := r.row(rf)
		return ok, nil
	})
	r.waitApplied()
	if got := fs.Calls("FetchHeaders"); got < 2 {
		t.Errorf("FetchHeaders ran %d times, want the event fetched again", got)
	}
	r.eventually("the error to clear", func() (bool, error) { return r.a.lastError() == "", nil })
}

// A header fetch refused with ErrBusy is retried after backfill_pause, on
// a background context, and the event is not acknowledged meanwhile.
func TestBuilderRetriesBusy(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	busy := &acctBusyMail{Store: fs}
	busy.busy.Store(true)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.mail = busy
	r.setSettings(func(s *config.CacheSettings) { s.BackfillPause = 250 * time.Millisecond })
	r.pass("INBOX")
	r.start()
	r.waitApplied()

	rf := r.add("INBOX", "one")[0]
	r.eventually("a refused header fetch", func() (bool, error) { return busy.refused.Load() >= 1, nil })
	if _, ok := r.row(rf); ok {
		t.Error("cached a row from a refused fetch")
	}
	if got, tail := r.acked(), r.tail(); got == tail {
		t.Error("acknowledged an event whose fetch was refused")
	}

	busy.busy.Store(false)
	r.advanceUntil("the builder to retry after the pause", 250*time.Millisecond, func() (bool, error) {
		_, ok := r.row(rf)
		return ok, nil
	})
	r.waitApplied()
	if n := busy.foreground.Load(); n != 0 {
		t.Errorf("%d header fetches ran on a foreground context", n)
	}
}

// A segment fetched before a folder drop is discarded, not written.
func TestCommitSegmentDiscardsAfterReset(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.pass("INBOX")
	if err := r.a.track(t.Context()); err != nil {
		t.Fatal(err)
	}
	epoch := r.a.epoch
	r.a.epoch++ // a drop ran while the segment was fetched
	late := ref.Ref{Account: "acme", Folder: "INBOX", UIDValidity: 1, UID: 1}
	row := Row{Account: "acme", Folder: "INBOX", UIDValidity: 1, UID: 1, Subject: "late", MessageID: "late@example.com", InternalDate: acctStart, Date: acctStart}
	done, err := r.a.commitSegment(t.Context(), epoch, segmentPlan{touched: map[string]bool{"INBOX": true}}, []Row{row})
	if err != nil || done {
		t.Fatalf("commitSegment = %v, %v; want false, nil after a reset", done, err)
	}
	if _, ok := r.row(late); ok {
		t.Error("a segment fetched before the reset was written")
	}
}

// A uidvalidity gap drops the folder's rows, keeping each Message-ID for
// ref recovery, and records the new UIDVALIDITY with the folder's current
// UIDNEXT as its baseline.
func TestBuilderUIDValidityGapDropsTheFolder(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.pass("INBOX")
	r.start()
	r.waitApplied()
	old := r.add("INBOX", "one", "two")
	r.waitApplied()
	if _, ok := r.row(old[0]); !ok {
		t.Fatal("the new message was not cached")
	}

	fs.NewUIDValidity("INBOX")
	r.pass("INBOX")
	r.waitApplied()
	if _, ok := r.row(old[0]); ok {
		t.Error("the gap left the row under the old UIDVALIDITY")
	}
	id, ok, err := r.store.MessageIDOf(t.Context(), old[0])
	if err != nil || !ok || id != "one@example.com" {
		t.Errorf("MessageIDOf(old ref) = %q, %v, %v; want one@example.com kept for recovery", id, ok, err)
	}
	p, row := r.progress("INBOX"), r.syncRow("INBOX")
	if p.UIDValidity != row.UIDValidity || p.UIDValidity == old[0].UIDValidity || p.BaselineUIDNext != row.UIDNext {
		t.Errorf("progress = %+v, want UIDVALIDITY %d and baseline %d from the sync row", p, row.UIDValidity, row.UIDNext)
	}
}

// A trimmed gap rebuilds the whole account: rows the server never had are
// gone, the baseline is the folder's current UIDNEXT, and events after the
// gap still apply.
func TestBuilderTrimmedGapRebuilds(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.pass("INBOX")
	r.start()
	r.waitApplied()
	first := r.add("INBOX", "one")[0]
	r.waitApplied()
	bogus := ref.Ref{Account: "acme", Folder: "INBOX", UIDValidity: first.UIDValidity, UID: 50}
	r.putRows(Row{Account: "acme", Folder: "INBOX", UIDValidity: first.UIDValidity, UID: 50, Subject: "bogus", MessageID: "bogus@example.com", InternalDate: acctStart, Date: acctStart})
	r.stop()

	r.add("INBOX", "two")
	later := r.add("INBOX", "three")[0]
	if _, err := r.log.Trim(t.Context(), 168*time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	r.renew()
	r.start()
	r.waitApplied()
	if _, ok := r.row(bogus); ok {
		t.Error("the trimmed gap kept a row the rebuild should have dropped")
	}
	if _, ok := r.row(later); !ok {
		t.Error("the event after the gap was not applied")
	}
	if p, row := r.progress("INBOX"), r.syncRow("INBOX"); p.BaselineUIDNext != row.UIDNext {
		t.Errorf("baseline = %d, want the current UIDNEXT %d", p.BaselineUIDNext, row.UIDNext)
	}
}

// A group acknowledged past the log's tail, as a reset state.db leaves it,
// rebuilds the account and moves the group back to the tail.
func TestBuilderRecoversAGroupPastTheTail(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.pass("INBOX")
	r.start()
	r.waitApplied()
	first := r.add("INBOX", "one")[0]
	r.waitApplied()
	r.stop()

	acked := r.acked()
	if ok, err := r.log.ResetGroup(t.Context(), watch.CacheGroup("acme"), acked, acked+100); err != nil || !ok {
		t.Fatalf("ResetGroup = %v, %v", ok, err)
	}
	bogus := ref.Ref{Account: "acme", Folder: "INBOX", UIDValidity: first.UIDValidity, UID: 50}
	r.putRows(Row{Account: "acme", Folder: "INBOX", UIDValidity: first.UIDValidity, UID: 50, Subject: "bogus", MessageID: "bogus@example.com", InternalDate: acctStart, Date: acctStart})
	r.renew()
	r.start()
	r.waitApplied()
	if got, tail := r.acked(), r.tail(); got != tail {
		t.Errorf("acked %d, want it moved back to the tail %d", got, tail)
	}
	if _, ok := r.row(bogus); ok {
		t.Error("the rebuild kept a row of the lost log")
	}
}
