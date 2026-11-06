package imapstore

import (
	"testing"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/testutil/imaptest"
	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
)

// summaryIDMail is INBOX for TestSummaryMessageID, appended as UIDs 1 to
// 4: a Message-ID in angle brackets, one with spaces around it, one
// without brackets, which is no msg-id at all, and none.
var summaryIDMail = []string{
	"From: a@example.net\r\nSubject: Bracketed\r\nDate: Mon, 26 Oct 2026 09:00:00 +0000\r\n" +
		"Message-ID: <one@example.net>\r\n\r\nhi\r\n",
	"From: a@example.net\r\nSubject: Spaced\r\nDate: Mon, 26 Oct 2026 09:01:00 +0000\r\n" +
		"Message-ID:   <two@example.net>  \r\n\r\nhi\r\n",
	"From: a@example.net\r\nSubject: Bare\r\nDate: Mon, 26 Oct 2026 09:02:00 +0000\r\n" +
		"Message-ID: three@example.net\r\n\r\nhi\r\n",
	"From: a@example.net\r\nSubject: None\r\nDate: Mon, 26 Oct 2026 09:03:00 +0000\r\n\r\nhi\r\n",
}

// A summary carries the envelope's Message-ID without angle brackets, the
// same value the header gives, and "" when the message has none the client
// can read.
func TestSummaryMessageID(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{
		Username: "host@example.test", Password: "pw", Mail: map[string][]string{"INBOX": summaryIDMail},
	})
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	res, err := s.Search(t.Context(), mailstore.SearchRequest{})
	if err != nil || len(res) != 1 || len(res[0].UIDs) != len(summaryIDMail) {
		t.Fatalf("Search = %+v, %v; want four messages in INBOX", res, err)
	}
	src := res[0].Source
	uids := []uint32{1, 2, 3, 4}
	sums, err := s.FetchSummaries(t.Context(), src, uids, mailstore.SummaryOptions{})
	if err != nil || len(sums) != len(uids) {
		t.Fatalf("FetchSummaries = %d summaries, %v; want %d", len(sums), err, len(uids))
	}
	hs, err := s.FetchHeaders(t.Context(), src, uids)
	if err != nil || len(hs) != len(uids) {
		t.Fatalf("FetchHeaders = %d headers, %v; want %d", len(hs), err, len(uids))
	}
	want := []string{"one@example.net", "two@example.net", "", ""}
	for i, w := range want {
		if sums[i].MessageID != w {
			t.Errorf("summary of UID %d: MessageID = %q, want %q", uids[i], sums[i].MessageID, w)
		}
		if hs[i].Summary.MessageID != w {
			t.Errorf("header of UID %d: Summary.MessageID = %q, want %q", uids[i], hs[i].Summary.MessageID, w)
		}
		if hs[i].Summary.MessageID != hs[i].MessageID {
			t.Errorf("header of UID %d: Summary.MessageID = %q, MessageID = %q; want them equal",
				uids[i], hs[i].Summary.MessageID, hs[i].MessageID)
		}
	}
}

// A Gmail summary carries the Message-ID too, though it fetches no
// References.
func TestSummaryMessageIDGmail(t *testing.T) {
	t.Parallel()
	f := newGmailFixture(t)
	res := f.search(t, "")
	sums, err := f.store.FetchSummaries(t.Context(), res[0].Source, []uint32{1, 2}, mailstore.SummaryOptions{})
	if err != nil || len(sums) != 2 {
		t.Fatalf("FetchSummaries = %d summaries, %v; want 2", len(sums), err)
	}
	if sums[0].MessageID != "inv@example.com" || sums[1].MessageID != "reply@gmail.example" {
		t.Errorf("MessageIDs = %q, %q; want inv@example.com and reply@gmail.example", sums[0].MessageID, sums[1].MessageID)
	}
}
