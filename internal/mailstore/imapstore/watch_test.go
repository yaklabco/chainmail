package imapstore

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/storetest"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/testutil/fakegmail"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

const watchUser = "watch@example.test"

// watchFixture starts a fake Gmail account holding msgs and a Gmail store on
// it, closed when t ends.
func watchFixture(t *testing.T, msgs ...fakegmail.Message) (*Store, *fakegmail.Server, *tlstest.CA) {
	t.Helper()
	ca := tlstest.New(t)
	srv := fakegmail.Start(t, ca, watchUser, "pw", msgs...)
	s := New(testAccount("gmail", srv.Endpoint, true), passwordCreds(watchUser, "pw"), Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	return s, srv, ca
}

// TestWatchFolderState checks STATUS gives the state a pass selects, with
// no HIGHESTMODSEQ on a server without CONDSTORE, and a missing folder is a
// FolderNotFoundError.
func TestWatchFolderState(t *testing.T) {
	t.Parallel()
	s, _, _ := watchFixture(t,
		fakegmail.Message{Raw: "Subject: one\r\n\r\nx\r\n", Date: gmailDay(1)},
		fakegmail.Message{Raw: "Subject: two\r\n\r\ny\r\n", Date: gmailDay(2)},
	)
	st, err := s.FolderState(t.Context(), storetest.GmailAllMail)
	if err != nil {
		t.Fatal(err)
	}
	if st.UIDValidity == 0 || st.UIDNext != 3 || st.CondStore || st.HighestModSeq != 0 {
		t.Errorf("FolderState = %+v, want a UIDVALIDITY, UIDNEXT 3 and no CONDSTORE", st)
	}
	d, err := s.SyncFolder(t.Context(), storetest.GmailAllMail, mailstore.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if d.State != st {
		t.Errorf("SyncFolder state = %+v, want the STATUS state %+v", d.State, st)
	}
	var nf *mailstore.FolderNotFoundError
	if _, err := s.FolderState(t.Context(), "Nope"); !errors.As(err, &nf) || nf.Requested != "Nope" {
		t.Errorf("FolderState(Nope) = %v, want a FolderNotFoundError for Nope", err)
	}
	if _, err := s.SyncFolder(t.Context(), "Nope", mailstore.SyncRequest{}); !errors.As(err, &nf) || nf.Requested != "Nope" {
		t.Errorf("SyncFolder(Nope) = %v, want a FolderNotFoundError for Nope", err)
	}
}

// TestWatchChangedGmailItems checks a pass fetches X-GM-MSGID and
// X-GM-LABELS into Changed on a Gmail store, and neither on a standard one,
// and opens the folder read-only.
func TestWatchChangedGmailItems(t *testing.T) {
	t.Parallel()
	for _, gmail := range []bool{true, false} {
		t.Run(map[bool]string{true: "gmail", false: "standard"}[gmail], func(t *testing.T) {
			t.Parallel()
			ca := tlstest.New(t)
			srv := fakegmail.Start(t, ca, watchUser, "pw",
				fakegmail.Message{Raw: "Subject: one\r\n\r\nx\r\n", Date: gmailDay(1), Labels: []string{`\Inbox`}},
				fakegmail.Message{Raw: "Subject: two\r\n\r\ny\r\n", Date: gmailDay(2), Labels: []string{"Work"}},
			)
			s := New(testAccount("gmail", srv.Endpoint, gmail), passwordCreds(watchUser, "pw"), Options{RootCAs: ca.Pool, Backoff: noBackoff})
			t.Cleanup(func() { _ = s.Close() })
			d, err := s.SyncFolder(t.Context(), storetest.GmailAllMail, mailstore.SyncRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if !d.FullFlags || len(d.Changed) != 2 {
				t.Fatalf("Changed = %+v, FullFlags %v; want both messages", d.Changed, d.FullFlags)
			}
			for i, label := range []string{`\Inbox`, "Work"} {
				f := d.Changed[i]
				if gmail && (f.GmailMsgID != srv.MsgID(i) || !slices.Contains(f.Labels, label)) {
					t.Errorf("Changed[%d] = %+v, want X-GM-MSGID %d and label %s", i, f, srv.MsgID(i), label)
				}
				if !gmail && (f.GmailMsgID != 0 || f.Labels != nil) {
					t.Errorf("Changed[%d] = %+v, want no Gmail items on a standard store", i, f)
				}
			}
			if n := srv.WritableSelects(); n != 0 {
				t.Errorf("writable SELECTs = %d, want 0: a sync pass reads only", n)
			}
		})
	}
}

// TestWatchRebaseCarriesGmailMsgIDs checks every rebase on a Gmail store,
// the first one and one after a UIDVALIDITY change, gives each message's
// X-GM-MSGID in Changed, so the engine can build refs for later expunges.
func TestWatchRebaseCarriesGmailMsgIDs(t *testing.T) {
	t.Parallel()
	s, srv, _ := watchFixture(t,
		fakegmail.Message{Raw: "Subject: one\r\n\r\nx\r\n", Date: gmailDay(1)},
		fakegmail.Message{Raw: "Subject: two\r\n\r\ny\r\n", Date: gmailDay(2)},
	)
	check := func(what string, d mailstore.Delta) {
		t.Helper()
		if !d.Rebased || len(d.Changed) != 2 {
			t.Fatalf("%s: Rebased %v, Changed %+v; want a rebase with both messages", what, d.Rebased, d.Changed)
		}
		for i, f := range d.Changed {
			if f.GmailMsgID != srv.MsgID(i) {
				t.Errorf("%s: Changed[%d] X-GM-MSGID = %d, want %d", what, i, f.GmailMsgID, srv.MsgID(i))
			}
		}
	}
	first, err := s.SyncFolder(t.Context(), storetest.GmailAllMail, mailstore.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	check("first pass", first)
	srv.NewUIDValidity(storetest.GmailAllMail)
	from := mailstore.Checkpoint{UIDValidity: first.State.UIDValidity, UIDNext: first.State.UIDNext}
	again, err := s.SyncFolder(t.Context(), storetest.GmailAllMail, mailstore.SyncRequest{From: from})
	if err != nil {
		t.Fatal(err)
	}
	check("after a UIDVALIDITY change", again)
}

// TestWatchStateItems checks what a pass fetches for message states: Gmail
// items only on Gmail, labels and flags only when flags are asked for, as a
// CONDSTORE rebase on Gmail asks for X-GM-MSGID alone.
func TestWatchStateItems(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		gmail, flags bool
		since        uint64
		want         imap.FetchOptions
	}{
		{name: "standard flags", flags: true, want: imap.FetchOptions{UID: true, Flags: true}},
		{name: "standard changed since", flags: true, since: 7, want: imap.FetchOptions{UID: true, Flags: true, ChangedSince: 7}},
		{name: "standard UIDs only", want: imap.FetchOptions{UID: true}},
		{name: "gmail flags", gmail: true, flags: true, want: imap.FetchOptions{UID: true, Flags: true, GmailMsgID: true, GmailLabels: true}},
		{name: "gmail msgids only", gmail: true, want: imap.FetchOptions{UID: true, GmailMsgID: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := New(testAccount("acct", provider.Endpoint{}, tt.gmail), passwordCreds("u", "pw"), Options{})
			if got := s.stateItems(tt.flags, tt.since); !reflect.DeepEqual(*got, tt.want) {
				t.Errorf("stateItems(%v, %d) = %+v, want %+v", tt.flags, tt.since, *got, tt.want)
			}
		})
	}
}
