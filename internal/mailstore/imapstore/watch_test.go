package imapstore

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/storetest"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/testutil/fakegmail"
	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
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

// syncWithin runs run on a worker it joins before returning, and fails the
// test if run is still running after limit. On timeout it cancels run's
// context and calls abort, which must unblock run even if cancelling does
// not, then waits for the worker before reporting the failure.
func syncWithin(t *testing.T, limit time.Duration, abort func(), run func(context.Context) (mailstore.Delta, error)) (mailstore.Delta, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var (
		d    mailstore.Delta
		err  error
		wg   sync.WaitGroup
		done = make(chan struct{})
	)
	wg.Go(func() {
		defer close(done)
		d, err = run(ctx)
	})
	select {
	case <-done:
		wg.Wait()
		return d, err
	case <-time.After(limit):
		cancel()
		abort()
		wg.Wait()
		t.Fatalf("sync still running after %v", limit)
		return mailstore.Delta{}, nil
	}
}

// TestWatchStateFetchOutlastsCommandTimeout checks a pass whose state FETCH
// takes longer in all than the command timeout, from a server that answers
// for each message well within it, succeeds: the timeout bounds the silence
// between replies, not the whole answer. A large Gmail All Mail rebases this
// way on every watcher start.
func TestWatchStateFetchOutlastsCommandTimeout(t *testing.T) {
	t.Parallel()
	const (
		n       = 20
		delay   = 40 * time.Millisecond // n*delay is twice the timeout
		timeout = 400 * time.Millisecond
	)
	cases := map[string]struct {
		condstore, gmail, watch bool
		from                    mailstore.Checkpoint
	}{
		// X-GM-MSGID for every message, on the watch connection.
		"gmail rebase": {condstore: true, gmail: true, watch: true},
		// UID and FLAGS for every message, without CONDSTORE.
		"full flags": {},
		// CHANGEDSINCE over every message, after a long outage.
		"changed since": {condstore: true, from: mailstore.Checkpoint{UIDValidity: scriptUIDValidity, UIDNext: n + 1, HighestModSeq: 1}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := startScriptServer(t, &scriptServer{n: n, condstore: tc.condstore, stateDelay: delay})
			s := New(testAccount("host", srv.ep, tc.gmail), passwordCreds("host@example.test", "pw"),
				Options{RootCAs: srv.ca.Pool, Backoff: noBackoff, CommandTimeout: timeout, Retries: -1})
			t.Cleanup(func() { _ = s.Close() })
			req := mailstore.SyncRequest{From: tc.from}
			abort := func() { _ = s.Close() }
			run := func(ctx context.Context) (mailstore.Delta, error) { return s.SyncFolder(ctx, "INBOX", req) }
			if tc.watch {
				w, err := s.OpenWatch(t.Context(), "INBOX")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = w.Close() })
				abort = func() { _ = w.Close() }
				run = func(ctx context.Context) (mailstore.Delta, error) { return w.Sync(ctx, req) }
			}
			d, err := syncWithin(t, 5*time.Second, abort, run)
			if err != nil {
				t.Fatalf("sync: %v", err)
			}
			if len(d.Changed) != n {
				t.Fatalf("Changed = %+v, want all %d messages", d.Changed, n)
			}
			for i, f := range d.Changed {
				if f.UID != uint32(i+1) {
					t.Errorf("Changed[%d].UID = %d, want %d: ascending", i, f.UID, i+1)
				}
				if tc.gmail && f.GmailMsgID != scriptMsgID(i+1) {
					t.Errorf("Changed[%d].GmailMsgID = %d, want %d", i, f.GmailMsgID, scriptMsgID(i+1))
				}
				if !tc.condstore && !slices.Equal(f.Flags, []string{`\Seen`}) {
					t.Errorf("Changed[%d].Flags = %v, want [\\Seen]", i, f.Flags)
				}
			}
			if tc.from == (mailstore.Checkpoint{}) && len(d.UIDs) != n {
				t.Errorf("UIDs = %v, want all %d", d.UIDs, n)
			}
		})
	}
}

// TestWatchStateFetchStallTimesOut checks a server that goes silent during
// a state FETCH, midway through the messages or after the last one and
// before the tagged OK, still runs into the command timeout, and the watch
// connection reports the account unavailable.
func TestWatchStateFetchStallTimesOut(t *testing.T) {
	t.Parallel()
	const timeout = 400 * time.Millisecond
	for name, srv := range map[string]*scriptServer{
		"midway":           {n: 10, condstore: true, stallAfter: 3},
		"before tagged OK": {n: 10, condstore: true, stallBeforeDone: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			startScriptServer(t, srv)
			s := New(testAccount("host", srv.ep, true), passwordCreds("host@example.test", "pw"),
				Options{RootCAs: srv.ca.Pool, Backoff: noBackoff, CommandTimeout: timeout, Retries: -1})
			t.Cleanup(func() { _ = s.Close() })
			w, err := s.OpenWatch(t.Context(), "INBOX")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = w.Close() })
			start := time.Now()
			_, err = syncWithin(t, 5*time.Second, func() { _ = w.Close() }, func(ctx context.Context) (mailstore.Delta, error) {
				return w.Sync(ctx, mailstore.SyncRequest{})
			})
			if !errors.Is(err, mailstore.ErrUnavailable) || !errors.Is(err, errCommandTimeout) {
				t.Errorf("Sync = %v, want ErrUnavailable from the command timeout", err)
			}
			if elapsed := time.Since(start); elapsed > 10*timeout {
				t.Errorf("Sync took %v with a %v command timeout", elapsed, timeout)
			}
		})
	}
}
