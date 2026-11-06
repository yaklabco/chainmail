package imapstore

import (
	"encoding/base64"
	"errors"
	"io"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/query"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/render"
	"github.com/yaklabco/chainmail/internal/testutil/fakegmail"
	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
)

const (
	gmailReply = "From: Me <me@gmail.example>\r\nTo: Alex Doe <alex@example.com>\r\nSubject: Re: Rechnung\r\n" +
		"Date: Tue, 27 Oct 2026 09:00:00 +0000\r\nMessage-ID: <reply@gmail.example>\r\nIn-Reply-To: <inv@example.com>\r\n" +
		"References: <inv@example.com>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Paid.\r\n\r\nOn Mon, 26 Oct 2026 at 09:00, Alex Doe <alex@example.com> wrote:\r\n> Please pay by Friday.\r\n"
	gmailTrashed = "From: alex@example.com\r\nSubject: Old invoice\r\n\r\nold\r\n"
)

func wrap76(s string) string {
	var b strings.Builder
	for len(s) > 76 {
		b.WriteString(s[:76] + "\r\n")
		s = s[76:]
	}
	b.WriteString(s)
	return b.String()
}

func invoiceRaw(att []byte) string {
	return "From: Alex Doe <alex@example.com>\r\nTo: Me <me@gmail.example>\r\n" +
		"Subject: =?iso-8859-1?q?Rechnung_f=FCr_Oktober?=\r\nDate: Mon, 26 Oct 2026 09:00:00 +0000\r\n" +
		"Message-ID: <inv@example.com>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"b\"\r\n\r\n" +
		"--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nPlease pay by Friday.\r\n" +
		"--b\r\nContent-Type: application/pdf; name=\"invoice.pdf\"\r\nContent-Disposition: attachment; filename=\"invoice.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" + wrap76(base64.StdEncoding.EncodeToString(att)) + "\r\n--b--\r\n"
}

func gmailDay(d int) time.Time { return time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC) }

type gmailFixture struct {
	store *Store
	srv   *fakegmail.Server
	att   []byte
	raw   string
}

// newGmailFixture: message 0 is an invoice with a 2.5 MiB attachment in the
// inbox, message 1 James's reply in Sent (same thread), message 2 in Trash.
// The fake has Gmail's capabilities, so no PREVIEW.
func newGmailFixture(t *testing.T) gmailFixture {
	t.Helper()
	return newGmailFixtureWith(t, fakegmail.Options{})
}

// newGmailFixtureWith is newGmailFixture on a fake started with opts.
func newGmailFixtureWith(t *testing.T, opts fakegmail.Options) gmailFixture {
	t.Helper()
	att := make([]byte, 5<<19)
	rng := rand.New(rand.NewChaCha8([32]byte{1}))
	for i := range att {
		att[i] = byte(rng.Uint32())
	}
	raw := invoiceRaw(att)
	preview := "Please pay by Friday."
	ca := tlstest.New(t)
	srv := fakegmail.StartWith(t, ca, opts, "gmail@example.test", "pw",
		fakegmail.Message{Raw: raw, Date: gmailDay(26), Labels: []string{`\Inbox`, "Receipts"}, Preview: &preview},
		fakegmail.Message{Raw: gmailReply, Date: gmailDay(27), Labels: []string{`\Sent`}, ThreadOf: 1},
		fakegmail.Message{Raw: gmailTrashed, Date: gmailDay(20), Trash: true},
	)
	s := New(testAccount("gmail", srv.Endpoint, true), passwordCreds("gmail@example.test", "pw"), Options{RootCAs: ca.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	return gmailFixture{store: s, srv: srv, att: att, raw: raw}
}

func searchTree(t *testing.T, q string) (*query.Node, string) {
	t.Helper()
	n, err := query.Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	scope, rest := query.Split(n)
	return rest, scope.In
}

func (f gmailFixture) search(t *testing.T, q string) []mailstore.SourceResult {
	t.Helper()
	tree, in := searchTree(t, q)
	res, err := f.store.Search(t.Context(), mailstore.SearchRequest{Query: tree, In: in})
	if err != nil {
		t.Fatalf("Search(%q): %v", q, err)
	}
	return res
}

func (f gmailFixture) refOf(t *testing.T, q string) ref.Ref {
	t.Helper()
	res := f.search(t, q)
	sums, err := f.store.FetchSummaries(t.Context(), res[0].Source, res[0].UIDs[:1], mailstore.SummaryOptions{})
	if err != nil || len(sums) != 1 {
		t.Fatalf("FetchSummaries: %v, %v", sums, err)
	}
	return sums[0].Ref
}

func TestGmailListFolders(t *testing.T) {
	t.Parallel()
	f := newGmailFixture(t)
	folders, err := f.store.ListFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]mailstore.Role{}
	for _, fo := range folders {
		roles[fo.Name] = fo.Role
		if fo.Name == "[Gmail]/All Mail" && (fo.Messages != 2 || fo.Unseen != 2) {
			t.Errorf("All Mail counts = %d/%d, want 2/2", fo.Messages, fo.Unseen)
		}
		if fo.Name == "[Gmail]" && fo.Selectable {
			t.Error("[Gmail] is not selectable")
		}
	}
	want := map[string]mailstore.Role{
		"INBOX": mailstore.RoleInbox, "[Gmail]/All Mail": mailstore.RoleAll, "[Gmail]/Sent Mail": mailstore.RoleSent,
		"[Gmail]/Trash": mailstore.RoleTrash, "[Gmail]/Spam": mailstore.RoleSpam, "Receipts": mailstore.RoleNone,
	}
	for name, role := range want {
		if roles[name] != role {
			t.Errorf("role of %s = %q, want %q", name, roles[name], role)
		}
	}
	if folders[0].Name != "INBOX" {
		t.Errorf("first folder = %s, want INBOX", folders[0].Name)
	}
}

func TestGmailSearch(t *testing.T) {
	t.Parallel()
	f := newGmailFixture(t)
	res := f.search(t, "from:alex after:2026-10-01")
	if len(res) != 1 || res[0].Source.Folder != "[Gmail]/All Mail" || !slices.Equal(res[0].UIDs, []uint32{1}) {
		t.Errorf("search = %+v", res)
	}
	if got := f.srv.LastRaw(); !slices.Equal(got, []string{"from:alex after:2026/10/01"}) {
		t.Errorf("X-GM-RAW sent = %q", got)
	}
	if got := f.srv.RawSearches(); len(got) != 1 || got[0].Folder != "[Gmail]/All Mail" {
		t.Errorf("raw searches = %+v, want one in All Mail", got)
	}
	res = f.search(t, "in:anywhere")
	var folders []string
	for _, r := range res {
		folders = append(folders, r.Source.Folder)
	}
	if !slices.Equal(folders, []string{"[Gmail]/All Mail", "[Gmail]/Trash", "[Gmail]/Spam"}) || !slices.Equal(res[0].UIDs, []uint32{2, 1}) {
		t.Errorf("in:anywhere = %+v", res)
	}
	if res := f.search(t, "has:attachment"); !slices.Equal(res[0].UIDs, []uint32{1}) {
		t.Errorf("has:attachment on Gmail = %+v", res)
	}
	tree, _ := searchTree(t, "alex")
	counted, err := f.store.Search(t.Context(), mailstore.SearchRequest{Query: tree, CountOnly: true})
	if err != nil || counted[0].Count != 2 || counted[0].UIDs != nil {
		t.Errorf("count = %+v, %v", counted, err)
	}
}

func TestGmailSummaries(t *testing.T) {
	t.Parallel()
	f := newGmailFixture(t)
	res := f.search(t, "")
	sums, err := f.store.FetchSummaries(t.Context(), res[0].Source, []uint32{2, 1}, mailstore.SummaryOptions{Snippets: true})
	if err != nil || len(sums) != 2 {
		t.Fatalf("FetchSummaries = %v, %v", sums, err)
	}
	reply, invoice := sums[0], sums[1]
	if invoice.Subject != "Rechnung für Oktober" {
		t.Errorf("subject = %q, want the decoded ISO-8859-1 word", invoice.Subject)
	}
	thread := strconv.FormatUint(f.srv.ThreadID(0), 16)
	if invoice.ThreadID != thread || reply.ThreadID != thread {
		t.Errorf("thread IDs = %q, %q, want %q", invoice.ThreadID, reply.ThreadID, thread)
	}
	if invoice.Ref.GmailMsgID != f.srv.MsgID(0) || !slices.Contains(invoice.Labels, "Receipts") || !invoice.HasAttachment {
		t.Errorf("invoice = %+v", invoice)
	}
	// Gmail has no PREVIEW, so the snippet comes from the first 2 KiB of the text part.
	if invoice.Preview != nil || invoice.Sample == nil {
		t.Fatalf("snippet source = preview %v, sample %v; want the 2 KiB sample", invoice.Preview, invoice.Sample)
	}
	if s := invoice.Sample; s.MediaType != "text/plain" || s.Charset != "utf-8" ||
		render.Snippet(s.MediaType, s.Charset, s.Encoding, s.Data) != "Please pay by Friday." {
		t.Errorf("sample = %+v", s)
	}
	if !invoice.InternalDate.Equal(gmailDay(26)) || len(invoice.From) != 1 || invoice.From[0].Email != "alex@example.com" {
		t.Errorf("invoice header fields = %v %+v", invoice.InternalDate, invoice.From)
	}
}

func TestGmailSummariesWithPreview(t *testing.T) {
	t.Parallel()
	f := newGmailFixtureWith(t, fakegmail.Options{Preview: true})
	res := f.search(t, "")
	sums, err := f.store.FetchSummaries(t.Context(), res[0].Source, []uint32{1}, mailstore.SummaryOptions{Snippets: true})
	if err != nil || len(sums) != 1 {
		t.Fatalf("FetchSummaries = %v, %v", sums, err)
	}
	if p := sums[0].Preview; p == nil || *p != "Please pay by Friday." || sums[0].Sample != nil {
		t.Errorf("snippet source = preview %v, sample %v; want PREVIEW", p, sums[0].Sample)
	}
	sums, err = f.store.FetchSummaries(t.Context(), res[0].Source, []uint32{1}, mailstore.SummaryOptions{})
	if err != nil || len(sums) != 1 || sums[0].Preview != nil || sums[0].Sample != nil {
		t.Errorf("without snippets = %+v, %v; want no preview and no sample", sums, err)
	}
}

func TestGmailFetchMessages(t *testing.T) {
	t.Parallel()
	f := newGmailFixture(t)
	res := f.search(t, "")
	sums, err := f.store.FetchSummaries(t.Context(), res[0].Source, res[0].UIDs, mailstore.SummaryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := f.store.FetchMessages(t.Context(), []ref.Ref{sums[0].Ref, sums[1].Ref})
	for i, r := range got {
		if r.Err != nil {
			t.Fatalf("message %d: %v", i, r.Err)
		}
	}
	reply, invoice := got[0].Message, got[1].Message
	body, err := render.Render(reply.Raw, render.Options{})
	if err != nil || body.Text != "Paid." || !body.QuotedTrimmed {
		t.Errorf("reply body = %+v, %v", body, err)
	}
	if !slices.Equal(reply.References, []string{"inv@example.com"}) || !slices.Equal(reply.InReplyTo, []string{"inv@example.com"}) {
		t.Errorf("reply threading headers = %v %v", reply.References, reply.InReplyTo)
	}
	if len(invoice.Attachments) != 1 || invoice.Attachments[0].Part != "2" || invoice.Attachments[0].Filename != "invoice.pdf" {
		t.Errorf("invoice attachments = %+v", invoice.Attachments)
	}
	if len(invoice.Raw) > 4096 {
		t.Errorf("rebuilt invoice is %d bytes; the attachment body should not be fetched", len(invoice.Raw))
	}
}

func TestGmailFetchMessagesRepeatsAndForeignRefs(t *testing.T) {
	t.Parallel()
	f := newGmailFixture(t)
	r := f.refOf(t, "subject:Rechnung -from:me")
	other := r
	other.Account = "elsewhere"
	got := f.store.FetchMessages(t.Context(), []ref.Ref{r, other, r})
	if len(got) != 3 {
		t.Fatalf("got %d results for 3 refs", len(got))
	}
	for _, i := range []int{0, 2} {
		if got[i].Err != nil || got[i].Message == nil || got[i].Message.Summary.Ref != r {
			t.Errorf("result %d = %+v; a ref given twice should be read for both", i, got[i])
		}
	}
	if !errors.Is(got[1].Err, mailstore.ErrNotFound) {
		t.Errorf("a ref for another account = %v, want ErrNotFound", got[1].Err)
	}
}

func TestGmailRefSurvivesAllMailReset(t *testing.T) {
	t.Parallel()
	f := newGmailFixture(t)
	r := f.refOf(t, "subject:Rechnung -from:me")
	f.srv.ResetAllMail()
	got := f.store.FetchMessages(t.Context(), []ref.Ref{r})
	if got[0].Err != nil {
		t.Fatalf("FetchMessages after reset: %v", got[0].Err)
	}
	fresh := got[0].Message.Summary.Ref
	if fresh.UIDValidity == r.UIDValidity || fresh.GmailMsgID != r.GmailMsgID {
		t.Errorf("fresh ref %v from old %v", fresh, r)
	}
	if f.srv.Logins() < 2 {
		t.Errorf("logins = %d; the dropped connection should have been replaced", f.srv.Logins())
	}
}

func TestGmailThread(t *testing.T) {
	t.Parallel()
	f := newGmailFixture(t)
	r := f.refOf(t, "subject:Rechnung -from:me")
	refs, err := f.store.Thread(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uint64
	for _, tr := range refs {
		ids = append(ids, tr.GmailMsgID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []uint64{f.srv.MsgID(0), f.srv.MsgID(1)}) {
		t.Errorf("thread = %v, want the invoice and the reply", refs)
	}

	// The same message seen from INBOX, by a ref whose UIDVALIDITY is out of
	// date, is found again by X-GM-MSGID and is not counted twice.
	inbox := ref.Ref{Account: r.Account, Folder: "INBOX", UIDValidity: 1, UID: 1, GmailMsgID: r.GmailMsgID}
	refs, err = f.store.Thread(t.Context(), inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Errorf("thread from INBOX = %v, want the invoice and the reply once each", refs)
	}
}

func TestGmailStreamPart(t *testing.T) {
	t.Parallel()
	f := newGmailFixture(t)
	r := f.refOf(t, "has:attachment")
	info, rc, err := f.store.StreamPart(t.Context(), r, "2")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	if cerr := rc.Close(); err == nil {
		err = cerr
	}
	if err != nil || !slices.Equal(data, f.att) {
		t.Errorf("attachment: %d bytes, %v; want %d identical bytes", len(data), err, len(f.att))
	}
	if info.Filename != "invoice.pdf" || info.ContentType != "application/pdf" {
		t.Errorf("info = %+v", info)
	}
	info, rc, err = f.store.StreamPart(t.Context(), r, "")
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(rc)
	_ = rc.Close()
	if string(data) != f.raw || info.ContentType != "message/rfc822" {
		t.Errorf("raw message: %d bytes, want %d", len(data), len(f.raw))
	}
	if _, _, err := f.store.StreamPart(t.Context(), r, "9"); !errors.Is(err, mailstore.ErrNotFound) {
		t.Errorf("missing part = %v, want ErrNotFound", err)
	}
}

func TestGmailRetriesDroppedConnection(t *testing.T) {
	t.Parallel()
	f := newGmailFixture(t)
	f.search(t, "alex")
	f.srv.DropConnections()
	if res := f.search(t, "alex"); res[0].Err != nil || len(res[0].UIDs) != 2 {
		t.Errorf("search after a drop = %+v", res)
	}
}

func TestGmailReadsNeverSetSeen(t *testing.T) {
	t.Parallel()
	f := newGmailFixture(t)
	r := f.refOf(t, "has:attachment")
	res := f.search(t, "")
	if _, err := f.store.FetchSummaries(t.Context(), res[0].Source, res[0].UIDs, mailstore.SummaryOptions{Snippets: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.store.FetchMessages(t.Context(), []ref.Ref{r}); got[0].Err != nil {
		t.Fatal(got[0].Err)
	}
	if _, err := f.store.Thread(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"2", ""} {
		_, rc, err := f.store.StreamPart(t.Context(), r, part)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
	for i := range 3 {
		if slices.Contains(f.srv.Flags(i), imap.FlagSeen) {
			t.Errorf("message %d is \\Seen after reads", i)
		}
	}
	if n := f.srv.WritableSelects(); n != 0 {
		t.Errorf("%d read-write SELECTs; every folder must be opened with EXAMINE", n)
	}
}
