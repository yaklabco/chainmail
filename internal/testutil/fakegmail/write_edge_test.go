package fakegmail

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// starState reads whether the message at All Mail uid is flagged and whether
// it carries \Starred, over the wire.
func starState(t *testing.T, s *Server, ca *tlstest.CA, uid imap.UID) (flagged, starred bool) {
	t.Helper()
	m := fetchIn(t, s, ca, "[Gmail]/All Mail", uid)
	return slices.Contains(m.Flags, imap.FlagFlagged), slices.Contains(m.GmailLabels, `\Starred`)
}

// TestFakeGmailStarLinksFlagAndLabel checks that \Flagged and \Starred are one
// state, as on Gmail, whichever interface changes it.
func TestFakeGmailStarLinksFlagAndLabel(t *testing.T) {
	t.Parallel()
	s, ca := startWrites(t)
	c := dial(t, s, ca)
	selectRW(t, c, "INBOX")
	check := func(step string, wantFlagged bool) {
		t.Helper()
		flagged, starred := starState(t, s, ca, 1)
		if flagged != wantFlagged || starred != wantFlagged {
			t.Errorf("%s: \\Flagged %v, \\Starred %v; want both %v", step, flagged, starred, wantFlagged)
		}
	}
	store := func(op imap.StoreFlagsOp, flags ...imap.Flag) {
		t.Helper()
		if _, err := c.Store(imap.UIDSetNum(101), &imap.StoreFlags{Op: op, Silent: true, Flags: flags}, nil).Collect(); err != nil {
			t.Fatal(err)
		}
	}
	sess := &session{s: s}
	if _, err := sess.Select("[Gmail]/All Mail", nil); err != nil {
		t.Fatal(err)
	}
	labels := func(op imap.StoreFlagsOp, labels ...string) {
		t.Helper()
		if _, err := sess.storeLabels(imap.UIDSetNum(1), op, labels); err != nil {
			t.Fatal(err)
		}
	}

	store(imap.StoreFlagsAdd, imap.FlagFlagged)
	check("+FLAGS (\\Flagged)", true)
	store(imap.StoreFlagsDel, imap.FlagFlagged)
	check("-FLAGS (\\Flagged)", false)
	store(imap.StoreFlagsSet, imap.FlagFlagged, imap.FlagSeen)
	check("FLAGS (\\Flagged \\Seen)", true)
	store(imap.StoreFlagsSet, imap.FlagSeen)
	check("FLAGS (\\Seen)", false)

	labels(imap.StoreFlagsAdd, `\starred`)
	check("+X-GM-LABELS (\\starred)", true)
	labels(imap.StoreFlagsDel, `\Starred`)
	check("-X-GM-LABELS (\\Starred)", false)
	labels(imap.StoreFlagsSet, `\Starred`, "Receipts")
	check("X-GM-LABELS (\\Starred Receipts)", true)
	labels(imap.StoreFlagsSet, "Receipts")
	check("X-GM-LABELS (Receipts)", false)
	if got := s.Flags(0); !reflect.DeepEqual(got, []imap.Flag{imap.FlagSeen}) {
		t.Errorf("flags = %v; a label STORE must change only \\Flagged", got)
	}
}

func TestFakeGmailStarSeedsAgree(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := Start(t, ca, "me@gmail.example", "pw",
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}, Flags: []imap.Flag{imap.FlagFlagged}},
		Message{Raw: rawTwo, Date: time.Now(), Labels: []string{`\Inbox`, `\Starred`}},
	)
	for uid := imap.UID(1); uid <= 2; uid++ {
		if flagged, starred := starState(t, s, ca, uid); !flagged || !starred {
			t.Errorf("seeded message %d: \\Flagged %v, \\Starred %v; want both", uid, flagged, starred)
		}
	}
}

// TestFakeGmailUIDStarAfterRemoval checks * names the highest UID still in the
// folder once the highest has left.
func TestFakeGmailUIDStarAfterRemoval(t *testing.T) {
	t.Parallel()
	s, ca := startWrites(t)
	c := dial(t, s, ca)
	selectRW(t, c, "INBOX")
	if _, err := c.Move(imap.UIDSetNum(102), "[Gmail]/Trash").Wait(); err != nil {
		t.Fatal(err)
	}
	// The client drops FETCH answers for a bare * (UIDSet.Contains reads it
	// as nothing), so UID STORE and FETCH are checked through the session's
	// resolver, which they share with COPY, MOVE, SEARCH and UID EXPUNGE.
	star := imap.UIDSet{{Start: 0, Stop: 0}}
	sess := &session{s: s}
	if _, err := sess.Select("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	idx, err := sess.messagesIn(star)
	var uids []imap.UID
	for _, i := range idx {
		uids = append(uids, sess.view[i].uid)
	}
	s.mu.Unlock()
	if err != nil || !reflect.DeepEqual(uids, []imap.UID{101}) {
		t.Errorf("UID * names %v, %v; want [101], the highest left", uids, err)
	}
	found, err := c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{star}}, nil).Wait()
	if err != nil || !reflect.DeepEqual(found.AllUIDs(), []imap.UID{101}) {
		t.Errorf("UID SEARCH UID * = %v, %v; want [101]", found, err)
	}
	data, err := c.Copy(star, "Receipts").Wait()
	if err != nil || !reflect.DeepEqual(data.SourceUIDs, imap.UIDSetNum(101)) {
		t.Errorf("UID COPY * = %+v, %v; want source UID 101", data, err)
	}
	if _, err := c.Store(imap.UIDSetNum(101), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if seqs, err := c.UIDExpunge(star).Collect(); err != nil || !reflect.DeepEqual(seqs, []uint32{1}) {
		t.Errorf("UID EXPUNGE * = %v, %v; want sequence 1, UID 101", seqs, err)
	}
	// An empty folder has no highest UID, so * names nothing.
	selectRW(t, c, "[Gmail]/Spam")
	if data, err := c.Copy(star, "Receipts").Wait(); err != nil || data.SourceUIDs != nil {
		t.Errorf("UID COPY * in an empty folder = %+v, %v; want OK with no COPYUID", data, err)
	}
}

// TestFakeGmailRenameBeneathItself checks RENAME refuses a destination under
// the source, which would otherwise relabel messages twice.
func TestFakeGmailRenameBeneathItself(t *testing.T) {
	t.Parallel()
	s, ca := startWrites(t)
	c := dial(t, s, ca)
	for _, name := range []string{"A", "A/B"} {
		if err := c.Create(name, nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}
	selectRW(t, c, "INBOX")
	if _, err := c.Copy(imap.UIDSetNum(101), "A").Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Copy(imap.UIDSetNum(102), "A/B").Wait(); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"A/B", "a/b", "A/C", "A/B/C"} {
		if err := c.Rename("A", to, nil).Wait(); err == nil {
			t.Errorf("RENAME A %q succeeded; a label cannot go beneath itself", to)
		}
	}
	names := listNames(t, c)
	for _, want := range []string{"A", "A/B"} {
		if !slices.Contains(names, want) {
			t.Errorf("LIST lacks %q after refused RENAMEs: %v", want, names)
		}
	}
	// A further write refreshes membership; both messages keep their folders.
	if _, err := c.Store(imap.UIDSetNum(101), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if got := where(t, s, ca, s.MsgID(0)); got["A"] == 0 || got["A/B"] != 0 {
		t.Errorf("message 0 is in %v, want A only of the two", got)
	}
	if got := where(t, s, ca, s.MsgID(1)); got["A/B"] == 0 || got["A"] != 0 {
		t.Errorf("message 1 is in %v, want A/B only of the two", got)
	}

	// A plain parent rename still carries its child.
	if err := c.Rename("A", "Z", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if got := fetchIn(t, s, ca, "[Gmail]/All Mail", 2).GmailLabels; !reflect.DeepEqual(got, []string{`\Inbox`, "Z/B"}) {
		t.Errorf("labels after RENAME A Z = %q, want [\\Inbox Z/B]", got)
	}
}

// TestFakeGmailSeededDeleted checks a fixture's \Deleted acts like one set by
// STORE: EXPUNGE removes it and -FLAGS clears it.
func TestFakeGmailSeededDeleted(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := Start(t, ca, "me@gmail.example", "pw",
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}, Flags: []imap.Flag{imap.FlagDeleted, imap.FlagSeen}},
		Message{Raw: rawTwo, Date: time.Now(), Labels: []string{`\Inbox`}, Flags: []imap.Flag{imap.FlagDeleted}},
	)
	c := dial(t, s, ca)
	selectRW(t, c, "INBOX")
	msgs, err := c.Store(imap.UIDSetNum(102), &imap.StoreFlags{Op: imap.StoreFlagsDel, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Collect()
	if err != nil || len(msgs) != 1 || slices.Contains(msgs[0].Flags, imap.FlagDeleted) {
		t.Errorf("STORE -FLAGS (\\Deleted) on a seeded deletion = %+v, %v; want it cleared", msgs, err)
	}
	seqs, err := c.Expunge().Collect()
	if err != nil || !reflect.DeepEqual(seqs, []uint32{1}) {
		t.Errorf("EXPUNGE = %v, %v; want the seeded deletion, sequence 1", seqs, err)
	}
	if got := where(t, s, ca, s.MsgID(0)); !reflect.DeepEqual(got, map[string]imap.UID{"[Gmail]/All Mail": 1}) {
		t.Errorf("expunged message is in %v, want All Mail only", got)
	}
	if got := where(t, s, ca, s.MsgID(1)); got["INBOX"] != 102 {
		t.Errorf("undeleted message is in %v; it must stay in INBOX", got)
	}
	if got := s.Flags(0); !reflect.DeepEqual(got, []imap.Flag{imap.FlagSeen}) {
		t.Errorf("flags = %v; \\Deleted is per folder, not in the shared flags", got)
	}
}

// TestFakeGmailSeqSetBeforeExists checks a sequence set never reaches a
// message the client has not heard of through EXISTS.
func TestFakeGmailSeqSetBeforeExists(t *testing.T) {
	t.Parallel()
	s, ca := startWrites(t)
	watcher := dial(t, s, ca)
	selectRW(t, watcher, "INBOX")
	writer := dial(t, s, ca)
	appendRaw(t, writer, "INBOX", "Subject: Late\r\n\r\nx\r\n")

	msgs, err := watcher.Fetch(imap.SeqSet{{Start: 1, Stop: 0}}, &imap.FetchOptions{UID: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.SeqNum > 2 {
			t.Errorf("FETCH 1:* answered sequence %d before EXISTS announced it", m.SeqNum)
		}
	}
	if n := watcher.Mailbox().NumMessages; n != 3 {
		t.Errorf("INBOX count after FETCH = %d, want 3 from the EXISTS that ends it", n)
	}
	msgs, err = watcher.Fetch(imap.SeqSet{{Start: 1, Stop: 0}}, &imap.FetchOptions{UID: true}).Collect()
	if err != nil || len(msgs) != 3 {
		t.Errorf("FETCH 1:* after EXISTS = %d messages, %v; want 3", len(msgs), err)
	}
}
