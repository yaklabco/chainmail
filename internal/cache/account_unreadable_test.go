package cache

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/watch"
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

// acctBatchMail is a MailStore that records the size of every header
// fetch. When failAt is set, the header fetch with that number, counting
// from 1, fails with mailstore.ErrUnavailable.
type acctBatchMail struct {
	*fake.Store
	failAt int
	mu     sync.Mutex
	sizes  []int
}

var _ MailStore = (*acctBatchMail)(nil)

func (m *acctBatchMail) FetchHeaders(ctx context.Context, src mailstore.Source, uids []uint32) ([]mailstore.Header, error) {
	m.mu.Lock()
	m.sizes = append(m.sizes, len(uids))
	fail := len(m.sizes) == m.failAt
	m.mu.Unlock()
	if fail {
		return nil, fmt.Errorf("%w: the connection dropped", mailstore.ErrUnavailable)
	}
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
		m := acctMsg("INBOX", fmt.Sprintf("m%d", i))
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

// The reread at start fetches at most four UIDs at a time, so the store
// always isolates them, and takes at most 200 records, those attempted
// longest ago. A reread that fails part way resumes after the chunks it
// committed, so no attempt is counted twice.
func TestRereadChunksCapsAndResumes(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	uids := addInbox(fs, 205)
	for _, uid := range uids {
		fs.Malform("INBOX", uid, fake.BadEnvelope)
	}
	r.pass("INBOX")
	r.start()
	r.waitBackfilled("INBOX")
	r.stop()
	// The first five were attempted again since, so they are the newest.
	r.clock.Advance(time.Minute)
	if err := r.store.MarkUnreadable(t.Context(), "acme", inbox, uids[:5]); err != nil {
		t.Fatal(err)
	}
	for _, uid := range uids[9:13] {
		fs.Malform("INBOX", uid, fake.WellFormed)
	}

	mail := &acctBatchMail{Store: fs, failAt: 2}
	r.mail = mail
	r.renew()
	r.start()
	r.clock.expectAfter(t, time.Second) // the builder's backoff after the failed chunk
	r.clock.Advance(time.Second)
	r.eventually("the reread of the last chunk", func() (bool, error) {
		return r.attempts("INBOX", uids[204]) == 2, nil
	})
	r.stop()

	sizes := mail.batches()
	if len(sizes) != 51 || slices.ContainsFunc(sizes, func(n int) bool { return n != 4 }) {
		t.Errorf("header fetch sizes = %v, want 51 fetches of 4: 200 records, one chunk again after the failure", sizes)
	}
	for i, uid := range uids {
		want := 2
		if i >= 9 && i < 13 {
			want = 0 // read now, so the record is gone
		}
		if got := r.attempts("INBOX", uid); got != want {
			t.Errorf("UID %d: %d attempts, want %d", uid, got, want)
		}
	}
	if got, want := r.cachedUIDs("INBOX", 1), uids[9:13]; !slices.Equal(got, want) {
		t.Errorf("cached UIDs = %v, want %v", got, want)
	}
}

// The reread leaves a folder in error for a later start: it fetches
// nothing there and counts no attempt, and the builder goes on.
func TestRereadSkipsAFolderInError(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	uids := addInbox(fs, 3)
	fs.Malform("INBOX", uids[1], fake.BadEnvelope)
	r.pass("INBOX")
	r.start()
	r.waitBackfilled("INBOX")
	r.waitApplied()
	r.stop()

	fs.Malform("INBOX", uids[1], fake.WellFormed)
	r.setState("INBOX", watch.StateError)
	r.publish(watch.Event{At: acctStart, Kind: watch.KindStatus, Account: "acme", Folder: "INBOX", State: watch.StateError})
	calls := fs.Calls("FetchHeaders")
	r.renew()
	r.start()
	r.waitApplied() // the builder applies events only after its reread
	if got := fs.Calls("FetchHeaders"); got != calls {
		t.Errorf("FetchHeaders ran %d more times, want none for a folder in error", got-calls)
	}
	if got := r.attempts("INBOX", uids[1]); got != 1 {
		t.Errorf("%d attempts, want 1", got)
	}
}

// New mail a sync pass cannot read reaches the cache as an unreadable
// event, which the builder records, and which waits in the log through a
// restart like any other event. Its expunge later ends the record.
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

	// The engine knows the UID, so a sweep reports it expunged, and the
	// builder forgets the record.
	fs.ExternalExpunge("INBOX", uids[1])
	r.sweep("INBOX")
	r.waitApplied()
	if got := r.unreadable(); len(got) != 0 {
		t.Errorf("unreadable after the expunge = %v, want none", got)
	}
}

// New mail the pass read but whose headers the builder cannot read is
// stored around the unreadable message, which is recorded. A fetch larger
// than mailstore.IsolatedBatch that fails with a plain ErrMalformed is
// split until the store isolates each part.
func TestBuilderRecordsUnreadableHeaders(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		n     int // new messages, the second unreadable
		batch int // SetMalformedBatch
		sizes []int
	}{
		{name: "isolated by the store", n: 4, sizes: []int{4}},
		{name: "split after a plain ErrMalformed", n: 12, batch: 6, sizes: []int{12, 6, 3, 3, 6}},
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

			uids := addInbox(fs, tt.n)
			r.pass("INBOX")
			fs.Malform("INBOX", uids[1], fake.BadEnvelope)
			fs.SetMalformedBatch(tt.batch)
			r.renew()
			r.start()
			r.waitApplied()
			if got, want := r.cachedUIDs("INBOX", 1), slices.Delete(slices.Clone(uids), 1, 2); !slices.Equal(got, want) {
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

// errFolderMalformed is a plain ErrMalformed, which a fetch of
// mailstore.IsolatedBatch UIDs or fewer returns only for a fault in the
// folder, such as a SELECT answer the reader cannot parse.
var errFolderMalformed = fmt.Errorf("%w: the folder's SELECT answer", mailstore.ErrMalformed)

// A plain ErrMalformed that splitting cannot explain is a fault in the
// folder: the builder's step fails with MALFORMED_RESPONSE and backs off,
// records nothing, and applies the events once the folder reads again.
func TestBuilderBacksOffAFolderFault(t *testing.T) {
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
	fs.Fail("FetchHeaders", errFolderMalformed)
	r.renew()
	r.start()
	r.clock.expectAfter(t, time.Second) // the builder's first backoff
	if e := r.a.lastError(); e != api.ReasonMalformedResponse {
		t.Errorf("last error = %q, want %q", e, api.ReasonMalformedResponse)
	}
	if got := r.unreadable(); len(got) != 0 {
		t.Errorf("unreadable = %v, want none for a folder fault", got)
	}
	if got, want := mail.batches(), []int{4}; !slices.Equal(got, want) {
		t.Errorf("header fetch sizes = %v, want %v: no split of what the store isolates", got, want)
	}
	if r.acked() == r.tail() {
		t.Error("the builder acknowledged events it could not apply")
	}
	fs.Fail("FetchHeaders", nil)
	r.clock.Advance(time.Second)
	r.waitApplied()
	if got := r.cachedUIDs("INBOX", 1); !slices.Equal(got, uids) {
		t.Errorf("cached UIDs = %v, want %v", got, uids)
	}
}

// A backfill batch larger than mailstore.IsolatedBatch that fails with a
// plain ErrMalformed, which the store returns when it cannot isolate the
// messages at fault, is halved for the next attempt until the store
// isolates them. The walk then goes on past the unreadable message, and
// each committed batch doubles the limit back towards backfill_batch.
func TestBackfillHalvesItsBatchOnAPlainMalformedResponse(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	mail := &acctBatchMail{Store: fs}
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.mail = mail
	uids := addInbox(fs, 24)
	fs.Malform("INBOX", uids[19], fake.BadEnvelope)
	fs.SetMalformedBatch(6)
	r.setSettings(func(s *config.CacheSettings) { s.BackfillBatch = 16 })
	r.pass("INBOX")
	r.start()
	r.waitBackfilled("INBOX")
	r.stop()

	// Highest first: 24 to 9 fails, 24 to 17 fails, 24 to 21 commits and
	// the limit doubles to 8; 20 to 13 fails, 20 to 17 commits with 20
	// unreadable and the limit doubles to 8; 16 to 9 commits and the limit
	// is back at 16; then 8 to 1.
	if got, want := mail.batches(), []int{16, 8, 4, 8, 4, 8, 8}; !slices.Equal(got, want) {
		t.Errorf("header fetch sizes = %v, want %v", got, want)
	}
	if got, want := r.cachedUIDs("INBOX", 1), slices.Delete(slices.Clone(uids), 19, 20); !slices.Equal(got, want) {
		t.Errorf("cached UIDs = %v, want %v", got, want)
	}
	if got, want := r.unreadable(), map[mailstore.Source][]uint32{inbox: {uids[19]}}; !reflect.DeepEqual(got, want) {
		t.Errorf("unreadable = %v, want %v", got, want)
	}
	if e := r.a.lastError(); e != "" {
		t.Errorf("last error = %q, want none: halving is not a failure", e)
	}
}

// A plain ErrMalformed for a batch the store always isolates is a fault in
// the folder: the backfill fails with MALFORMED_RESPONSE and backs off,
// records nothing, and finishes once the folder reads again.
func TestBackfillBacksOffAFolderFault(t *testing.T) {
	t.Parallel()
	fs := fake.New("acme", false)
	mail := &acctBatchMail{Store: fs}
	r := newAcctRig(t, fs, acctAccount(t, "hostinger", []string{"INBOX"}, []string{"INBOX"}))
	r.mail = mail
	uids := addInbox(fs, 24)
	fs.Fail("FetchHeaders", errFolderMalformed)
	r.setSettings(func(s *config.CacheSettings) { s.BackfillBatch = 16 })
	r.pass("INBOX")
	r.start()
	r.clock.expectAfter(t, time.Second) // the backfill's first backoff
	if got, want := mail.batches(), []int{16, 8, 4}; !slices.Equal(got, want) {
		t.Errorf("header fetch sizes = %v, want %v: halving stops where the store isolates", got, want)
	}
	if e := r.a.lastError(); e != api.ReasonMalformedResponse {
		t.Errorf("last error = %q, want %q", e, api.ReasonMalformedResponse)
	}
	if got := r.unreadable(); len(got) != 0 {
		t.Errorf("unreadable = %v, want none for a folder fault", got)
	}
	if p := r.progress("INBOX"); p.BackfillDone {
		t.Error("backfill done after a folder fault")
	}
	fs.Fail("FetchHeaders", nil)
	r.clock.Advance(time.Second)
	r.waitBackfilled("INBOX")
	if got := r.cachedUIDs("INBOX", 1); !slices.Equal(got, uids) {
		t.Errorf("cached UIDs = %v, want %v", got, uids)
	}
}
