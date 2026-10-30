package imapstore

import (
	"crypto/tls"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/fakegmail"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

const (
	allMail    = "[Gmail]/All Mail"
	gmailTrash = "[Gmail]/Trash"
	gmailSpam  = "[Gmail]/Spam"
)

// Messages of newGmailWrites, by index.
const (
	gwInbox   = iota // labels \Inbox and Receipts: All Mail 1, INBOX 101, Receipts 1001
	gwInbox2         // label \Inbox: All Mail 2, INBOX 102
	gwWork           // label Work: All Mail 3, Work 1101
	gwTrashed        // in Trash: Trash 301
	gwSpammed        // in Spam: Spam 401
)

type gmailWrites struct {
	store *Store
	srv   *fakegmail.Server
	ca    *tlstest.CA
}

func gwRaw(id string) string {
	return fmt.Sprintf("From: a@example.com\r\nSubject: %s\r\nMessage-ID: <%s@x>\r\n\r\n%s\r\n", id, id, id)
}

func newGmailWrites(t *testing.T) gmailWrites {
	t.Helper()
	ca := tlstest.New(t)
	srv := fakegmail.Start(t, ca, "gw@example.test", "pw",
		fakegmail.Message{Raw: gwRaw("inbox"), Date: gmailDay(1), Labels: []string{`\Inbox`, "Receipts"}},
		fakegmail.Message{Raw: gwRaw("inbox2"), Date: gmailDay(2), Labels: []string{`\Inbox`}},
		fakegmail.Message{Raw: gwRaw("work"), Date: gmailDay(3), Labels: []string{"Work"}},
		fakegmail.Message{Raw: gwRaw("trashed"), Date: gmailDay(4), Trash: true},
		fakegmail.Message{Raw: gwRaw("spammed"), Date: gmailDay(5), Spam: true},
	)
	s := New(testAccount("gmail", srv.Endpoint, true), passwordCreds("gw@example.test", "pw"), Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	return gmailWrites{store: s, srv: srv, ca: ca}
}

// client logs in to the fake on a connection of the test's own.
func (w gmailWrites) client(t *testing.T) *imapclient.Client {
	t.Helper()
	c, err := imapclient.DialTLS(w.srv.Endpoint.Addr(), &imapclient.Options{TLSConfig: &tls.Config{RootCAs: w.ca.Pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Login("gw@example.test", "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	return c
}

// ref is the ref of uid in folder, with the folder's UIDVALIDITY now.
func (w gmailWrites) ref(t *testing.T, folder string, uid uint32) ref.Ref {
	t.Helper()
	data, err := w.client(t).Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	return ref.Ref{Account: "gmail", Folder: folder, UIDValidity: data.UIDValidity, UID: uid}
}

// found is the All Mail ref of message i as an X-GM-MSGID search finds it,
// with its X-GM-MSGID.
func (w gmailWrites) found(t *testing.T, i int) ref.Ref {
	t.Helper()
	r := w.ref(t, allMail, w.where(t, i)[allMail])
	r.GmailMsgID = w.srv.MsgID(i)
	return r
}

// where maps each folder holding message i to its UID there.
func (w gmailWrites) where(t *testing.T, i int) map[string]uint32 {
	t.Helper()
	c := w.client(t)
	list, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]uint32{}
	for _, l := range list {
		if slices.Contains(l.Attrs, imap.MailboxAttrNoSelect) {
			continue
		}
		if _, err := c.Select(l.Mailbox, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			t.Fatal(err)
		}
		data, err := c.UIDSearch(&imap.SearchCriteria{GmailMsgID: []uint64{w.srv.MsgID(i)}}, nil).Wait()
		if err != nil {
			t.Fatal(err)
		}
		for _, uid := range data.AllUIDs() {
			out[l.Mailbox] = uint32(uid)
		}
	}
	return out
}

// labels returns message i's X-GM-LABELS, read in folder.
func (w gmailWrites) labels(t *testing.T, i int, folder string) []string {
	t.Helper()
	uid, ok := w.where(t, i)[folder]
	if !ok {
		t.Fatalf("message %d is not in %s", i, folder)
	}
	c := w.client(t)
	if _, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	bufs, err := c.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{UID: true, GmailLabels: true}).Collect()
	if err != nil || len(bufs) != 1 {
		t.Fatalf("FETCH X-GM-LABELS = %v, %v", bufs, err)
	}
	return bufs[0].GmailLabels
}

// in reports whether got holds exactly the folders want names.
func in(got map[string]uint32, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, f := range want {
		if _, ok := got[f]; !ok {
			return false
		}
	}
	return true
}

func one(t *testing.T, res []mailstore.RefResult) mailstore.RefResult {
	t.Helper()
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1", len(res))
	}
	return res[0]
}

// TestGmailMove covers the Move rows of the Google table in spec 7.1.
func TestGmailMove(t *testing.T) {
	t.Parallel()
	t.Run("to a label adds it and removes Inbox, keeping the ref", func(t *testing.T) {
		t.Parallel()
		w := newGmailWrites(t)
		r := w.ref(t, allMail, 1)
		if got := one(t, w.store.Move(t.Context(), []ref.Ref{r}, "Work")); got != (mailstore.RefResult{Ref: r}) {
			t.Errorf("Move = %+v, want the ref unchanged", got)
		}
		if got := w.where(t, gwInbox); !in(got, allMail, "Receipts", "Work") {
			t.Errorf("message is in %v, want All Mail, Receipts and Work", got)
		}
	})
	t.Run("to INBOX adds Inbox only", func(t *testing.T) {
		t.Parallel()
		w := newGmailWrites(t)
		r := w.ref(t, allMail, 3)
		if got := one(t, w.store.Move(t.Context(), []ref.Ref{r}, "INBOX")); got != (mailstore.RefResult{Ref: r}) {
			t.Errorf("Move = %+v, want the ref unchanged", got)
		}
		if got := w.where(t, gwWork); !in(got, allMail, "INBOX", "Work") {
			t.Errorf("message is in %v, want All Mail, INBOX and Work", got)
		}
	})
	for _, dest := range []string{gmailTrash, gmailSpam} {
		t.Run("to "+dest+" moves it there under a new ref", func(t *testing.T) {
			t.Parallel()
			w := newGmailWrites(t)
			got := one(t, w.store.Move(t.Context(), []ref.Ref{w.ref(t, allMail, 1)}, dest))
			where := w.where(t, gwInbox)
			if got.Err != nil || got.Ref != w.ref(t, dest, where[dest]) {
				t.Errorf("Move = %+v, want the ref in %s (%v)", got, dest, where)
			}
			if !in(where, dest) {
				t.Errorf("message is in %v, want only %s", where, dest)
			}
		})
	}
	t.Run("from Trash or Spam goes back to All Mail, then takes the label", func(t *testing.T) {
		t.Parallel()
		w := newGmailWrites(t)
		res := w.store.Move(t.Context(), []ref.Ref{w.ref(t, gmailTrash, 301), w.ref(t, gmailSpam, 401)}, "Work")
		for i, msg := range []int{gwTrashed, gwSpammed} {
			where := w.where(t, msg)
			if !in(where, allMail, "Work") {
				t.Errorf("message %d is in %v, want All Mail and Work", msg, where)
			}
			if res[i].Err != nil || res[i].Ref != w.ref(t, allMail, where[allMail]) {
				t.Errorf("Move result %d = %+v, want its All Mail ref", i, res[i])
			}
		}
	})
	t.Run("from Trash to INBOX", func(t *testing.T) {
		t.Parallel()
		w := newGmailWrites(t)
		got := one(t, w.store.Move(t.Context(), []ref.Ref{w.ref(t, gmailTrash, 301)}, "INBOX"))
		where := w.where(t, gwTrashed)
		if !in(where, allMail, "INBOX") || got.Err != nil || got.Ref.Folder != allMail {
			t.Errorf("Move = %+v, message in %v; want it back in All Mail and INBOX", got, where)
		}
	})
	t.Run("into the folder a ref names keeps it", func(t *testing.T) {
		t.Parallel()
		w := newGmailWrites(t)
		r := w.ref(t, gmailTrash, 301)
		if got := one(t, w.store.Move(t.Context(), []ref.Ref{r}, gmailTrash)); got != (mailstore.RefResult{Ref: r}) {
			t.Errorf("Move = %+v, want the ref unchanged", got)
		}
	})
	t.Run("of a ref in INBOX gives its All Mail ref", func(t *testing.T) {
		t.Parallel()
		w := newGmailWrites(t)
		got := one(t, w.store.Move(t.Context(), []ref.Ref{w.ref(t, "INBOX", 102)}, "Work"))
		if got.Err != nil || got.Ref != w.found(t, gwInbox2) {
			t.Errorf("Move = %+v, want the All Mail ref, since the message left INBOX", got)
		}
		if where := w.where(t, gwInbox2); !in(where, allMail, "Work") {
			t.Errorf("message is in %v, want All Mail and Work", where)
		}
	})
	t.Run("to a missing folder is not found", func(t *testing.T) {
		t.Parallel()
		w := newGmailWrites(t)
		if got := one(t, w.store.Move(t.Context(), []ref.Ref{w.ref(t, allMail, 1)}, "Nope")); !errors.Is(got.Err, mailstore.ErrNotFound) {
			t.Errorf("Move = %+v, want ErrNotFound", got)
		}
		if got := w.where(t, gwInbox); !in(got, allMail, "INBOX", "Receipts") {
			t.Errorf("message is in %v; a failed move changes nothing", got)
		}
	})
	t.Run("of a message that is gone is stale", func(t *testing.T) {
		t.Parallel()
		w := newGmailWrites(t)
		r := w.ref(t, allMail, 1)
		gone := r
		gone.UID = 999
		res := w.store.Move(t.Context(), []ref.Ref{gone, r}, "Work")
		if !errors.Is(res[0].Err, mailstore.ErrStaleRef) || res[1] != (mailstore.RefResult{Ref: r}) {
			t.Errorf("Move = %+v, want the gone ref stale and the other moved", res)
		}
	})
}

func TestGmailCopy(t *testing.T) {
	t.Parallel()
	w := newGmailWrites(t)
	r := w.ref(t, allMail, 1)
	if got := one(t, w.store.Copy(t.Context(), []ref.Ref{r}, "Work")); got != (mailstore.RefResult{Ref: r}) {
		t.Errorf("Copy = %+v, want the ref unchanged", got)
	}
	if got := w.where(t, gwInbox); !in(got, allMail, "INBOX", "Receipts", "Work") {
		t.Errorf("message is in %v, want Work added and INBOX kept", got)
	}
	inbox := w.ref(t, "INBOX", 102)
	if got := one(t, w.store.Copy(t.Context(), []ref.Ref{inbox}, "Work")); got != (mailstore.RefResult{Ref: inbox}) {
		t.Errorf("Copy of an INBOX ref = %+v, want it unchanged: the message stays in INBOX", got)
	}
	for _, dest := range []string{gmailTrash, gmailSpam} {
		if got := one(t, w.store.Copy(t.Context(), []ref.Ref{r}, dest)); !errors.Is(got.Err, mailstore.ErrUnsupported) {
			t.Errorf("Copy to %s = %+v, want ErrUnsupported", dest, got)
		}
	}
	if got := w.where(t, gwInbox); !in(got, allMail, "INBOX", "Receipts", "Work") {
		t.Errorf("message is in %v after refused copies", got)
	}
}

func TestGmailArchive(t *testing.T) {
	t.Parallel()
	w := newGmailWrites(t)
	r := w.ref(t, allMail, 1)
	if got := one(t, w.store.Archive(t.Context(), []ref.Ref{r})); got != (mailstore.RefResult{Ref: r}) {
		t.Errorf("Archive = %+v, want the ref unchanged", got)
	}
	if got := w.where(t, gwInbox); !in(got, allMail, "Receipts") {
		t.Errorf("message is in %v, want All Mail and Receipts", got)
	}
	got := one(t, w.store.Archive(t.Context(), []ref.Ref{w.ref(t, "INBOX", 102)}))
	if got.Err != nil || got.Ref != w.found(t, gwInbox2) {
		t.Errorf("Archive of an INBOX ref = %+v, want its All Mail ref", got)
	}
	if got := w.where(t, gwInbox2); !in(got, allMail) {
		t.Errorf("message is in %v, want All Mail only", got)
	}
}

func TestGmailTrash(t *testing.T) {
	t.Parallel()
	w := newGmailWrites(t)
	trashed := w.ref(t, gmailTrash, 301)
	res := w.store.Trash(t.Context(), []ref.Ref{w.ref(t, allMail, 1), trashed})
	where := w.where(t, gwInbox)
	if !in(where, gmailTrash) || res[0].Err != nil || res[0].Ref != w.ref(t, gmailTrash, where[gmailTrash]) {
		t.Errorf("Trash = %+v, message in %v; want it in Trash under its new ref", res[0], where)
	}
	if res[1] != (mailstore.RefResult{Ref: trashed}) {
		t.Errorf("Trash of a trashed ref = %+v, want it unchanged", res[1])
	}
}

func TestGmailExpunge(t *testing.T) {
	t.Parallel()
	w := newGmailWrites(t)
	res := w.store.Expunge(t.Context(), []ref.Ref{w.ref(t, allMail, 1), w.ref(t, "INBOX", 102)})
	for i, r := range res {
		if r != (mailstore.RefResult{}) {
			t.Errorf("Expunge result %d = %+v, want a zero ref", i, r)
		}
	}
	for _, msg := range []int{gwInbox, gwInbox2} {
		if got := w.where(t, msg); len(got) != 0 {
			t.Errorf("message %d is still in %v", msg, got)
		}
	}
	if got := w.where(t, gwTrashed); !in(got, gmailTrash) {
		t.Errorf("the message already in Trash is in %v; expunging others must leave it", got)
	}
	if got := one(t, w.store.Expunge(t.Context(), []ref.Ref{w.ref(t, gmailTrash, 301)})); got != (mailstore.RefResult{}) {
		t.Errorf("Expunge in Trash = %+v", got)
	}
	if got := w.where(t, gwTrashed); len(got) != 0 {
		t.Errorf("the trashed message is still in %v", got)
	}
}

func TestGmailStoreFlagsStars(t *testing.T) {
	t.Parallel()
	w := newGmailWrites(t)
	r := w.ref(t, allMail, 1)
	if got := one(t, w.store.StoreFlags(t.Context(), []ref.Ref{r}, []string{`\Flagged`}, nil)); got != (mailstore.RefResult{Ref: r}) {
		t.Errorf("StoreFlags = %+v", got)
	}
	if got := w.labels(t, gwInbox, allMail); !slices.Contains(got, `\Starred`) {
		t.Errorf("labels = %q, want \\Starred: \\Flagged is the star", got)
	}
}

func TestGmailStoreLabels(t *testing.T) {
	t.Parallel()
	w := newGmailWrites(t)
	r := w.ref(t, allMail, 1)
	odd := []string{`Work "Stuff"`, "Café/Ünï", `back\slash`}
	add := append([]string{`\important`, "work"}, odd...)
	if got := one(t, w.store.StoreLabels(t.Context(), []ref.Ref{r}, add, []string{`\Inbox`, "Receipts"})); got != (mailstore.RefResult{Ref: r}) {
		t.Errorf("StoreLabels = %+v, want the ref unchanged", got)
	}
	want := append([]string{`\Important`, "Work"}, odd...)
	if got := w.labels(t, gwInbox, allMail); !slices.Equal(got, want) {
		t.Errorf("labels = %q, want %q", got, want)
	}
	folders, err := w.store.ListFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range odd {
		if !slices.ContainsFunc(folders, func(f mailstore.Folder) bool { return f.Name == label }) {
			t.Errorf("folders %v lack the new label %q", folders, label)
		}
	}

	t.Run("a ref in the label folder it loses gives its All Mail ref", func(t *testing.T) {
		got := one(t, w.store.StoreLabels(t.Context(), []ref.Ref{w.ref(t, "Work", 1101)}, nil, []string{"Work"}))
		if got.Err != nil || got.Ref != w.found(t, gwWork) {
			t.Errorf("StoreLabels = %+v, want the All Mail ref", got)
		}
	})
	t.Run("folder names stand for system labels", func(t *testing.T) {
		r2 := w.ref(t, allMail, 2)
		if got := one(t, w.store.StoreLabels(t.Context(), []ref.Ref{r2}, []string{"[Gmail]/Sent Mail"}, []string{"INBOX"})); got != (mailstore.RefResult{Ref: r2}) {
			t.Errorf("StoreLabels = %+v", got)
		}
		if got := w.labels(t, gwInbox2, allMail); !slices.Equal(got, []string{`\Sent`}) {
			t.Errorf("labels = %q, want only \\Sent", got)
		}
	})
	t.Run("All Mail, Trash and Spam are not labels", func(t *testing.T) {
		for _, l := range []string{allMail, gmailTrash, "[gmail]/spam"} {
			if got := one(t, w.store.StoreLabels(t.Context(), []ref.Ref{r}, []string{l}, nil)); !errors.Is(got.Err, mailstore.ErrUnsupported) {
				t.Errorf("StoreLabels(%q) = %+v, want ErrUnsupported", l, got)
			}
		}
	})
	t.Run("bad labels are refused before anything is sent", func(t *testing.T) {
		for _, l := range []string{"", `\Bogus`, "a\r\nb"} {
			if got := one(t, w.store.StoreLabels(t.Context(), []ref.Ref{r}, []string{l}, nil)); got.Err == nil {
				t.Errorf("StoreLabels(%q) succeeded", l)
			}
		}
	})
}

func TestGmailWritesKeepGmailMsgIDOnNewRefs(t *testing.T) {
	t.Parallel()
	w := newGmailWrites(t)
	r := w.ref(t, gmailTrash, 301)
	r.GmailMsgID = w.srv.MsgID(gwTrashed)
	got := one(t, w.store.Move(t.Context(), []ref.Ref{r}, "Work"))
	if got.Err != nil || got.Ref.GmailMsgID != r.GmailMsgID {
		t.Errorf("Move = %+v, want the new ref to keep X-GM-MSGID %d", got, r.GmailMsgID)
	}
}

func TestGmailMoveMixedBatch(t *testing.T) {
	t.Parallel()
	w := newGmailWrites(t)
	kept := w.ref(t, allMail, 1)
	res := w.store.Move(t.Context(), []ref.Ref{kept, w.ref(t, gmailTrash, 301), w.ref(t, "INBOX", 102)}, "Receipts")
	if res[0] != (mailstore.RefResult{Ref: kept}) {
		t.Errorf("All Mail ref: %+v, want it unchanged", res[0])
	}
	for i, msg := range map[int]int{1: gwTrashed, 2: gwInbox2} {
		where := w.where(t, msg)
		if !in(where, allMail, "Receipts") || res[i].Err != nil || res[i].Ref.Folder != allMail || res[i].Ref.UID != where[allMail] {
			t.Errorf("result %d = %+v, message in %v; want its All Mail ref, labelled Receipts", i, res[i], where)
		}
	}
}
