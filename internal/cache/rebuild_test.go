package cache

import (
	"errors"
	"slices"
	"testing"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// Rebuild of one folder drops its rows without keeping Message-IDs and
// resets its progress to a fresh baseline; other folders are untouched.
// Rebuild of "" does every tracked folder. An untracked folder is
// NOT_FOUND.
func TestRebuildResetsProgress(t *testing.T) {
	t.Parallel()
	r := newAcctRig(t, fake.New("acme", false), acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX", "Archive"}))
	r.putFolder(watch.FolderRow{Account: "acme", Folder: "INBOX", UIDValidity: 7, UIDNext: 10, Mode: watch.ModeIdle, State: watch.StateWatching, LastSyncAt: acctStart})
	r.putFolder(watch.FolderRow{Account: "acme", Folder: "Archive", UIDValidity: 7, UIDNext: 5, Mode: watch.ModePoll, State: watch.StatePolling, LastSyncAt: acctStart})
	for _, f := range []string{"INBOX", "Archive"} {
		p := Progress{Account: "acme", Folder: f, UIDValidity: 7, BaselineUIDNext: 3, BackfillLowUID: 1, BackfillDone: true, WindowDone: true, LastAppliedAt: acctStart}
		if err := r.store.PutProgress(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	inbox := ref.Ref{Account: "acme", Folder: "INBOX", UIDValidity: 7, UID: 4}
	archive := ref.Ref{Account: "acme", Folder: "Archive", UIDValidity: 7, UID: 2}
	r.putRows(
		Row{Account: "acme", Folder: "INBOX", UIDValidity: 7, UID: 4, Subject: "in", MessageID: "in@example.com", InternalDate: acctStart, Date: acctStart},
		Row{Account: "acme", Folder: "Archive", UIDValidity: 7, UID: 2, Subject: "ar", MessageID: "ar@example.com", InternalDate: acctStart, Date: acctStart},
	)

	folders, err := r.a.Rebuild(t.Context(), "INBOX")
	if err != nil || !slices.Equal(folders, []string{"INBOX"}) {
		t.Fatalf("Rebuild(INBOX) = %q, %v", folders, err)
	}
	if p := r.progress("INBOX"); p.UIDValidity != 7 || p.BaselineUIDNext != 10 || p.BackfillLowUID != 0 || p.BackfillDone || p.WindowDone {
		t.Errorf("INBOX progress = %+v, want UIDVALIDITY 7, baseline 10 and nothing done", p)
	}
	if _, ok := r.row(inbox); ok {
		t.Error("Rebuild(INBOX) kept an INBOX row")
	}
	if _, ok, err := r.store.MessageIDOf(t.Context(), inbox); err != nil || ok {
		t.Errorf("MessageIDOf(INBOX row) = %v, %v; a rebuild keeps no recovery rows", ok, err)
	}
	if _, ok := r.row(archive); !ok {
		t.Error("Rebuild(INBOX) dropped an Archive row")
	}
	if p := r.progress("Archive"); !p.BackfillDone {
		t.Errorf("Archive progress = %+v, want it untouched", p)
	}

	folders, err = r.a.Rebuild(t.Context(), "")
	if err != nil || !slices.Equal(folders, []string{"Archive", "INBOX"}) {
		t.Fatalf("Rebuild(\"\") = %q, %v", folders, err)
	}
	if _, ok := r.row(archive); ok {
		t.Error("Rebuild(\"\") kept an Archive row")
	}
	if p := r.progress("Archive"); p.BaselineUIDNext != 5 || p.BackfillDone {
		t.Errorf("Archive progress = %+v, want baseline 5 and backfill not done", p)
	}

	_, err = r.a.Rebuild(t.Context(), "Nope")
	var nf *mailstore.FolderNotFoundError
	if !errors.Is(err, mailstore.ErrNotFound) || !errors.As(err, &nf) || nf.Requested != "Nope" {
		t.Errorf("Rebuild(Nope) = %v, want a FolderNotFoundError for Nope", err)
	}
}

// After a rebuild the running backfill fetches the folder again, and rows
// the server never had are gone.
func TestRebuildBackfillsAgain(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	for _, n := range []string{"one", "two", "three"} {
		fs.Add(acctMsg("INBOX", n))
	}
	r.pass("INBOX")
	r.start()
	r.waitBackfilled("INBOX")
	before := fs.Calls("FetchHeaders")
	bogus := ref.Ref{Account: "acme", Folder: "INBOX", UIDValidity: 1, UID: 77}
	r.putRows(Row{Account: "acme", Folder: "INBOX", UIDValidity: 1, UID: 77, Subject: "bogus", MessageID: "bogus@example.com", InternalDate: acctStart, Date: acctStart})

	folders, err := r.a.Rebuild(t.Context(), "INBOX")
	if err != nil || !slices.Equal(folders, []string{"INBOX"}) {
		t.Fatalf("Rebuild = %q, %v", folders, err)
	}
	r.eventually("the rebuilt folder to backfill again", func() (bool, error) {
		p, ok, err := r.store.Progress(t.Context(), "acme", "INBOX")
		return ok && p.BackfillDone && fs.Calls("FetchHeaders") > before, err
	})
	if _, ok := r.row(bogus); ok {
		t.Error("the rebuild kept a row the server never had")
	}
	if n := r.count("INBOX", 1); n != 3 {
		t.Errorf("%d rows after the rebuild, want 3", n)
	}
}
