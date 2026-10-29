package fakegmail

import (
	"crypto/tls"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
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
	if !c.Caps().Has(imap.CapGmailExt1) || !c.Caps().Has(imap.CapPreview) {
		t.Errorf("caps = %v", c.Caps())
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
