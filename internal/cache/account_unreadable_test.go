package cache

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// unreadable is the rig account's recorded unreadable UIDs.
func (r *acctRig) unreadable() map[mailstore.Source][]uint32 {
	r.t.Helper()
	got, err := r.store.UnreadableUIDs(r.t.Context(), r.acct.Name)
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

// attempts is how many times the rig account's UID in folder was found
// unreadable, 0 when it has no record.
func (r *acctRig) attempts(folder string, uid uint32) int {
	r.t.Helper()
	var n int
	err := r.store.db.QueryRowContext(r.t.Context(),
		"SELECT coalesce(max(attempts), 0) FROM unreadable WHERE account = ? AND folder = ? AND uid = ?",
		r.acct.Name, folder, uid).Scan(&n)
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

// cachedUIDs is the UIDs the cache holds for folder at uidValidity, ascending.
func (r *acctRig) cachedUIDs(folder string, uidValidity uint32) []uint32 {
	r.t.Helper()
	src := mailstore.Source{Folder: folder, UIDValidity: uidValidity}
	uids, _, err := r.store.SearchUIDs(r.t.Context(), r.acct.Name, src, Where{}, 0, false)
	if err != nil {
		r.t.Fatal(err)
	}
	slices.Sort(uids)
	return uids
}

// acctBatchMail is a MailStore that records the size of every header fetch.
type acctBatchMail struct {
	*fake.Store
	mu    sync.Mutex
	sizes []int
}

var _ MailStore = (*acctBatchMail)(nil)

func (m *acctBatchMail) FetchHeaders(ctx context.Context, src mailstore.Source, uids []uint32) ([]mailstore.Header, error) {
	m.mu.Lock()
	m.sizes = append(m.sizes, len(uids))
	m.mu.Unlock()
	return m.Store.FetchHeaders(ctx, src, uids)
}

func (m *acctBatchMail) batches() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.sizes)
}

// inbox is the fake INBOX's source at UIDVALIDITY 1.
var inbox = mailstore.Source{Folder: "INBOX", UIDValidity: 1}

// addInbox adds n messages to the fake's INBOX, the second with an
// attachment, and returns their UIDs.
func addInbox(fs *fake.Store, n int) []uint32 {
	var uids []uint32
	for i := range n {
		m := acctMsg("INBOX", "m"+string(rune('a'+i)))
		if i == 1 {
			m.Attachments = []fake.Attachment{{Part: "2", Filename: "a.pdf", ContentType: "application/pdf", Data: []byte("%PDF")}}
		}
		uids = append(uids, fs.Add(m).UID)
	}
	return uids
}

// A backfill over a message the server's answer cannot be parsed for
// stores the rest, records that UID, and finishes the folder. A message
// read without its body structure is stored with its attachments unknown.
// After a restart with a reader that parses them, the cache reads each
// recorded UID once more: a message now read is stored and its record
// deleted, one still unreadable counts another attempt, and one the folder
// no longer holds loses its record.
func TestBackfillMovesPastAnUnreadableMessage(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	uids := addInbox(fs, 6)
	fs.Malform("INBOX", uids[1], fake.BadStructure)
	for _, i := range []int{2, 3, 4} {
		fs.Malform("INBOX", uids[i], fake.BadEnvelope)
	}
	r.pass("INBOX")
	r.start()
	r.waitBackfilled("INBOX")
	r.waitApplied()
	r.stop()

	if got, want := r.cachedUIDs("INBOX", 1), []uint32{uids[0], uids[1], uids[5]}; !slices.Equal(got, want) {
		t.Errorf("cached UIDs = %v, want %v", got, want)
	}
	if got, want := r.unreadable(), map[mailstore.Source][]uint32{inbox: {uids[2], uids[3], uids[4]}}; !reflect.DeepEqual(got, want) {
		t.Errorf("unreadable = %v, want %v", got, want)
	}
	degraded, _ := r.row(ref.Ref{Account: "acme", Folder: "INBOX", UIDValidity: 1, UID: uids[1]})
	if !degraded.AttachmentsUnknown || degraded.HasAttachment || degraded.Attachments != nil {
		t.Errorf("degraded row: unknown %v, has %v, attachments %v; want unknown and none", degraded.AttachmentsUnknown, degraded.HasAttachment, degraded.Attachments)
	}
	incomplete, err := r.store.Incomplete(t.Context(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if want := []FolderIncomplete{{Folder: "INBOX", Unreadable: 3, AttachmentsUnknown: 1}}; !reflect.DeepEqual(incomplete, want) {
		t.Errorf("Incomplete = %+v, want %+v", incomplete, want)
	}
	if got := r.budget("2026-11-02"); got != 3*headerBudgetBytes {
		t.Errorf("budget = %d, want %d for the three headers read", got, 3*headerBudgetBytes)
	}

	fs.Malform("INBOX", uids[2], fake.WellFormed)
	fs.ExternalExpunge("INBOX", uids[4])
	r.renew()
	r.start()
	r.eventually("the restart to read the recorded messages again", func() (bool, error) {
		return r.attempts("INBOX", uids[3]) == 2, nil
	})
	r.waitApplied()
	if got, want := r.cachedUIDs("INBOX", 1), []uint32{uids[0], uids[1], uids[2], uids[5]}; !slices.Equal(got, want) {
		t.Errorf("cached UIDs after the restart = %v, want %v", got, want)
	}
	if got, want := r.unreadable(), map[mailstore.Source][]uint32{inbox: {uids[3]}}; !reflect.DeepEqual(got, want) {
		t.Errorf("unreadable after the restart = %v, want %v", got, want)
	}
	if got := fs.Calls("FetchHeaders"); got != 2 {
		t.Errorf("FetchHeaders ran %d times, want 2: the backfill, then the one reread", got)
	}
}

// New mail a sync pass cannot read reaches the cache as an unreadable
// event, which the builder records, and which waits in the log through a
// restart like any other event.
func TestBuilderRecordsUnreadableNewMail(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.pass("INBOX") // the baseline of an empty folder
	r.start()
	r.waitApplied()
	r.stop()

	uids := addInbox(fs, 3)
	fs.Malform("INBOX", uids[1], fake.BadEnvelope)
	r.pass("INBOX")
	r.renew()
	r.start()
	r.waitApplied()
	if got, want := r.cachedUIDs("INBOX", 1), []uint32{uids[0], uids[2]}; !slices.Equal(got, want) {
		t.Errorf("cached UIDs = %v, want %v", got, want)
	}
	if got, want := r.unreadable(), map[mailstore.Source][]uint32{inbox: {uids[1]}}; !reflect.DeepEqual(got, want) {
		t.Errorf("unreadable = %v, want %v", got, want)
	}
	if got := fs.Calls("FetchHeaders"); got != 1 {
		t.Errorf("FetchHeaders ran %d times, want 1: the new mail it could read", got)
	}
}

// New mail the pass read but whose headers the builder cannot read is
// stored around the unreadable message, which is recorded. A fetch that
// fails with a plain ErrMalformed is split until each part is isolated.
func TestBuilderRecordsUnreadableHeaders(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		batch int // SetMalformedBatch
		sizes []int
	}{
		{name: "isolated by the store", sizes: []int{4}},
		{name: "split after a plain ErrMalformed", batch: 2, sizes: []int{4, 2, 1, 1, 2}},
		{name: "a single UID still malformed", batch: 1, sizes: []int{4, 2, 1, 1, 2}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := fake.New("acme", false)
			mail := &acctBatchMail{Store: fs}
			r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
			r.mail = mail
			r.pass("INBOX")
			r.start()
			r.waitApplied()
			r.stop()

			uids := addInbox(fs, 4)
			r.pass("INBOX")
			fs.Malform("INBOX", uids[1], fake.BadEnvelope)
			fs.SetMalformedBatch(tt.batch)
			r.renew()
			r.start()
			r.waitApplied()
			if got, want := r.cachedUIDs("INBOX", 1), []uint32{uids[0], uids[2], uids[3]}; !slices.Equal(got, want) {
				t.Errorf("cached UIDs = %v, want %v", got, want)
			}
			if got, want := r.unreadable(), map[mailstore.Source][]uint32{inbox: {uids[1]}}; !reflect.DeepEqual(got, want) {
				t.Errorf("unreadable = %v, want %v", got, want)
			}
			if got := mail.batches(); !slices.Equal(got, tt.sizes) {
				t.Errorf("header fetch sizes = %v, want %v", got, tt.sizes)
			}
			if e := r.a.lastError(); e != "" {
				t.Errorf("last error = %q, want none", e)
			}
		})
	}
}

// A backfill batch that fails with a plain ErrMalformed, which the store
// returns when it cannot isolate the message, is halved for the next
// attempt, down to one UID. A single UID that still fails is recorded as
// unreadable. The batch returns to backfill_batch after a success.
func TestBackfillHalvesItsBatchOnAPlainMalformedResponse(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		batch int // SetMalformedBatch
	}{
		{name: "the store isolates one UID", batch: 3},
		{name: "a single UID still fails", batch: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := fake.New("acme", false)
			mail := &acctBatchMail{Store: fs}
			r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
			r.mail = mail
			uids := addInbox(fs, 8)
			fs.Malform("INBOX", uids[5], fake.BadEnvelope)
			fs.SetMalformedBatch(tt.batch)
			r.setSettings(func(s *config.CacheSettings) { s.BackfillBatch = 8 })
			r.pass("INBOX")
			r.start()
			r.waitBackfilled("INBOX")
			r.stop()

			// 8 fails, 4 fails, 2 (UIDs 8 and 7) succeed; then 6 left: 6
			// fails, 3 fails, 1 (UID 6) is recorded; then the last 5.
			if got, want := mail.batches(), []int{8, 4, 2, 6, 3, 1, 5}; !slices.Equal(got, want) {
				t.Errorf("header fetch sizes = %v, want %v", got, want)
			}
			want := slices.Delete(slices.Clone(uids), 5, 6)
			if got := r.cachedUIDs("INBOX", 1); !slices.Equal(got, want) {
				t.Errorf("cached UIDs = %v, want %v", got, want)
			}
			if got, want := r.unreadable(), map[mailstore.Source][]uint32{inbox: {uids[5]}}; !reflect.DeepEqual(got, want) {
				t.Errorf("unreadable = %v, want %v", got, want)
			}
			if e := r.a.lastError(); e != "" {
				t.Errorf("last error = %q, want none: halving is not a failure", e)
			}
		})
	}
}
