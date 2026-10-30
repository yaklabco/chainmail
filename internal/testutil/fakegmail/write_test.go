package fakegmail

import (
	"io"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

const (
	rawTwo   = "From: Bo <bo@example.com>\r\nSubject: Two\r\nMessage-ID: <b@example.com>\r\n\r\nSecond.\r\n"
	rawThree = "From: Cy <cy@example.com>\r\nSubject: Three\r\nMessage-ID: <c@example.com>\r\n\r\nThird.\r\n"
)

// startWrites runs a fake with three messages:
//
//	0: INBOX 101, Receipts 1001, All Mail 1
//	1: INBOX 102, All Mail 2
//	2: Trash 301
func startWrites(t *testing.T) (*Server, *tlstest.CA) {
	t.Helper()
	ca := tlstest.New(t)
	s := Start(t, ca, "me@gmail.example", "pw",
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`, "Receipts"}},
		Message{Raw: rawTwo, Date: time.Now(), Labels: []string{`\Inbox`}},
		Message{Raw: rawThree, Date: time.Now(), Trash: true},
	)
	return s, ca
}

func selectRW(t *testing.T, c *imapclient.Client, name string) *imap.SelectData {
	t.Helper()
	data, err := c.Select(name, nil).Wait()
	if err != nil {
		t.Fatalf("SELECT %s: %v", name, err)
	}
	return data
}

// where answers which folders hold the message with msgID, and its UID in
// each, by asking the server over a fresh read-only connection.
func where(t *testing.T, s *Server, ca *tlstest.CA, msgID uint64) map[string]imap.UID {
	t.Helper()
	c := dial(t, s, ca)
	defer func() { _ = c.Logout().Wait() }()
	list, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]imap.UID{}
	for _, l := range list {
		if slices.Contains(l.Attrs, imap.MailboxAttrNoSelect) {
			continue
		}
		if _, err := c.Select(l.Mailbox, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			t.Fatal(err)
		}
		data, err := c.UIDSearch(&imap.SearchCriteria{GmailMsgID: []uint64{msgID}}, nil).Wait()
		if err != nil {
			t.Fatal(err)
		}
		for _, uid := range data.AllUIDs() {
			got[l.Mailbox] = uid
		}
	}
	return got
}

// fetchIn reads one message by UID from a folder over a fresh read-only connection.
func fetchIn(t *testing.T, s *Server, ca *tlstest.CA, folder string, uid imap.UID) *imapclient.FetchMessageBuffer {
	t.Helper()
	c := dial(t, s, ca)
	defer func() { _ = c.Logout().Wait() }()
	if _, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	msgs, err := c.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
		UID: true, Flags: true, GmailMsgID: true, GmailThreadID: true, GmailLabels: true,
		BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("UID FETCH %d in %s = %d messages, want 1", uid, folder, len(msgs))
	}
	return msgs[0]
}

func listNames(t *testing.T, c *imapclient.Client) []string {
	t.Helper()
	list, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, l := range list {
		names = append(names, l.Mailbox)
	}
	return names
}

func TestFakeGmailStoreFlags(t *testing.T) {
	t.Parallel()
	s, ca := startWrites(t)
	c := dial(t, s, ca)
	selectRW(t, c, "INBOX")

	msgs, err := c.Store(imap.UIDSetNum(101), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].UID != 101 || !slices.Contains(msgs[0].Flags, imap.FlagFlagged) {
		t.Errorf("STORE +FLAGS answered %+v, want one FETCH for UID 101 with \\Flagged", msgs)
	}
	if !slices.Contains(s.Flags(0), imap.FlagFlagged) {
		t.Errorf("flags = %v, want \\Flagged", s.Flags(0))
	}
	if got := fetchIn(t, s, ca, "[Gmail]/All Mail", 1).Flags; !slices.Contains(got, imap.FlagFlagged) {
		t.Errorf("All Mail flags = %v; flags belong to the message, not the folder", got)
	}

	msgs, err = c.Store(imap.UIDSetNum(101), &imap.StoreFlags{Op: imap.StoreFlagsDel, Silent: true, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Collect()
	if err != nil || len(msgs) != 0 {
		t.Errorf("STORE -FLAGS.SILENT = %v, %v; want no FETCH", msgs, err)
	}
	if slices.Contains(s.Flags(0), imap.FlagFlagged) {
		t.Errorf("flags = %v after -FLAGS", s.Flags(0))
	}

	if _, err := c.Store(imap.UIDSetNum(101), &imap.StoreFlags{Op: imap.StoreFlagsSet, Flags: []imap.Flag{imap.FlagSeen, imap.FlagAnswered}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if got := s.Flags(0); !reflect.DeepEqual(got, []imap.Flag{imap.FlagSeen, imap.FlagAnswered}) {
		t.Errorf("flags after FLAGS replace = %v", got)
	}

	msgs, err = c.Store(imap.UIDSetNum(999), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Collect()
	if err != nil || len(msgs) != 0 {
		t.Errorf("STORE on a stale UID = %v, %v; Gmail answers OK with no FETCH", msgs, err)
	}

	// \Deleted is kept per folder: marking it in INBOX leaves All Mail alone.
	msgs, err = c.Store(imap.UIDSetNum(101), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Collect()
	if err != nil || len(msgs) != 1 || !slices.Contains(msgs[0].Flags, imap.FlagDeleted) {
		t.Errorf("STORE +FLAGS (\\Deleted) = %v, %v", msgs, err)
	}
	if got := fetchIn(t, s, ca, "[Gmail]/All Mail", 1).Flags; slices.Contains(got, imap.FlagDeleted) {
		t.Errorf("All Mail flags = %v; \\Deleted in INBOX must not show in All Mail", got)
	}
	if got := s.WritableSelects(); got != 1 {
		t.Errorf("WritableSelects = %d, want 1", got)
	}
}

func TestFakeGmailRefusesWritesAfterExamine(t *testing.T) {
	t.Parallel()
	s, ca := startWrites(t)
	c := dial(t, s, ca)
	if _, err := c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store(imap.UIDSetNum(101), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Collect(); err == nil {
		t.Error("STORE after EXAMINE succeeded")
	}
	if _, err := c.Move(imap.UIDSetNum(101), "[Gmail]/Trash").Wait(); err == nil {
		t.Error("MOVE after EXAMINE succeeded")
	}
	if _, err := c.UIDExpunge(imap.UIDSetNum(101)).Collect(); err == nil {
		t.Error("UID EXPUNGE after EXAMINE succeeded")
	}
	if slices.Contains(s.Flags(0), imap.FlagFlagged) {
		t.Error("a refused STORE changed flags")
	}
	sess := &session{s: s}
	if _, err := sess.Select("INBOX", &imap.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.storeLabels(imap.UIDSetNum(101), imap.StoreFlagsAdd, []string{"Paid"}); err == nil {
		t.Error("STORE X-GM-LABELS after EXAMINE succeeded")
	}
}

// TestFakeGmailStoreLabels drives the X-GM-LABELS store directly: the go-imap
// fork parses STORE FLAGS only, so no client can send it yet.
func TestFakeGmailStoreLabels(t *testing.T) {
	t.Parallel()
	s, ca := startWrites(t)
	sess := &session{s: s}
	if _, err := sess.Select("[Gmail]/All Mail", nil); err != nil {
		t.Fatal(err)
	}
	upd, err := sess.storeLabels(imap.UIDSetNum(1), imap.StoreFlagsAdd, []string{`\important`, "Work Stuff", "Projects/2026", "receipts"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`\Inbox`, "Receipts", `\Important`, "Work Stuff", "Projects/2026"}
	if len(upd) != 1 || upd[0].seq != 1 || upd[0].uid != 1 || !reflect.DeepEqual(upd[0].labels, want) {
		t.Errorf("+X-GM-LABELS answered %+v, want seq 1 UID 1 labels %q", upd, want)
	}
	// The labels come back over the wire quoted as Gmail quotes them.
	if got := fetchIn(t, s, ca, "[Gmail]/All Mail", 1).GmailLabels; !reflect.DeepEqual(got, want) {
		t.Errorf("FETCH X-GM-LABELS = %q, want %q", got, want)
	}
	c := dial(t, s, ca)
	names := listNames(t, c)
	for _, label := range []string{"Work Stuff", "Projects/2026"} {
		if !slices.Contains(names, label) {
			t.Errorf("LIST lacks the new label %q: %v", label, names)
		}
	}
	if got := where(t, s, ca, s.MsgID(0)); got["Work Stuff"] == 0 || got["Projects/2026"] == 0 {
		t.Errorf("message is in %v, want it in its new label folders", got)
	}

	if _, err := sess.storeLabels(imap.UIDSetNum(1), imap.StoreFlagsDel, []string{`\Inbox`}); err != nil {
		t.Fatal(err)
	}
	got := where(t, s, ca, s.MsgID(0))
	if _, ok := got["INBOX"]; ok {
		t.Errorf("message is in %v after -X-GM-LABELS (\\Inbox)", got)
	}
	if got["[Gmail]/All Mail"] != 1 {
		t.Errorf("message is in %v; a label change keeps its All Mail UID", got)
	}

	upd, err = sess.storeLabels(imap.UIDSetNum(1), imap.StoreFlagsSet, []string{"Receipts"})
	if err != nil || len(upd) != 1 || !reflect.DeepEqual(upd[0].labels, []string{"Receipts"}) {
		t.Errorf("X-GM-LABELS replace = %+v, %v", upd, err)
	}

	upd, err = sess.storeLabels(imap.UIDSetNum(999), imap.StoreFlagsAdd, []string{"Receipts"})
	if err != nil || len(upd) != 0 {
		t.Errorf("label STORE on a stale UID = %+v, %v; want OK with no FETCH", upd, err)
	}
}

func TestFakeGmailCopy(t *testing.T) {
	t.Parallel()
	s, ca := startWrites(t)
	c := dial(t, s, ca)
	selectRW(t, c, "INBOX")

	if _, err := c.Copy(imap.UIDSetNum(102), "Taxes").Wait(); err == nil {
		t.Error("COPY to a missing folder succeeded")
	}
	data, err := c.Copy(imap.UIDSetNum(102), "Receipts").Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(data.SourceUIDs, imap.UIDSetNum(102)) || !reflect.DeepEqual(data.DestUIDs, imap.UIDSetNum(1002)) || data.UIDValidity == 0 {
		t.Errorf("COPYUID = %+v, want 102 -> 1002", data)
	}
	if m := fetchIn(t, s, ca, "Receipts", 1002); m.GmailMsgID != s.MsgID(1) {
		t.Errorf("Receipts UID 1002 is X-GM-MSGID %d, want %d", m.GmailMsgID, s.MsgID(1))
	}
	sel, err := c.Select("Receipts", &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil || sel.UIDValidity != data.UIDValidity {
		t.Errorf("Receipts UIDVALIDITY = %v, %v; COPYUID said %d", sel, err, data.UIDValidity)
	}
	if got := where(t, s, ca, s.MsgID(1)); got["INBOX"] != 102 {
		t.Errorf("message is in %v; COPY must leave it in INBOX", got)
	}

	selectRW(t, c, "INBOX")
	data, err = c.Copy(imap.UIDSetNum(101), "Receipts").Wait()
	if err != nil || !reflect.DeepEqual(data.DestUIDs, imap.UIDSetNum(1001)) {
		t.Errorf("COPY of a message already labelled = %+v, %v; want its existing UID 1001", data, err)
	}
	data, err = c.Copy(imap.UIDSetNum(999), "Receipts").Wait()
	if err != nil || data.DestUIDs != nil {
		t.Errorf("COPY of a stale UID = %+v, %v; want OK with no COPYUID", data, err)
	}
}

func TestFakeGmailMove(t *testing.T) {
	t.Parallel()
	s, ca := startWrites(t)
	c := dial(t, s, ca)
	selectRW(t, c, "INBOX")

	data, err := c.Move(imap.UIDSetNum(102), "[Gmail]/Trash").Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(data.SourceUIDs, imap.UIDSetNum(102)) || !reflect.DeepEqual(data.DestUIDs, imap.UIDSetNum(302)) {
		t.Errorf("MOVE COPYUID = %+v, want 102 -> 302", data)
	}
	if n := c.Mailbox().NumMessages; n != 1 {
		t.Errorf("INBOX has %d messages after MOVE, want 1 (the EXPUNGE was not sent)", n)
	}
	if got := where(t, s, ca, s.MsgID(1)); !reflect.DeepEqual(got, map[string]imap.UID{"[Gmail]/Trash": 302}) {
		t.Errorf("trashed message is in %v, want Trash only", got)
	}

	// A move between labels swaps them; the All Mail UID stays.
	if _, err := c.Move(imap.UIDSetNum(101), "Paid").Wait(); err == nil {
		t.Error("MOVE to a missing folder succeeded")
	}
	data, err = c.Move(imap.UIDSetNum(101), "Receipts").Wait()
	if err != nil || !reflect.DeepEqual(data.DestUIDs, imap.UIDSetNum(1001)) {
		t.Errorf("MOVE INBOX -> Receipts = %+v, %v; want dest 1001", data, err)
	}
	if got := fetchIn(t, s, ca, "[Gmail]/All Mail", 1).GmailLabels; !reflect.DeepEqual(got, []string{"Receipts"}) {
		t.Errorf("labels after MOVE = %q, want [Receipts]", got)
	}

	// Out of Trash: a new All Mail UID, as Gmail gives.
	selectRW(t, c, "[Gmail]/Trash")
	data, err = c.Move(imap.UIDSetNum(302), "INBOX").Wait()
	if err != nil || !reflect.DeepEqual(data.DestUIDs, imap.UIDSetNum(103)) {
		t.Errorf("MOVE Trash -> INBOX = %+v, %v; want dest 103", data, err)
	}
	if got := where(t, s, ca, s.MsgID(1)); !reflect.DeepEqual(got, map[string]imap.UID{"INBOX": 103, "[Gmail]/All Mail": 3}) {
		t.Errorf("restored message is in %v", got)
	}
	if m := fetchIn(t, s, ca, "INBOX", 103); m.GmailMsgID != s.MsgID(1) {
		t.Errorf("INBOX UID 103 is X-GM-MSGID %d, want %d", m.GmailMsgID, s.MsgID(1))
	}
}

func TestFakeGmailAppend(t *testing.T) {
	t.Parallel()
	tests := []struct {
		folder, label string
	}{
		{"INBOX", `\Inbox`},
		{"[Gmail]/Sent Mail", `\Sent`},
		{"[Gmail]/Drafts", `\Draft`},
		{"Receipts", "Receipts"},
	}
	for _, tt := range tests {
		t.Run(tt.folder, func(t *testing.T) {
			t.Parallel()
			s, ca := startWrites(t)
			c := dial(t, s, ca)
			const raw = "From: Me <me@gmail.example>\r\nSubject: New\r\nMessage-ID: <new@gmail.example>\r\n\r\nHi.\r\n"
			data := appendRaw(t, c, tt.folder, raw, imap.FlagSeen)
			sel, err := c.Select(tt.folder, &imap.SelectOptions{ReadOnly: true}).Wait()
			if err != nil {
				t.Fatal(err)
			}
			if data.UIDValidity != sel.UIDValidity || data.UID+1 != sel.UIDNext {
				t.Errorf("APPENDUID %d %d, folder UIDVALIDITY %d UIDNEXT %d", data.UIDValidity, data.UID, sel.UIDValidity, sel.UIDNext)
			}
			m := fetchIn(t, s, ca, tt.folder, data.UID)
			if !slices.Contains(m.GmailLabels, tt.label) || !slices.Contains(m.Flags, imap.FlagSeen) {
				t.Errorf("appended message labels %q flags %v, want %s and \\Seen", m.GmailLabels, m.Flags, tt.label)
			}
			if got := string(m.FindBodySection(&imap.FetchItemBodySection{Peek: true})); got != raw {
				t.Errorf("body = %q, want %q", got, raw)
			}
			if got := where(t, s, ca, m.GmailMsgID); got["[Gmail]/All Mail"] != 3 {
				t.Errorf("appended message is in %v, want All Mail 3 too", got)
			}
		})
	}
	t.Run("missing folder", func(t *testing.T) {
		t.Parallel()
		s, ca := startWrites(t)
		c := dial(t, s, ca)
		cmd := c.Append("Nope", 4, nil)
		_, _ = cmd.Write([]byte("x\r\n\r"))
		_ = cmd.Close()
		if _, err := cmd.Wait(); err == nil {
			t.Error("APPEND to a missing folder succeeded")
		}
	})
}

func appendRaw(t *testing.T, c *imapclient.Client, folder, raw string, flags ...imap.Flag) *imap.AppendData {
	t.Helper()
	cmd := c.Append(folder, int64(len(raw)), &imap.AppendOptions{Flags: flags})
	if _, err := io.WriteString(cmd, raw); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := cmd.Wait()
	if err != nil {
		t.Fatalf("APPEND %s: %v", folder, err)
	}
	return data
}

func TestFakeGmailExpunge(t *testing.T) {
	t.Parallel()
	deleted := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}

	t.Run("All Mail moves to Trash", func(t *testing.T) {
		t.Parallel()
		s, ca := startWrites(t)
		c := dial(t, s, ca)
		selectRW(t, c, "[Gmail]/All Mail")
		if _, err := c.Store(imap.UIDSetNum(2), deleted, nil).Collect(); err != nil {
			t.Fatal(err)
		}
		seqs, err := c.UIDExpunge(imap.UIDSetNum(1, 2)).Collect()
		if err != nil || !reflect.DeepEqual(seqs, []uint32{2}) {
			t.Errorf("UID EXPUNGE = %v, %v; want sequence 2 only", seqs, err)
		}
		if got := where(t, s, ca, s.MsgID(1)); !reflect.DeepEqual(got, map[string]imap.UID{"[Gmail]/Trash": 302}) {
			t.Errorf("expunged message is in %v, want Trash", got)
		}
		if got := where(t, s, ca, s.MsgID(0)); got["[Gmail]/All Mail"] != 1 {
			t.Errorf("unflagged message is in %v; it must stay", got)
		}
	})
	t.Run("Trash is permanent", func(t *testing.T) {
		t.Parallel()
		s, ca := startWrites(t)
		c := dial(t, s, ca)
		selectRW(t, c, "[Gmail]/Trash")
		if _, err := c.Store(imap.UIDSetNum(301), deleted, nil).Collect(); err != nil {
			t.Fatal(err)
		}
		seqs, err := c.Expunge().Collect()
		if err != nil || !reflect.DeepEqual(seqs, []uint32{1}) {
			t.Errorf("EXPUNGE = %v, %v; want sequence 1", seqs, err)
		}
		if got := where(t, s, ca, s.MsgID(2)); len(got) != 0 {
			t.Errorf("message expunged from Trash is still in %v", got)
		}
	})
	t.Run("UID EXPUNGE keeps deleted messages outside its set", func(t *testing.T) {
		t.Parallel()
		s, ca := startWrites(t)
		c := dial(t, s, ca)
		selectRW(t, c, "[Gmail]/All Mail")
		if _, err := c.Store(imap.UIDSetNum(1, 2), deleted, nil).Collect(); err != nil {
			t.Fatal(err)
		}
		if seqs, err := c.UIDExpunge(imap.UIDSetNum(2)).Collect(); err != nil || !reflect.DeepEqual(seqs, []uint32{2}) {
			t.Errorf("UID EXPUNGE 2 = %v, %v", seqs, err)
		}
		if got := where(t, s, ca, s.MsgID(0)); got["[Gmail]/All Mail"] != 1 {
			t.Errorf("message outside the UID set is in %v; it must stay", got)
		}
	})
	t.Run("a label folder drops the label", func(t *testing.T) {
		t.Parallel()
		s, ca := startWrites(t)
		c := dial(t, s, ca)
		selectRW(t, c, "INBOX")
		if _, err := c.Store(imap.UIDSetNum(101), deleted, nil).Collect(); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Expunge().Collect(); err != nil {
			t.Fatal(err)
		}
		if got := where(t, s, ca, s.MsgID(0)); !reflect.DeepEqual(got, map[string]imap.UID{"[Gmail]/All Mail": 1, "Receipts": 1001}) {
			t.Errorf("message expunged from INBOX is in %v, want All Mail and Receipts", got)
		}
	})
}

func TestFakeGmailFolders(t *testing.T) {
	t.Parallel()
	s, ca := startWrites(t)
	c := dial(t, s, ca)
	for _, name := range []string{"Projects", "Projects/2026", "Work Stuff"} {
		if err := c.Create(name, nil).Wait(); err != nil {
			t.Errorf("CREATE %q: %v", name, err)
		}
	}
	names := listNames(t, c)
	for _, want := range []string{"Projects", "Projects/2026", "Work Stuff"} {
		if !slices.Contains(names, want) {
			t.Errorf("LIST lacks %q: %v", want, names)
		}
	}
	for _, name := range []string{"Receipts", "receipts", "INBOX", "[Gmail]", "[Gmail]/Mine", "[Gmail]/Trash"} {
		if err := c.Create(name, nil).Wait(); err == nil {
			t.Errorf("CREATE %q succeeded", name)
		}
	}

	if err := c.Rename("Receipts", "Paid", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	names = listNames(t, c)
	if slices.Contains(names, "Receipts") || !slices.Contains(names, "Paid") {
		t.Errorf("LIST after RENAME = %v", names)
	}
	if m := fetchIn(t, s, ca, "Paid", 1001); m.GmailMsgID != s.MsgID(0) || !slices.Contains(m.GmailLabels, "Paid") {
		t.Errorf("Paid UID 1001 = %d %q; RENAME keeps UIDs and relabels messages", m.GmailMsgID, m.GmailLabels)
	}
	if err := c.Rename("Projects", "Plans", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if names := listNames(t, c); !slices.Contains(names, "Plans/2026") || slices.Contains(names, "Projects/2026") {
		t.Errorf("LIST after renaming a parent = %v; children follow", names)
	}
	refusedRenames := [][2]string{
		{"[Gmail]/Trash", "Bin"}, {"Paid", "[Gmail]/Paid"}, {"INBOX", "Old"}, {"Nope", "Other"}, {"Paid", "Work Stuff"},
	}
	for _, r := range refusedRenames {
		if err := c.Rename(r[0], r[1], nil).Wait(); err == nil {
			t.Errorf("RENAME %q %q succeeded", r[0], r[1])
		}
	}

	if err := c.Delete("Paid").Wait(); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(listNames(t, c), "Paid") {
		t.Error("LIST still has Paid after DELETE")
	}
	if got := fetchIn(t, s, ca, "[Gmail]/All Mail", 1).GmailLabels; slices.Contains(got, "Paid") {
		t.Errorf("labels after DELETE = %q; the label goes with its folder", got)
	}
	for _, name := range []string{"[Gmail]/Spam", "[Gmail]", "INBOX", "Nope"} {
		if err := c.Delete(name).Wait(); err == nil {
			t.Errorf("DELETE %q succeeded", name)
		}
	}
}

func TestFakeGmailFileSent(t *testing.T) {
	t.Parallel()
	s, ca := startWrites(t)
	s.FileSent([]byte("From: Me <me@gmail.example>\r\nSubject: Re: Invoice\r\nMessage-ID: <r@gmail.example>\r\nIn-Reply-To: <a@example.com>\r\nReferences: <a@example.com>\r\n\r\nPaid.\r\n"))
	got := where(t, s, ca, s.MsgID(3))
	if got["[Gmail]/Sent Mail"] == 0 || got["[Gmail]/All Mail"] == 0 {
		t.Fatalf("filed message is in %v, want Sent Mail and All Mail", got)
	}
	m := fetchIn(t, s, ca, "[Gmail]/Sent Mail", got["[Gmail]/Sent Mail"])
	if !reflect.DeepEqual(m.GmailLabels, []string{`\Sent`}) || !slices.Contains(m.Flags, imap.FlagSeen) {
		t.Errorf("filed message labels %q flags %v, want [\\Sent] and \\Seen", m.GmailLabels, m.Flags)
	}
	if m.GmailThreadID != s.ThreadID(0) || s.ThreadID(3) != s.ThreadID(0) {
		t.Errorf("reply thread %d, want the original's %d", m.GmailThreadID, s.ThreadID(0))
	}
}

func TestFakeGmailOtherSessionsSeeChanges(t *testing.T) {
	t.Parallel()
	s, ca := startWrites(t)
	watcher := dial(t, s, ca)
	selectRW(t, watcher, "INBOX")
	writer := dial(t, s, ca)

	data := appendRaw(t, writer, "INBOX", "Subject: Late\r\n\r\nx\r\n")
	found, err := watcher.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil || !slices.Contains(found.AllUIDs(), data.UID) {
		t.Errorf("UID SEARCH on a session already in INBOX = %v, %v; want the appended UID %d", found, err, data.UID)
	}
	if err := watcher.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if n := watcher.Mailbox().NumMessages; n != 3 {
		t.Errorf("INBOX count after another session's APPEND = %d, want 3", n)
	}

	selectRW(t, writer, "INBOX")
	if _, err := writer.Move(imap.UIDSetNum(101), "[Gmail]/Trash").Wait(); err != nil {
		t.Fatal(err)
	}
	if err := watcher.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if n := watcher.Mailbox().NumMessages; n != 2 {
		t.Errorf("INBOX count after another session's MOVE = %d, want 2", n)
	}
	msgs, err := watcher.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{UID: true}).Collect()
	if err != nil || len(msgs) != 1 || msgs[0].UID != 102 {
		t.Errorf("FETCH 1 after the EXPUNGE = %v, %v; want UID 102", msgs, err)
	}
}
