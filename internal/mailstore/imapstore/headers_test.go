package imapstore

import (
	"errors"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/storetest"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/fakegmail"
	"github.com/jamesainslie/chainmail/internal/testutil/imaptest"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

var (
	_ mailstore.HeaderFetcher = (*Store)(nil)
	_ mailstore.Scoper        = (*Store)(nil)
)

// headersRich is a reply with Cc, Bcc and a PDF attachment; headersPlain
// has none of those. A test server appends them in this order, so they
// get UIDs 1 and 2.
const (
	headersRich = "From: Alice <alice@example.net>\r\nTo: Host <host@example.test>\r\n" +
		"Cc: Carol <carol@example.net>, erin@example.net\r\nBcc: dave@example.net\r\nSubject: Report\r\n" +
		"Date: Mon, 26 Oct 2026 09:00:00 +0000\r\nMessage-ID: <rich@example.net>\r\nIn-Reply-To: <root@example.net>\r\n" +
		"References: <older@example.net> <root@example.net>\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"b\"\r\n\r\n" +
		"--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nSee attached.\r\n" +
		"--b\r\nContent-Type: application/pdf; name=\"report.pdf\"\r\nContent-Disposition: attachment; filename=\"report.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQ=\r\n--b--\r\n"
	headersPlain = "From: bob@example.net\r\nSubject: Plain\r\nDate: Tue, 27 Oct 2026 09:00:00 +0000\r\n\r\nhello\r\n"
)

// headersStore returns a store on a test server holding headersRich and
// headersPlain in INBOX, and INBOX's source.
func headersStore(t *testing.T) (*Store, mailstore.Source) {
	t.Helper()
	ca := tlstest.New(t)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, imaptest.User{
		Username: "host@example.test", Password: "pw", Mail: map[string][]string{"INBOX": {headersRich, headersPlain}},
	})
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	res, err := s.Search(t.Context(), mailstore.SearchRequest{})
	if err != nil || len(res) != 1 || !slices.Equal(res[0].UIDs, []uint32{2, 1}) {
		t.Fatalf("Search = %+v, %v; want UIDs 2 and 1 in INBOX", res, err)
	}
	return s, res[0].Source
}

func TestFetchHeadersStandard(t *testing.T) {
	t.Parallel()
	s, src := headersStore(t)
	hs, err := s.FetchHeaders(t.Context(), src, []uint32{2, 0, 99, 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 2 || hs[0].Summary.Ref.UID != 2 || hs[1].Summary.Ref.UID != 1 {
		t.Fatalf("FetchHeaders gave %+v, want UIDs 2 then 1 with 0 and 99 left out", hs)
	}
	plain, rich := hs[0], hs[1]
	sums, err := s.FetchSummaries(t.Context(), src, []uint32{2, 1}, mailstore.SummaryOptions{})
	if err != nil || len(sums) != 2 {
		t.Fatalf("FetchSummaries = %v, %v", sums, err)
	}
	for i, h := range hs {
		if !storetest.SameSummary(h.Summary, sums[i]) {
			t.Errorf("header %d summary = %+v, want FetchSummaries' %+v", i, h.Summary, sums[i])
		}
		if h.Summary.Preview != nil || h.Summary.Sample != nil {
			t.Errorf("header %d has a snippet", i)
		}
		if slices.Contains(sums[i].Flags, `\Seen`) {
			t.Errorf("flags of UID %d after FetchHeaders = %q, want no \\Seen", sums[i].Ref.UID, sums[i].Flags)
		}
	}
	if want := []mailstore.Address{{Name: "Carol", Email: "carol@example.net"}, {Email: "erin@example.net"}}; !slices.Equal(rich.Cc, want) {
		t.Errorf("Cc = %v, want %v", rich.Cc, want)
	}
	if want := []mailstore.Address{{Email: "dave@example.net"}}; !slices.Equal(rich.Bcc, want) {
		t.Errorf("Bcc = %v, want %v", rich.Bcc, want)
	}
	if rich.MessageID != "rich@example.net" {
		t.Errorf("MessageID = %q, want it without angle brackets", rich.MessageID)
	}
	if got := mailstore.MessageIDs(rich.InReplyTo); !slices.Equal(got, []string{"root@example.net"}) {
		t.Errorf("In-Reply-To = %q", rich.InReplyTo)
	}
	if got := mailstore.MessageIDs(rich.References); !slices.Equal(got, []string{"older@example.net", "root@example.net"}) {
		t.Errorf("References = %q", rich.References)
	}
	if want := mailstore.ThreadKey("older@example.net"); rich.Summary.ThreadID != want || !rich.Summary.HasAttachment {
		t.Errorf("thread %q, attachment %v; want %q and true", rich.Summary.ThreadID, rich.Summary.HasAttachment, want)
	}
	if len(plain.Cc) != 0 || len(plain.Bcc) != 0 || plain.MessageID != "" || len(plain.References) != 0 || len(plain.Attachments) != 0 {
		t.Errorf("plain header = %+v, want only its summary", plain)
	}
	// What FetchMessages reads of the same message agrees.
	res := s.FetchMessages(t.Context(), []ref.Ref{rich.Summary.Ref})[0]
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	m := res.Message
	if !slices.Equal(rich.Cc, m.Cc) || rich.MessageID != m.MessageID || !slices.Equal(rich.InReplyTo, m.InReplyTo) ||
		!slices.Equal(rich.References, m.References) || !slices.Equal(rich.Attachments, m.Attachments) || len(rich.Attachments) != 1 {
		t.Errorf("header = %+v, want what FetchMessages read: Cc %v, ID %q, In-Reply-To %q, References %q, attachments %+v",
			rich, m.Cc, m.MessageID, m.InReplyTo, m.References, m.Attachments)
	}
}

func TestFetchHeadersErrors(t *testing.T) {
	t.Parallel()
	s, src := headersStore(t)
	stale := src
	stale.UIDValidity++
	if _, err := s.FetchHeaders(t.Context(), stale, []uint32{1}); !errors.Is(err, mailstore.ErrStaleRef) {
		t.Errorf("FetchHeaders with another UIDVALIDITY = %v, want ErrStaleRef", err)
	}
	if _, err := s.FetchHeaders(t.Context(), mailstore.Source{Folder: "Nope", UIDValidity: 1}, []uint32{1}); !errors.Is(err, mailstore.ErrNotFound) {
		t.Errorf("FetchHeaders in a missing folder = %v, want ErrNotFound", err)
	}
	if hs, err := s.FetchHeaders(t.Context(), src, nil); err != nil || hs != nil {
		t.Errorf("FetchHeaders of no UIDs = %v, %v; want nil, nil", hs, err)
	}
}

// On Gmail the header carries the labels, X-GM-MSGID and the thread ID
// from X-GM-THRID, and References too, which a Gmail summary does not ask
// for.
func TestFetchHeadersGmail(t *testing.T) {
	t.Parallel()
	f := newGmailFixture(t)
	res := f.search(t, "")
	src := res[0].Source
	hs, err := f.store.FetchHeaders(t.Context(), src, []uint32{1, 2})
	if err != nil || len(hs) != 2 {
		t.Fatalf("FetchHeaders = %+v, %v", hs, err)
	}
	invoice, reply := hs[0], hs[1]
	thread := strconv.FormatUint(f.srv.ThreadID(0), 16)
	if invoice.Summary.ThreadID != thread || reply.Summary.ThreadID != thread {
		t.Errorf("thread IDs = %q, %q, want %q", invoice.Summary.ThreadID, reply.Summary.ThreadID, thread)
	}
	if invoice.Summary.Ref.GmailMsgID != f.srv.MsgID(0) || !slices.Contains(invoice.Summary.Labels, "Receipts") ||
		!slices.Contains(invoice.Summary.Labels, `\Inbox`) || !invoice.Summary.HasAttachment {
		t.Errorf("invoice summary = %+v", invoice.Summary)
	}
	if len(invoice.Attachments) != 1 || invoice.Attachments[0].Filename != "invoice.pdf" || invoice.MessageID != "inv@example.com" {
		t.Errorf("invoice header = %+v", invoice)
	}
	if reply.MessageID != "reply@gmail.example" || !slices.Equal(mailstore.MessageIDs(reply.InReplyTo), []string{"inv@example.com"}) ||
		!slices.Equal(mailstore.MessageIDs(reply.References), []string{"inv@example.com"}) {
		t.Errorf("reply header = %+v, want its Message-ID, In-Reply-To and References", reply)
	}
	sums, err := f.store.FetchSummaries(t.Context(), src, []uint32{1, 2}, mailstore.SummaryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range hs {
		if !storetest.SameSummary(hs[i].Summary, sums[i]) {
			t.Errorf("header %d summary = %+v, want FetchSummaries' %+v", i, hs[i].Summary, sums[i])
		}
		if slices.Contains(sums[i].Flags, `\Seen`) {
			t.Errorf("flags of UID %d after FetchHeaders = %q, want no \\Seen", sums[i].Ref.UID, sums[i].Flags)
		}
	}
}

// A message another client added to a folder the connection keeps open is
// found even when the server tells the session of it late, as Gmail does:
// FetchHeaders asks again after opening the folder afresh, as reads do, so
// the cache never takes a new message for an expunged one.
func TestFetchHeadersSeesMailFromElsewhere(t *testing.T) {
	t.Parallel()
	for _, mode := range []struct {
		name string
		opts fakegmail.Options
	}{
		{"late arrivals", fakegmail.Options{LateArrivals: true}},
		{"late exists", fakegmail.Options{LateExists: true}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			reusedFolderSeesMailFromElsewhere(t, mode.opts, nil, func(t *testing.T, s *Store, r ref.Ref, _ *fakegmail.Server) error {
				hs, err := s.FetchHeaders(t.Context(), mailstore.Source{Folder: r.Folder, UIDValidity: r.UIDValidity}, []uint32{r.UID})
				if err == nil && (len(hs) != 1 || hs[0].Summary.Subject != "Arrival" || hs[0].MessageID != "late@example.com") {
					t.Errorf("FetchHeaders = %+v, want the arrival", hs)
				}
				return err
			})
		})
	}
}

// gmailAttachedMessage is a body structure as Gmail sends it for some
// attached messages: the MESSAGE/RFC822 part leaves out the envelope, body
// structure and line count RFC 3501 requires after its size, and carries
// the extension data in their place.
const gmailAttachedMessage = `(("text" "plain" ("charset" "us-ascii") NIL NIL "7bit" 12 1 NIL NIL NIL)` +
	`("MESSAGE" "RFC822" NIL NIL NIL "7BIT" 71499 NIL ("ATTACHMENT" NIL) NIL) "MIXED" ("BOUNDARY" "b1") NIL NIL)`

// A message whose attached message Gmail describes without an envelope is
// read like any other, with the attached message as an attachment. Such a
// message used to fail the whole FETCH and drop the connection, so every
// header and summary batch holding it failed as unavailable, and the cache
// backfill retried that batch forever.
func TestFetchHeadersReadsGmailAttachedMessageWithoutEnvelope(t *testing.T) {
	t.Parallel()
	srv := startScriptServer(t, &scriptServer{n: 2, structure: gmailAttachedMessage})
	s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
	src := mailstore.Source{Folder: "INBOX", UIDValidity: scriptUIDValidity}
	hs, err := s.FetchHeaders(t.Context(), src, []uint32{1, 2})
	if err != nil {
		t.Fatalf("FetchHeaders: %v", err)
	}
	if len(hs) != 2 {
		t.Fatalf("FetchHeaders gave %d headers, want 2", len(hs))
	}
	want := []mailstore.Attachment{{Part: "2", Filename: "message.eml", ContentType: "message/rfc822", Size: 71499}}
	for _, h := range hs {
		if !reflect.DeepEqual(h.Attachments, want) {
			t.Errorf("UID %d attachments = %+v, want %+v", h.Summary.Ref.UID, h.Attachments, want)
		}
	}
	sums, err := s.FetchSummaries(t.Context(), src, []uint32{1, 2}, mailstore.SummaryOptions{})
	if err != nil {
		t.Fatalf("FetchSummaries: %v", err)
	}
	if len(sums) != 2 {
		t.Fatalf("FetchSummaries gave %d summaries, want 2", len(sums))
	}
	for _, sum := range sums {
		if !sum.HasAttachment {
			t.Errorf("UID %d summary has no attachment, want the attached message", sum.Ref.UID)
		}
	}
}
