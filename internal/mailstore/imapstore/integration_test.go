//go:build integration

package imapstore

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/render"
	"github.com/jamesainslie/chainmail/internal/testutil/dovecot"
)

// TestMain runs one Dovecot container for every test in this package and
// removes it afterwards.
func TestMain(m *testing.M) { os.Exit(dovecot.Main(m)) }

// dove is the shared server. It is read at test time: Main starts it after
// package initialisation.
func dove() *dovecot.Server { return dovecot.Shared() }

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

// doveUser is a mailbox of this test's own.
func doveUser(t *testing.T) string {
	return strings.Trim(nonWord.ReplaceAllString(strings.ToLower(t.Name()), "-"), "-") + "@example.test"
}

func doveStore(t *testing.T, user string) *Store {
	t.Helper()
	acct := testAccount("dove", dove().Endpoint, false)
	acct.Username = user
	s := New(acct, passwordCreds(user, dovecot.Password), Options{RootCAs: dove().CA.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	return s
}

type doveMail struct {
	from, subject, messageID, inReplyTo, body string
	references                                []string
	attachment                                []byte
}

func (m doveMail) raw() string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: me@example.test\r\nSubject: %s\r\nDate: Mon, 26 Oct 2026 09:00:00 +0000\r\nMessage-ID: <%s>\r\n", m.from, m.subject, m.messageID)
	if m.inReplyTo != "" {
		fmt.Fprintf(&b, "In-Reply-To: <%s>\r\n", m.inReplyTo)
	}
	if len(m.references) > 0 {
		fmt.Fprintf(&b, "References: <%s>\r\n", strings.Join(m.references, "> <"))
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	if m.attachment == nil {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n" + m.body + "\r\n")
		return b.String()
	}
	b.WriteString("Content-Type: multipart/mixed; boundary=\"b\"\r\n\r\n")
	b.WriteString("--b\r\nContent-Type: multipart/alternative; boundary=\"a\"\r\n\r\n")
	b.WriteString("--a\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + m.body + "\r\n")
	b.WriteString("--a\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>" + m.body + "</p>\r\n--a--\r\n")
	b.WriteString("--b\r\nContent-Type: application/pdf; name=\"invoice.pdf\"\r\nContent-Disposition: attachment; filename=\"invoice.pdf\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n" + wrap76(base64.StdEncoding.EncodeToString(m.attachment)) + "\r\n")
	b.WriteString("--b\r\nContent-Type: text/plain; charset=utf-8; name=\"notes.txt\"\r\nContent-Disposition: attachment; filename=\"notes.txt\"\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\nCosts =E2=82=AC5.\r\n--b--\r\n")
	return b.String()
}

func put(t *testing.T, user, folder string, m doveMail, date time.Time, flags ...imap.Flag) imap.UID {
	t.Helper()
	uid, err := dove().Append(user, folder, m.raw(), date, flags...)
	if err != nil {
		t.Fatalf("append to %s: %v", folder, err)
	}
	return uid
}

func oct(d int) time.Time { return time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC) }

func (s *Store) mustSearch(t *testing.T, q string) map[string][]uint32 {
	t.Helper()
	tree, in := searchTree(t, q)
	res, err := s.Search(t.Context(), mailstore.SearchRequest{Query: tree, In: in})
	if err != nil {
		t.Fatalf("Search(%q): %v", q, err)
	}
	out := map[string][]uint32{}
	for _, r := range res {
		if r.Err != nil {
			t.Fatalf("Search(%q) in %s: %v", q, r.Source.Folder, r.Err)
		}
		if len(r.UIDs) > 0 {
			out[r.Source.Folder] = r.UIDs
		}
	}
	return out
}

func refFor(t *testing.T, user, folder string, uid imap.UID) ref.Ref {
	t.Helper()
	v, err := dove().UIDValidity(user, folder)
	if err != nil {
		t.Fatal(err)
	}
	return ref.Ref{Account: "dove", Folder: folder, UIDValidity: v, UID: uint32(uid)}
}

func TestDovecotFoldersAndRoles(t *testing.T) {
	user := doveUser(t)
	put(t, user, "INBOX", doveMail{from: "a@example.com", subject: "one", messageID: "1@x", body: "x"}, oct(1))
	put(t, user, "INBOX", doveMail{from: "a@example.com", subject: "two", messageID: "2@x", body: "x"}, oct(2), imap.FlagSeen)
	folders, err := doveStore(t, user).ListFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]mailstore.Role{
		"INBOX": mailstore.RoleInbox, "Sent": mailstore.RoleSent, "Drafts": mailstore.RoleDrafts,
		"Archive": mailstore.RoleArchive, "Trash": mailstore.RoleTrash, "Junk": mailstore.RoleSpam,
	}
	for _, f := range folders {
		if role, ok := want[f.Name]; ok && f.Role != role {
			t.Errorf("%s role = %q, want %q", f.Name, f.Role, role)
		}
		if f.Name == "INBOX" && (f.Messages != 2 || f.Unseen != 1) {
			t.Errorf("INBOX counts = %d/%d, want 2/1", f.Messages, f.Unseen)
		}
	}
}

func TestDovecotSearch(t *testing.T) {
	user := doveUser(t)
	put(t, user, "INBOX", doveMail{from: "billing@acme.example", subject: "Invoice October", messageID: "i10@x", body: "Please pay by Friday."}, oct(20))
	put(t, user, "Archive", doveMail{from: "billing@acme.example", subject: "Invoice September", messageID: "i9@x", body: "paid"}, time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC))
	put(t, user, "Trash", doveMail{from: "billing@acme.example", subject: "Invoice old", messageID: "i1@x", body: "old"}, oct(1))
	put(t, user, "Sent", doveMail{from: "me@example.test", subject: "Lunch", messageID: "l@x", body: "noon?"}, oct(21))
	s := doveStore(t, user)

	if got := s.mustSearch(t, "invoice"); len(got) != 2 || len(got["INBOX"]) != 1 || len(got["Archive"]) != 1 {
		t.Errorf("invoice = %v, want INBOX and Archive, not Trash", got)
	}
	if got := s.mustSearch(t, "invoice in:trash"); len(got) != 1 || len(got["Trash"]) != 1 {
		t.Errorf("in:trash = %v", got)
	}
	if got := s.mustSearch(t, "invoice in:anywhere"); len(got) != 3 {
		t.Errorf("in:anywhere = %v", got)
	}
	if got := s.mustSearch(t, "from:acme after:2026-10-01"); len(got) != 1 || got["INBOX"] == nil {
		t.Errorf("from:acme after:2026-10-01 = %v", got)
	}
	tree, _ := searchTree(t, "invoice")
	res, err := s.Search(t.Context(), mailstore.SearchRequest{Query: tree, CountOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var total uint32
	for _, r := range res {
		total += r.Count
		if r.UIDs != nil {
			t.Errorf("count-only search returned UIDs in %s", r.Source.Folder)
		}
	}
	if total != 2 {
		t.Errorf("count = %d, want 2", total)
	}
	tree, _ = searchTree(t, "has:attachment")
	if _, err := s.Search(t.Context(), mailstore.SearchRequest{Query: tree}); !errors.Is(err, mailstore.ErrUnsupportedQuery) {
		t.Errorf("has:attachment on Dovecot = %v, want ErrUnsupportedQuery", err)
	}
}

func TestDovecotSnippetsAndPoolReuse(t *testing.T) {
	user := doveUser(t)
	uid := put(t, user, "INBOX", doveMail{from: "a@example.com", subject: "s", messageID: "s@x", body: "Please pay by Friday."}, oct(20))
	s := doveStore(t, user)
	src := mailstore.Source{Folder: "INBOX", UIDValidity: refFor(t, user, "INBOX", uid).UIDValidity}
	for range 3 {
		sums, err := s.FetchSummaries(t.Context(), src, []uint32{uint32(uid)}, mailstore.SummaryOptions{Snippets: true})
		if err != nil || len(sums) != 1 {
			t.Fatalf("FetchSummaries = %v, %v", sums, err)
		}
		// Dovecot 2.3.21 advertises PREVIEW, so the 2 KiB sample is not needed.
		if sums[0].Preview == nil || sums[0].Sample != nil {
			t.Errorf("snippet source = preview %v, sample %v; want PREVIEW", sums[0].Preview, sums[0].Sample)
		}
		var snippet string
		switch sum := sums[0]; {
		case sum.Preview != nil:
			snippet = render.Clip(*sum.Preview)
		case sum.Sample != nil:
			snippet = render.Snippet(sum.Sample.MediaType, sum.Sample.Charset, sum.Sample.Encoding, sum.Sample.Data)
		}
		if !strings.Contains(snippet, "Please pay") {
			t.Errorf("snippet = %q", snippet)
		}
	}
	s.pool.mu.Lock()
	open, idle := s.pool.open, len(s.pool.idle)
	selected := ""
	if idle == 1 {
		selected = s.pool.idle[0].c.Selected()
	}
	s.pool.mu.Unlock()
	if open != 1 || selected != "INBOX" {
		t.Errorf("after three reads of INBOX: %d open, idle conn on %q; want one conn kept on INBOX", open, selected)
	}
}

func TestDovecotThreadAcrossFolders(t *testing.T) {
	user := doveUser(t)
	orig := put(t, user, "INBOX", doveMail{from: "a@example.com", subject: "Plan", messageID: "t1@x", body: "plan?"}, oct(20))
	put(t, user, "Sent", doveMail{from: "me@example.test", subject: "Re: Plan", messageID: "r1@x", inReplyTo: "t1@x", references: []string{"t1@x"}, body: "yes"}, oct(21))
	put(t, user, "INBOX", doveMail{from: "a@example.com", subject: "Re: Plan", messageID: "r2@x", inReplyTo: "r1@x", references: []string{"t1@x", "r1@x"}, body: "great"}, oct(22))
	put(t, user, "INBOX", doveMail{from: "b@example.com", subject: "Other", messageID: "o@x", body: "x"}, oct(23))
	s := doveStore(t, user)
	refs, err := s.Thread(t.Context(), refFor(t, user, "INBOX", orig))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 {
		t.Fatalf("thread = %v, want 3 messages across INBOX and Sent", refs)
	}
	got := s.FetchMessages(t.Context(), refs)
	threadIDs := map[string]bool{}
	for _, r := range got {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
		threadIDs[r.Message.Summary.ThreadID] = true
	}
	if len(threadIDs) != 1 {
		t.Errorf("thread IDs = %v, want one", threadIDs)
	}
}

func TestDovecotStaleRefAndCursor(t *testing.T) {
	user := doveUser(t)
	uid := put(t, user, "INBOX", doveMail{from: "a@example.com", subject: "s", messageID: "st@x", body: "x"}, oct(20))
	s := doveStore(t, user)
	r := refFor(t, user, "INBOX", uid)
	if got := s.FetchMessages(t.Context(), []ref.Ref{r}); got[0].Err != nil {
		t.Fatal(got[0].Err)
	}
	if err := dove().SetUIDValidity(user, "INBOX", r.UIDValidity+1000); err != nil {
		t.Fatal(err)
	}
	if got := s.FetchMessages(t.Context(), []ref.Ref{r}); !errors.Is(got[0].Err, mailstore.ErrStaleRef) {
		t.Errorf("after a UIDVALIDITY change: %v, want ErrStaleRef", got[0].Err)
	}
	res, err := s.Search(t.Context(), mailstore.SearchRequest{Bounds: map[string]mailstore.Bound{"INBOX": {UIDValidity: r.UIDValidity, Below: 10}}})
	if err != nil || len(res) != 1 || !errors.Is(res[0].Err, mailstore.ErrStaleRef) {
		t.Errorf("continuing a search across a UIDVALIDITY change = %+v, %v", res, err)
	}
}

func TestDovecotRetryAfterKick(t *testing.T) {
	user := doveUser(t)
	put(t, user, "INBOX", doveMail{from: "a@example.com", subject: "k", messageID: "k@x", body: "x"}, oct(20))
	s := doveStore(t, user)
	s.mustSearch(t, "k")
	if err := dove().Kick(user); err != nil {
		t.Fatal(err)
	}
	if got := s.mustSearch(t, "k"); len(got["INBOX"]) != 1 {
		t.Errorf("search after a kick = %v", got)
	}
}

func TestDovecotReadsRenderAndNeverSetSeen(t *testing.T) {
	user := doveUser(t)
	att := make([]byte, 3<<20)
	if _, err := rand.Read(att); err != nil {
		t.Fatal(err)
	}
	inv := put(t, user, "INBOX", doveMail{from: "a@example.com", subject: "Invoice", messageID: "inv@x", body: "Please pay by Friday.", attachment: att}, oct(20))
	reply := put(t, user, "Sent", doveMail{from: "me@example.test", subject: "Re: Invoice", messageID: "rep@x", inReplyTo: "inv@x", references: []string{"inv@x"}, body: "Paid."}, oct(21))
	s := doveStore(t, user)
	r := refFor(t, user, "INBOX", inv)

	got := s.FetchMessages(t.Context(), []ref.Ref{r})
	if got[0].Err != nil {
		t.Fatal(got[0].Err)
	}
	msg := got[0].Message
	body, err := render.Render(msg.Raw, render.Options{})
	if err != nil || body.Text != "Please pay by Friday." {
		t.Errorf("rendered body = %q, %v", body.Text, err)
	}
	var parts []string
	for _, a := range msg.Attachments {
		parts = append(parts, a.Part+" "+a.Filename)
	}
	if !slices.Equal(parts, []string{"2 invoice.pdf", "3 notes.txt"}) {
		t.Errorf("attachments = %v", parts)
	}
	if len(msg.Raw) > 8192 {
		t.Errorf("rebuilt message is %d bytes; attachments should not be fetched", len(msg.Raw))
	}

	for part, want := range map[string][]byte{"2": att, "3": []byte("Costs €5.")} {
		_, rc, err := s.StreamPart(t.Context(), r, part)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || !slices.Equal(data, want) {
			t.Errorf("part %s: %d bytes, %v; want %d", part, len(data), err, len(want))
		}
	}
	_, rc, err := s.StreamPart(t.Context(), r, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
	if _, err := s.Thread(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	src := mailstore.Source{Folder: "INBOX", UIDValidity: r.UIDValidity}
	if _, err := s.FetchSummaries(t.Context(), src, []uint32{r.UID}, mailstore.SummaryOptions{Snippets: true}); err != nil {
		t.Fatal(err)
	}

	for folder, uid := range map[string]imap.UID{"INBOX": inv, "Sent": reply} {
		flags, err := dove().Flags(user, folder, uid)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(flags, imap.FlagSeen) {
			t.Errorf("%s/%d is \\Seen after get, thread, attachment and snippets", folder, uid)
		}
	}
}
