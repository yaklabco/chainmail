package fakegmail

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
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
	touch(t, c)
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

// TestFakeGmailLateArrivals checks Options.LateArrivals: a session already
// in a folder learns of a message another session added there only at a
// NOOP, not at a UID SEARCH, UID FETCH or UID STORE, which until then do
// not see it.
func TestFakeGmailLateArrivals(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := StartWith(t, ca, Options{LateArrivals: true}, "me@gmail.example", "pw",
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}},
		Message{Raw: rawTwo, Date: time.Now(), Labels: []string{`\Inbox`}},
	)
	watcher := dial(t, s, ca)
	selectRW(t, watcher, "INBOX")
	writer := dial(t, s, ca)
	uid := appendRaw(t, writer, "INBOX", "Subject: Late\r\n\r\nx\r\n").UID
	set := imap.UIDSetNum(uid)

	found, err := watcher.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil || slices.Contains(found.AllUIDs(), uid) {
		t.Errorf("UID SEARCH before a NOOP = %v, %v; want the arrival %d left out", found.AllUIDs(), err, uid)
	}
	msgs, err := watcher.Fetch(set, &imap.FetchOptions{UID: true}).Collect()
	if err != nil || len(msgs) != 0 {
		t.Errorf("UID FETCH before a NOOP = %v, %v; want nothing", msgs, err)
	}
	msgs, err = watcher.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Collect()
	if err != nil || len(msgs) != 0 {
		t.Errorf("UID STORE before a NOOP = %v, %v; want nothing", msgs, err)
	}
	if n := watcher.Mailbox().NumMessages; n != 2 {
		t.Errorf("INBOX count before a NOOP = %d, want 2", n)
	}

	if err := watcher.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if n := watcher.Mailbox().NumMessages; n != 3 {
		t.Errorf("INBOX count after a NOOP = %d, want 3", n)
	}
	found, err = watcher.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil || !slices.Contains(found.AllUIDs(), uid) {
		t.Errorf("UID SEARCH after a NOOP = %v, %v; want the arrival %d", found.AllUIDs(), err, uid)
	}
	msgs, err = watcher.Fetch(set, &imap.FetchOptions{UID: true}).Collect()
	if err != nil || len(msgs) != 1 {
		t.Errorf("UID FETCH after a NOOP = %v, %v; want the arrival", msgs, err)
	}
}

// TestFakeGmailLateExists checks Options.LateExists: a session already in
// Trash does not learn of a message another session moved there at its
// NOOP, nor see it at the UID FETCH after, which instead announces it, so
// only the command after that sees it. A SELECT sees it at once.
func TestFakeGmailLateExists(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := StartWith(t, ca, Options{LateExists: true}, "me@gmail.example", "pw",
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}},
		Message{Raw: rawTwo, Date: time.Now(), Labels: []string{`\Inbox`}},
	)
	watcher := dial(t, s, ca)
	selectRW(t, watcher, "[Gmail]/Trash")
	if err := watcher.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	writer := dial(t, s, ca)
	selectRW(t, writer, "INBOX")
	moved, err := writer.Move(imap.UIDSetNum(101), "[Gmail]/Trash").Wait()
	if err != nil || moved.DestUIDs == nil {
		t.Fatalf("MOVE = %+v, %v", moved, err)
	}
	set := moved.DestUIDs

	if err := watcher.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if n := watcher.Mailbox().NumMessages; n != 0 {
		t.Errorf("Trash count after a NOOP = %d, want 0", n)
	}
	msgs, err := watcher.Fetch(set, &imap.FetchOptions{UID: true}).Collect()
	if err != nil || len(msgs) != 0 {
		t.Errorf("UID FETCH after the NOOP = %v, %v; want nothing", msgs, err)
	}
	if n := watcher.Mailbox().NumMessages; n != 1 {
		t.Errorf("Trash count after the UID FETCH = %d, want 1", n)
	}
	msgs, err = watcher.Fetch(set, &imap.FetchOptions{UID: true}).Collect()
	if err != nil || len(msgs) != 1 {
		t.Errorf("UID FETCH after it was announced = %v, %v; want the moved message", msgs, err)
	}

	// Another arrival, which a SELECT sees at once.
	moved, err = writer.Move(imap.UIDSetNum(102), "[Gmail]/Trash").Wait()
	if err != nil || moved.DestUIDs == nil {
		t.Fatalf("MOVE = %+v, %v", moved, err)
	}
	if err := watcher.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if data := selectRW(t, watcher, "[Gmail]/Trash"); data.NumMessages != 2 {
		t.Errorf("Trash count at a SELECT = %d, want 2", data.NumMessages)
	}
	msgs, err = watcher.Fetch(moved.DestUIDs, &imap.FetchOptions{UID: true}).Collect()
	if err != nil || len(msgs) != 1 {
		t.Errorf("UID FETCH after a SELECT = %v, %v; want the moved message", msgs, err)
	}
}

// startLabelled runs a fake with one message per label set, in order, so
// message i has All Mail UID i+1.
func startLabelled(t *testing.T, labels ...[]string) (*Server, *tlstest.CA) {
	t.Helper()
	ca := tlstest.New(t)
	msgs := make([]Message, 0, len(labels))
	for i, l := range labels {
		raw := "From: Alex <alex@example.com>\r\nSubject: Fold\r\nMessage-ID: <fold" + string(rune('a'+i)) + "@example.com>\r\n\r\nBody.\r\n"
		msgs = append(msgs, Message{Raw: raw, Date: time.Now(), Labels: l})
	}
	return Start(t, ca, "me@gmail.example", "pw", msgs...), ca
}

// touch appends a message to INBOX, a write that makes the server refresh
// every folder's membership. A flag STORE does not.
func touch(t *testing.T, c *imapclient.Client) {
	t.Helper()
	appendRaw(t, c, "INBOX", "From: Touch <touch@example.com>\r\nSubject: Touch\r\nMessage-ID: <touch@example.com>\r\n\r\nRefresh.\r\n")
}

// TestFakeGmailRenameFoldsSigma checks RENAME relabels every spelling that
// strings.EqualFold matches, as folder lookup does: Σ, σ and ς are one label,
// though Σ and ς lowercase differently.
func TestFakeGmailRenameFoldsSigma(t *testing.T) {
	t.Parallel()
	s, ca := startLabelled(t, []string{"Σ"}, []string{"ς"}, []string{"σ"}, []string{"σ", "ς"})
	c := dial(t, s, ca)
	if err := c.Rename("Σ", "Z", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	touch(t, c)
	// Message 3 carries two spellings of the one label and ends with one Z.
	for i := range 4 {
		if got := fetchIn(t, s, ca, "[Gmail]/All Mail", imap.UID(i+1)).GmailLabels; !reflect.DeepEqual(got, []string{"Z"}) {
			t.Errorf("message %d labels after RENAME Σ Z = %q, want [Z]", i, got)
		}
		if got := where(t, s, ca, s.MsgID(i)); got["Z"] == 0 {
			t.Errorf("message %d is in %v after RENAME Σ Z and a write, want Z", i, got)
		}
	}
}

// TestFakeGmailRenameFoldsKelvin checks the parent and child matching of
// RENAME follows strings.EqualFold when the spellings differ in byte length:
// the Kelvin sign is three bytes and folds with k.
func TestFakeGmailRenameFoldsKelvin(t *testing.T) {
	t.Parallel()
	t.Run("a child under a Kelvin spelling moves with its parent", func(t *testing.T) {
		t.Parallel()
		s, ca := startLabelled(t, []string{"k"}, []string{"K/B"})
		c := dial(t, s, ca)
		if err := c.Rename("K", "Z", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		touch(t, c)
		if got := fetchIn(t, s, ca, "[Gmail]/All Mail", 2).GmailLabels; !reflect.DeepEqual(got, []string{"Z/B"}) {
			t.Errorf("child labels after RENAME K Z = %q, want [Z/B]", got)
		}
		if got := where(t, s, ca, s.MsgID(1)); got["Z/B"] == 0 {
			t.Errorf("child message is in %v, want Z/B", got)
		}
		if names := listNames(t, c); slices.Contains(names, "K/B") {
			t.Errorf("LIST still has the old child after RENAME K Z: %q", names)
		}
	})
	t.Run("a destination beneath a Kelvin spelling is refused", func(t *testing.T) {
		t.Parallel()
		s, ca := startLabelled(t, []string{"k"})
		c := dial(t, s, ca)
		if err := c.Rename("k", "K/C", nil).Wait(); err == nil {
			t.Error("RENAME k \"K/C\" succeeded; a label cannot go beneath itself")
		}
	})
}

// TestFakeGmailRenameKeepsSharpS checks ß stays distinct from ss, as
// strings.EqualFold keeps it, so renaming one leaves the other alone.
func TestFakeGmailRenameKeepsSharpS(t *testing.T) {
	t.Parallel()
	s, ca := startLabelled(t, []string{"ß"}, []string{"ss"})
	c := dial(t, s, ca)
	if err := c.Rename("ß", "Z", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	touch(t, c)
	if got := fetchIn(t, s, ca, "[Gmail]/All Mail", 2).GmailLabels; !reflect.DeepEqual(got, []string{"ss"}) {
		t.Errorf("ss labels after RENAME ß Z = %q, want [ss]", got)
	}
	if got := where(t, s, ca, s.MsgID(1)); got["ss"] == 0 || got["Z"] != 0 {
		t.Errorf("ss message is in %v, want ss and not Z", got)
	}
}

// TestFakeGmailRawInFolds checks X-GM-RAW in: matches folder names the way
// strings.EqualFold does: the long s (ſ) folds with s.
func TestFakeGmailRawInFolds(t *testing.T) {
	t.Parallel()
	s, ca := startLabelled(t, []string{`\Sent`})
	c := dial(t, s, ca)
	if _, err := c.Select("[Gmail]/All Mail", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := c.UIDSearch(&imap.SearchCriteria{GmailRaw: []string{"in:ſent"}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got := data.AllUIDs(); !reflect.DeepEqual(got, []imap.UID{1}) {
		t.Errorf("X-GM-RAW in:ſent = %v, want [1]", got)
	}
}

// TestFoldKeyAgreesWithEqualFold checks foldKey and cutFoldPrefix match label
// names exactly as strings.EqualFold does.
func TestFoldKeyAgreesWithEqualFold(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b string
		want bool
	}{
		{"Σ", "σ", true},
		{"Σ", "ς", true},
		{"σ", "ς", true},
		{"K", "k", true},
		{"k", "K", true},
		{"Receipts", "rECEIPTS", true},
		{"ß", "ss", false},
		{"ß", "SS", false},
		{"ß", "ẞ", true},
		{"ſent", "SENT", true},
		{"a", "b", false},
		{"ab", "a", false},
		{"\xff", "\xfe", true},
	}
	for _, tt := range tests {
		if got := strings.EqualFold(tt.a, tt.b); got != tt.want {
			t.Fatalf("strings.EqualFold(%q, %q) = %v; the table is wrong", tt.a, tt.b, got)
		}
		if got := foldKey(tt.a) == foldKey(tt.b); got != tt.want {
			t.Errorf("foldKey(%q) == foldKey(%q) is %v, want %v", tt.a, tt.b, got, tt.want)
		}
		rest, ok := cutFoldPrefix(tt.a+"/x", tt.b+"/")
		if ok != tt.want || (ok && rest != "x") {
			t.Errorf("cutFoldPrefix(%q, %q) = %q, %v; want \"x\", %v", tt.a+"/x", tt.b+"/", rest, ok, tt.want)
		}
	}
}
