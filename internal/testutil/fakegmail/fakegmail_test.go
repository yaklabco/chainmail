package fakegmail

import (
	"crypto/tls"
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
)

const raw1 = "From: Alex <alex@example.com>\r\nSubject: Invoice\r\nMessage-ID: <a@example.com>\r\n\r\nPlease pay.\r\n"

func dial(t *testing.T, s *Server, ca *tlstest.CA) *imapclient.Client {
	t.Helper()
	c, err := imapclient.DialTLS(s.Endpoint.Addr(), &imapclient.Options{TLSConfig: &tls.Config{RootCAs: ca.Pool, MinVersion: tls.VersionTLS12}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Login("me@gmail.example", "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFakeGmail(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := Start(t, ca, "me@gmail.example", "pw",
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`, "Receipts"}},
		Message{Raw: "Subject: Re: Invoice\r\nFrom: Me <me@gmail.example>\r\n\r\nPaid.\r\n", Date: time.Now(), Labels: []string{`\Sent`}, ThreadOf: 1},
		Message{Raw: "Subject: Old\r\n\r\nx\r\n", Date: time.Now(), Trash: true},
	)
	c := dial(t, s, ca)
	if !c.Caps().Has(imap.CapGmailExt1) || !c.Caps().Has(imap.CapESearch) || c.Caps().Has(imap.CapPreview) {
		t.Errorf("caps = %v, want X-GM-EXT-1 and ESEARCH but no PREVIEW, as on Gmail", c.Caps())
	}
	list, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, l := range list {
		names = append(names, l.Mailbox)
	}
	for _, want := range []string{"INBOX", "[Gmail]/All Mail", "[Gmail]/Sent Mail", "[Gmail]/Trash", "[Gmail]/Spam", "Receipts"} {
		if !slices.Contains(names, want) {
			t.Errorf("LIST lacks %s: %v", want, names)
		}
	}
	if _, err := c.Select("[Gmail]/All Mail", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := c.UIDSearch(&imap.SearchCriteria{GmailRaw: []string{"from:alex label:receipts"}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got := data.AllUIDs(); !reflect.DeepEqual(got, []imap.UID{1}) {
		t.Errorf("X-GM-RAW search = %v, want [1]", got)
	}
	if got := s.LastRaw(); !reflect.DeepEqual(got, []string{"from:alex label:receipts"}) {
		t.Errorf("LastRaw = %q", got)
	}
	data, err = c.UIDSearch(&imap.SearchCriteria{GmailThreadID: []uint64{s.ThreadID(0)}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got := data.AllUIDs(); !reflect.DeepEqual(got, []imap.UID{1, 2}) {
		t.Errorf("X-GM-THRID search = %v, want [1 2]", got)
	}
	msgs, err := c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{GmailMsgID: true, GmailThreadID: true, GmailLabels: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if m := msgs[0]; m.GmailMsgID != s.MsgID(0) || m.GmailThreadID != s.ThreadID(0) || !slices.Contains(m.GmailLabels, "Receipts") {
		t.Errorf("fetch = %d %d %v", m.GmailMsgID, m.GmailThreadID, m.GmailLabels)
	}

	if _, err := c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{Peek: true}}}).Collect(); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(s.Flags(0), imap.FlagSeen) {
		t.Error("BODY.PEEK set \\Seen")
	}
	if _, err := c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}}).Collect(); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(s.Flags(0), imap.FlagSeen) {
		t.Error("BODY[] did not set \\Seen; the fake must behave like Gmail so tests catch it")
	}
	if s.WritableSelects() != 0 {
		t.Errorf("writable selects = %d", s.WritableSelects())
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if s.WritableSelects() != 1 {
		t.Errorf("SELECT not counted: %d", s.WritableSelects())
	}
}

func TestFakeGmailResetAllMail(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := Start(t, ca, "me@gmail.example", "pw", Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}})
	c := dial(t, s, ca)
	before, err := c.Select("[Gmail]/All Mail", &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	s.ResetAllMail()
	if err := c.Noop().Wait(); err == nil {
		t.Error("ResetAllMail should drop open connections")
	}
	c2 := dial(t, s, ca)
	after, err := c2.Select("[Gmail]/All Mail", &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if after.UIDValidity == before.UIDValidity {
		t.Error("UIDVALIDITY did not change")
	}
	data, err := c2.UIDSearch(&imap.SearchCriteria{GmailMsgID: []uint64{s.MsgID(0)}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if uids := data.AllUIDs(); len(uids) != 1 || uids[0] == 1 {
		t.Errorf("after reset the message has UIDs %v, want one new UID", uids)
	}
	if s.Logins() != 2 {
		t.Errorf("logins = %d, want 2", s.Logins())
	}
}

func TestFakeGmailEmptyRaw(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := Start(t, ca, "me@gmail.example", "pw", Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}})
	c := dial(t, s, ca)
	if _, err := c.Select("[Gmail]/All Mail", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := c.UIDSearch(&imap.SearchCriteria{GmailRaw: []string{""}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got := data.AllUIDs(); !reflect.DeepEqual(got, []imap.UID{1}) {
		t.Errorf("empty X-GM-RAW search = %v, want [1]", got)
	}
}

// gmailCaps is what imap.gmail.com advertised after LOGIN, measured live on 2026-10-29.
var gmailCaps = []imap.Cap{
	"APPENDLIMIT", "CHILDREN", "COMPRESS=DEFLATE", "CONDSTORE", "ENABLE", "ESEARCH", "ID", "IDLE", "IMAP4rev1",
	"LIST-EXTENDED", "LIST-STATUS", "LITERAL-", "MOVE", "NAMESPACE", "QUOTA", "SPECIAL-USE", "UIDPLUS",
	"UNSELECT", "UTF8=ACCEPT", "X-GM-EXT-1", "XLIST",
}

// notEmulated are Gmail capabilities imapserver cannot advertise. The fake
// leaves them out rather than claim what it does not do.
var notEmulated = []imap.Cap{"COMPRESS=DEFLATE", "CONDSTORE", "ID", "QUOTA", "XLIST"}

func TestFakeGmailCapabilities(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	tests := []struct {
		name    string
		opts    Options
		preview bool
	}{
		{"default is Gmail", Options{}, false},
		{"preview opt-in", Options{Preview: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			preview := "Please pay."
			s := StartWith(t, ca, tt.opts, "me@gmail.example", "pw", Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}, Preview: &preview})
			c := dial(t, s, ca)
			caps := c.Caps()
			for _, want := range gmailCaps {
				if !slices.Contains(notEmulated, want) && !caps.Has(want) {
					t.Errorf("caps lack Gmail's %s: %v", want, caps)
				}
			}
			for got := range caps {
				extra := got != imap.CapSASLIR && !strings.HasPrefix(string(got), "APPENDLIMIT=") && !slices.Contains(gmailCaps, got)
				if extra && (got != imap.CapPreview || !tt.preview) {
					t.Errorf("caps have %s, which Gmail does not advertise: %v", got, caps)
				}
			}
			if caps.Has(imap.CapPreview) != tt.preview {
				t.Errorf("PREVIEW advertised = %v, want %v", caps.Has(imap.CapPreview), tt.preview)
			}
			if _, err := c.Select("[Gmail]/All Mail", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
				t.Fatal(err)
			}
			msgs, err := c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{Preview: true}).Collect()
			switch {
			case tt.preview && (err != nil || len(msgs) != 1 || msgs[0].Preview == nil || *msgs[0].Preview != preview):
				t.Errorf("FETCH PREVIEW = %v, %v; want %q", msgs, err, preview)
			case !tt.preview && err == nil:
				t.Error("FETCH PREVIEW succeeded on a server that does not advertise PREVIEW")
			}
		})
	}
}

func TestFakeGmailRejectsUnsupportedCriteria(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := Start(t, ca, "me@gmail.example", "pw", Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}})
	c := dial(t, s, ca)
	if _, err := c.Select("[Gmail]/All Mail", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	unsupported := map[string]imap.SearchCriteria{
		"SeqNum":     {SeqNum: []imap.SeqSet{imap.SeqSetNum(1)}},
		"Since":      {Since: day},
		"Before":     {Before: day},
		"SentSince":  {SentSince: day},
		"SentBefore": {SentBefore: day},
		"Body":       {Body: []string{"pay"}},
		"Larger":     {Larger: 1},
		"Smaller":    {Smaller: 1 << 20},
		"in Not":     {Not: []imap.SearchCriteria{{Since: day}}},
		"in Or":      {Or: [][2]imap.SearchCriteria{{{Text: []string{"pay"}}, {Body: []string{"pay"}}}}},
		"raw in Not": {Not: []imap.SearchCriteria{{GmailRaw: []string{"from:alex"}}}},
	}
	for name, criteria := range unsupported {
		_, err := c.UIDSearch(&criteria, nil).Wait()
		var ierr *imap.Error
		if !errors.As(err, &ierr) || ierr.Type != imap.StatusResponseTypeBad || !strings.Contains(ierr.Text, "fakegmail: unsupported criterion") {
			t.Errorf("%s: err = %v, want BAD fakegmail: unsupported criterion", name, err)
		}
	}
	// imapserver cannot parse MODSEQ and imapclient never sends RETURN (SAVE),
	// so ask the session directly.
	sess := &session{s: s, selected: s.folderLocked("[Gmail]/All Mail")}
	if _, err := sess.Search(imapserver.NumKindUID, &imap.SearchCriteria{ModSeq: &imap.SearchCriteriaModSeq{ModSeq: 1}}, nil); err == nil {
		t.Error("MODSEQ matched instead of failing")
	}
	if _, err := sess.Search(imapserver.NumKindUID, &imap.SearchCriteria{}, &imap.SearchOptions{ReturnSave: true}); err == nil {
		t.Error("SEARCH RETURN (SAVE) succeeded; the fake does not advertise SEARCHRES")
	}
	supported := imap.SearchCriteria{
		UID:           []imap.UIDSet{imap.UIDSetNum(1)},
		GmailRaw:      []string{"from:alex"},
		GmailMsgID:    []uint64{s.MsgID(0)},
		GmailThreadID: []uint64{s.ThreadID(0)},
	}
	data, err := c.UIDSearch(&supported, nil).Wait()
	if err != nil || !reflect.DeepEqual(data.AllUIDs(), []imap.UID{1}) {
		t.Errorf("supported criteria = %v, %v; want [1]", data, err)
	}
}

func TestFakeGmailESearch(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := Start(t, ca, "me@gmail.example", "pw",
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}},
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}},
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}},
	)
	c := dial(t, s, ca)
	if _, err := c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := c.UIDSearch(&imap.SearchCriteria{}, &imap.SearchOptions{ReturnMin: true, ReturnMax: true, ReturnCount: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if data.Min != 101 || data.Max != 103 || data.Count != 3 {
		t.Errorf("ESEARCH MIN MAX COUNT = %d %d %d, want 101 103 3", data.Min, data.Max, data.Count)
	}
}

func TestFakeGmailFolderUIDs(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := Start(t, ca, "me@gmail.example", "pw",
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`, "Receipts"}},
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Sent`, "Taxes"}},
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Draft`}},
		Message{Raw: raw1, Date: time.Now(), Trash: true},
		Message{Raw: raw1, Date: time.Now(), Spam: true},
	)
	c := dial(t, s, ca)
	want := map[string]imap.UID{
		"[Gmail]/All Mail": 3, "INBOX": 101, "[Gmail]/Sent Mail": 201, "[Gmail]/Trash": 301,
		"[Gmail]/Spam": 401, "[Gmail]/Drafts": 501, "Receipts": 1001, "Taxes": 1101,
	}
	for folder, uid := range want {
		wantUIDs := []imap.UID{uid}
		if folder == "[Gmail]/All Mail" {
			wantUIDs = []imap.UID{1, 2, 3} // the inbox, sent and draft messages
		}
		sel, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			t.Fatal(err)
		}
		if sel.UIDNext != uid+1 {
			t.Errorf("%s: UIDNEXT = %d, want %d", folder, sel.UIDNext, uid+1)
		}
		data, err := c.UIDSearch(&imap.SearchCriteria{GmailRaw: []string{"alex"}}, nil).Wait()
		if err != nil {
			t.Fatal(err)
		}
		if got := data.AllUIDs(); !reflect.DeepEqual(got, wantUIDs) {
			t.Errorf("%s: UIDs = %v, want %v", folder, got, wantUIDs)
		}
	}
	if _, err := c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	if msgs, err := c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{UID: true}).Collect(); err != nil || len(msgs) != 0 {
		t.Errorf("UID FETCH 1 in INBOX = %v, %v; an All Mail UID must not resolve in INBOX", msgs, err)
	}
	got := s.RawSearches()
	if len(got) != len(want) {
		t.Fatalf("RawSearches = %v, want %d", got, len(want))
	}
	for _, r := range got {
		if _, ok := want[r.Folder]; !ok || r.Raw != "alex" {
			t.Errorf("RawSearch = %+v", r)
		}
	}
	if raws := s.LastRaw(); len(raws) != len(want) || raws[0] != "alex" {
		t.Errorf("LastRaw = %q", raws)
	}
	s.ResetAllMail()
	c2 := dial(t, s, ca)
	if _, err := c2.Select("[Gmail]/All Mail", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := c2.UIDSearch(&imap.SearchCriteria{GmailMsgID: []uint64{s.MsgID(0)}}, nil).Wait()
	if err != nil || len(data.AllUIDs()) != 1 || data.AllUIDs()[0] <= 1101 {
		t.Errorf("All Mail UID after reset = %v, %v; want one above every other folder's", data, err)
	}
}

func TestFakeGmailStar(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := Start(t, ca, "me@gmail.example", "pw",
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}},
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}},
	)
	star := func(start uint32) imap.UIDSet { return imap.UIDSet{{Start: imap.UID(start), Stop: 0}} }
	// imapclient matches FETCH responses against the unresolved set, so it
	// drops replies to a set with *. Check the fake's resolution directly.
	allMail := s.folderLocked("[Gmail]/All Mail")
	inbox := s.folderLocked("INBOX")
	tests := []struct {
		name   string
		f      *folder
		numSet imap.NumSet
		want   []int
	}{
		{"UID *", allMail, star(0), []int{1}},
		{"UID 3:* past the end", allMail, star(3), []int{1}},
		{"UID 1:*", allMail, star(1), []int{0, 1}},
		{"UID 2:* in INBOX", inbox, star(2), []int{0, 1}},
		{"UID 102:* in INBOX", inbox, star(102), []int{1}},
		{"seq *", allMail, imap.SeqSet{{Start: 0, Stop: 0}}, []int{1}},
		{"seq 1:*", inbox, imap.SeqSet{{Start: 1, Stop: 0}}, []int{0, 1}},
	}
	// messagesIn resolves against a session's view of the folder.
	messagesIn := func(f *folder, numSet imap.NumSet) ([]int, error) {
		sess := &session{s: s}
		if _, err := sess.Select(f.name, &imap.SelectOptions{ReadOnly: true}); err != nil {
			return nil, err
		}
		return sess.messagesIn(numSet)
	}
	for _, tt := range tests {
		got, err := messagesIn(tt.f, tt.numSet)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: messages %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}
	if _, err := messagesIn(allMail, nil); err == nil {
		t.Error("an unexpected message set type matched instead of failing")
	}
	c := dial(t, s, ca)
	if _, err := c.Select("[Gmail]/All Mail", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{star(0)}}, nil).Wait()
	if err != nil || !reflect.DeepEqual(data.AllUIDs(), []imap.UID{2}) {
		t.Errorf("UID SEARCH UID * = %v, %v; want [2]", data, err)
	}
	data, err = c.UIDSearch(&imap.SearchCriteria{Not: []imap.SearchCriteria{{UID: []imap.UIDSet{star(0)}}}}, nil).Wait()
	if err != nil || !reflect.DeepEqual(data.AllUIDs(), []imap.UID{1}) {
		t.Errorf("UID SEARCH NOT UID * = %v, %v; want [1]", data, err)
	}
}

func TestFakeGmailIDsAboveInt64(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := Start(t, ca, "me@gmail.example", "pw",
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}},
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}, ThreadOf: 1},
	)
	for i := range 2 {
		if s.MsgID(i) <= math.MaxInt64 || s.ThreadID(i) <= math.MaxInt64 {
			t.Errorf("message %d: X-GM-MSGID %d, X-GM-THRID %d; want both above MaxInt64", i, s.MsgID(i), s.ThreadID(i))
		}
	}
	c := dial(t, s, ca)
	if _, err := c.Select("[Gmail]/All Mail", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := c.UIDSearch(&imap.SearchCriteria{GmailMsgID: []uint64{s.MsgID(1)}}, nil).Wait()
	if err != nil || !reflect.DeepEqual(data.AllUIDs(), []imap.UID{2}) {
		t.Errorf("X-GM-MSGID search = %v, %v; want [2]", data, err)
	}
	msgs, err := c.Fetch(imap.UIDSetNum(2), &imap.FetchOptions{GmailMsgID: true, GmailThreadID: true}).Collect()
	if err != nil || len(msgs) != 1 || msgs[0].GmailMsgID != s.MsgID(1) || msgs[0].GmailThreadID != s.ThreadID(0) {
		t.Errorf("FETCH X-GM-MSGID X-GM-THRID = %v, %v", msgs, err)
	}
}

func TestFakeGmailList(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := Start(t, ca, "me@gmail.example", "pw",
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`, "Receipts"}},
		Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}, Flags: []imap.Flag{imap.FlagSeen}},
	)
	c := dial(t, s, ca)
	list, err := c.List("", "*", &imap.ListOptions{ReturnStatus: &imap.StatusOptions{NumMessages: true, NumUnseen: true}}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	var sawAllMail bool
	for _, l := range list {
		if l.Mailbox != "[Gmail]/All Mail" {
			continue
		}
		sawAllMail = true
		if l.Status == nil || l.Status.NumMessages == nil || *l.Status.NumMessages != 2 || l.Status.NumUnseen == nil || *l.Status.NumUnseen != 1 {
			t.Errorf("LIST-STATUS for All Mail = %+v, want 2 messages, 1 unseen", l.Status)
		}
	}
	if !sawAllMail {
		t.Errorf("LIST lacks All Mail: %v", list)
	}
	list, err = c.List("", "[Gmail]/%", nil).Collect()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range list {
		if !strings.HasPrefix(l.Mailbox, "[Gmail]/") {
			t.Errorf(`LIST "" "[Gmail]/%%" returned %s`, l.Mailbox)
		}
	}
	if len(list) != 5 {
		t.Errorf(`LIST "" "[Gmail]/%%" = %d folders, want 5`, len(list))
	}
	list, err = c.List("", "", nil).Collect()
	if err != nil || len(list) != 1 || list[0].Delim != '/' || list[0].Mailbox != "" {
		t.Errorf(`LIST "" "" = %v, %v; want the hierarchy delimiter`, list, err)
	}
	list, err = c.List("", "*", &imap.ListOptions{SelectSpecialUse: true}).Collect()
	if err != nil || len(list) != 5 {
		t.Errorf("LIST (SPECIAL-USE) = %d folders, %v; want the 5 special-use ones", len(list), err)
	}
	for _, l := range list {
		if l.Mailbox == "INBOX" || l.Mailbox == "Receipts" || l.Mailbox == "[Gmail]" {
			t.Errorf("LIST (SPECIAL-USE) returned %s", l.Mailbox)
		}
	}
	list, err = c.List("", "*", &imap.ListOptions{SelectSubscribed: true, SelectRemote: true, SelectRecursiveMatch: true}).Collect()
	if err != nil || len(list) != 8 {
		t.Errorf("LIST (SUBSCRIBED REMOTE RECURSIVEMATCH) = %d folders, %v; want all 8, as every Gmail folder is subscribed", len(list), err)
	}
	for _, l := range list {
		if !slices.Contains(l.Attrs, imap.MailboxAttrSubscribed) {
			t.Errorf("LIST (SUBSCRIBED): %s lacks \\Subscribed", l.Mailbox)
		}
	}
	if _, err := c.List("", "*", &imap.ListOptions{ReturnStatus: &imap.StatusOptions{Size: true}}).Collect(); err == nil {
		t.Error("LIST RETURN (STATUS (SIZE)) succeeded; the fake does not advertise STATUS=SIZE")
	}
	if err := c.Noop().Wait(); err != nil {
		t.Errorf("connection unusable after a refused LIST-STATUS: %v", err)
	}
}

func TestFakeGmailStatus(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := Start(t, ca, "me@gmail.example", "pw", Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}})
	c := dial(t, s, ca)
	data, err := c.Status("INBOX", &imap.StatusOptions{NumMessages: true, AppendLimit: true}).Wait()
	if err != nil || data.NumMessages == nil || *data.NumMessages != 1 || data.AppendLimit == nil || *data.AppendLimit != appendLimit {
		t.Errorf("STATUS (MESSAGES APPENDLIMIT) = %+v, %v", data, err)
	}
	for name, opts := range map[string]*imap.StatusOptions{
		"SIZE":            {Size: true},
		"DELETED":         {NumDeleted: true},
		"DELETED-STORAGE": {DeletedStorage: true},
		"HIGHESTMODSEQ":   {HighestModSeq: true},
	} {
		if _, err := c.Status("INBOX", opts).Wait(); err == nil {
			t.Errorf("STATUS (%s) succeeded; Gmail does not advertise it", name)
		}
	}
	if err := c.Noop().Wait(); err != nil {
		t.Errorf("connection unusable after refused STATUS items: %v", err)
	}
	// imapclient cannot ask for RECENT, which Gmail answers.
	sess := &session{s: s}
	if data, err := sess.Status("INBOX", &imap.StatusOptions{NumRecent: true}); err != nil || data.NumRecent == nil || *data.NumRecent != 0 {
		t.Errorf("STATUS (RECENT) = %+v, %v; want 0", data, err)
	}
}

func TestFakeGmailRejectsUnadvertisedFetchItems(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := Start(t, ca, "me@gmail.example", "pw", Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}})
	c := dial(t, s, ca)
	if _, err := c.Select("[Gmail]/All Mail", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	for name, opts := range map[string]*imap.FetchOptions{
		"BINARY.PEEK[1]": {BinarySection: []*imap.FetchItemBinarySection{{Part: []int{1}, Peek: true}}},
		"BINARY.SIZE[1]": {BinarySectionSize: []*imap.FetchItemBinarySectionSize{{Part: []int{1}}}},
		"MODSEQ":         {ModSeq: true},
		"CHANGEDSINCE 1": {UID: true, ChangedSince: 1},
	} {
		if _, err := c.Fetch(imap.UIDSetNum(1), opts).Collect(); err == nil {
			t.Errorf("FETCH %s succeeded; Gmail does not advertise it", name)
		}
	}
	if slices.Contains(s.Flags(0), imap.FlagSeen) {
		t.Error("a refused FETCH set \\Seen")
	}
}
