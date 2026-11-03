package fake

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

func cacheDay(d int) time.Time { return time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC) }

// A message in Sent or Drafts has a Bcc header, which bcc: matches, as IMAP
// SEARCH BCC does. One without Bcc never matches bcc:.
func TestFakeBccMatches(t *testing.T) {
	t.Parallel()
	for _, gmail := range []bool{false, true} {
		s := New("acct", gmail)
		folder := "INBOX"
		if gmail {
			folder = "[Gmail]/All Mail"
		}
		r := s.Add(Message{Folder: folder, Subject: "sent", From: []mailstore.Address{{Email: "me@example.test"}}, Bcc: []mailstore.Address{{Name: "Dave", Email: "dave@example.net"}}})
		s.Add(Message{Folder: folder, Subject: "plain", To: []mailstore.Address{{Email: "dave@example.net"}}})
		if n := count(t, s, "bcc:dave", ""); n != 1 {
			t.Errorf("gmail=%v: bcc:dave matched %d, want 1", gmail, n)
		}
		if n := count(t, s, "-bcc:dave", ""); n != 1 {
			t.Errorf("gmail=%v: -bcc:dave matched %d, want 1", gmail, n)
		}
		if raw := s.RawMessage(r); !bytes.Contains(raw, []byte("\r\nBcc: \"Dave\" <dave@example.net>\r\n")) {
			t.Errorf("gmail=%v: raw message lacks the Bcc header:\n%s", gmail, raw)
		}
	}
}

// Add keeps a copy of Bcc, so the caller's slice can change afterwards.
func TestFakeAddCopiesBcc(t *testing.T) {
	t.Parallel()
	s := New("acct", false)
	bcc := []mailstore.Address{{Email: "dave@example.net"}}
	s.Add(Message{Folder: "INBOX", Subject: "s", Bcc: bcc})
	bcc[0].Email = "mallory@example.net"
	if n := count(t, s, "bcc:dave", ""); n != 1 {
		t.Errorf("bcc:dave matched %d after the caller changed its slice, want 1", n)
	}
}

// A message appended with a Bcc header keeps it, as a server stores a draft.
func TestFakeAppendKeepsBcc(t *testing.T) {
	t.Parallel()
	s := New("acct", false)
	raw := "From: me@example.test\r\nTo: bob@example.net\r\nBcc: dave@example.net\r\nSubject: draft\r\n\r\nbody\r\n"
	if _, err := s.Append(t.Context(), "Drafts", nil, cacheDay(20), []byte(raw)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "bcc:dave", "drafts"); n != 1 {
		t.Errorf("bcc:dave in Drafts matched %d, want 1", n)
	}
}

var (
	_ mailstore.HeaderFetcher = (*Store)(nil)
	_ mailstore.Scoper        = (*Store)(nil)
)

func TestFakeFetchHeaders(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	rich := s.Add(Message{
		Folder: "INBOX", Date: cacheDay(26), Subject: "Report", Body: "See attached.",
		From:       []mailstore.Address{{Name: "Alice", Email: "alice@example.net"}},
		To:         []mailstore.Address{{Email: "host@example.test"}},
		Cc:         []mailstore.Address{{Name: "Carol", Email: "carol@example.net"}, {Email: "erin@example.net"}},
		Bcc:        []mailstore.Address{{Email: "dave@example.net"}},
		MessageID:  "<rich@example.net>",
		InReplyTo:  []string{"<root@example.net>"},
		References: []string{"<older@example.net>", "<root@example.net>"},
		Attachments: []Attachment{
			{Part: "2", Filename: "report.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.4")},
		},
	})
	plain := s.Add(Message{Folder: "INBOX", Date: cacheDay(27), Subject: "Plain", Flags: []string{`\Flagged`}})
	src := mailstore.Source{Folder: "INBOX", UIDValidity: rich.UIDValidity}
	hs, err := s.FetchHeaders(t.Context(), src, []uint32{plain.UID, 0, 99, rich.UID})
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 2 || hs[0].Summary.Ref != plain || hs[1].Summary.Ref != rich {
		t.Fatalf("FetchHeaders gave %d headers, want Plain then Report with UIDs 0 and 99 left out", len(hs))
	}
	sums, err := s.FetchSummaries(t.Context(), src, []uint32{plain.UID, rich.UID}, mailstore.SummaryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	msg := s.FetchMessages(t.Context(), []ref.Ref{rich})[0].Message
	want := mailstore.Header{
		Summary:     sums[1],
		Cc:          []mailstore.Address{{Name: "Carol", Email: "carol@example.net"}, {Email: "erin@example.net"}},
		Bcc:         []mailstore.Address{{Email: "dave@example.net"}},
		MessageID:   "rich@example.net",
		InReplyTo:   msg.InReplyTo,
		References:  msg.References,
		Attachments: msg.Attachments,
	}
	if !reflect.DeepEqual(hs[1], want) {
		t.Errorf("header = %+v\nwant %+v", hs[1], want)
	}
	if !reflect.DeepEqual(hs[0], mailstore.Header{Summary: sums[0]}) {
		t.Errorf("plain header = %+v, want only the summary %+v", hs[0], sums[0])
	}
	if got := s.Flags(rich); slices.Contains(got, `\Seen`) {
		t.Errorf("flags after FetchHeaders = %q, want no \\Seen", got)
	}
	if hs, err := s.FetchHeaders(t.Context(), src, nil); err != nil || len(hs) != 0 {
		t.Errorf("FetchHeaders of no UIDs = %v, %v; want none", hs, err)
	}
}

func TestFakeFetchHeadersErrors(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	r := s.Add(Message{Folder: "INBOX", Date: cacheDay(26), Subject: "s"})
	src := mailstore.Source{Folder: "INBOX", UIDValidity: r.UIDValidity}
	stale := src
	stale.UIDValidity++
	if _, err := s.FetchHeaders(t.Context(), stale, []uint32{r.UID}); !errors.Is(err, mailstore.ErrStaleRef) {
		t.Errorf("FetchHeaders with another UIDVALIDITY = %v, want ErrStaleRef", err)
	}
	if _, err := s.FetchHeaders(t.Context(), mailstore.Source{Folder: "Nope", UIDValidity: 1}, []uint32{1}); !errors.Is(err, mailstore.ErrNotFound) {
		t.Errorf("FetchHeaders in a missing folder = %v, want ErrNotFound", err)
	}
	boom := errors.New("boom")
	s.Fail("FetchHeaders", boom)
	if _, err := s.FetchHeaders(t.Context(), src, []uint32{r.UID}); !errors.Is(err, boom) {
		t.Errorf("FetchHeaders with Fail set = %v, want %v", err, boom)
	}
	if n := s.Calls("FetchHeaders"); n != 3 {
		t.Errorf("Calls(FetchHeaders) = %d, want 3", n)
	}
}

func TestFakeFetchHeadersGmail(t *testing.T) {
	t.Parallel()
	s := New("g", true)
	s.AddFolder("Work", mailstore.RoleNone)
	r := s.Add(Message{Folder: "[Gmail]/All Mail", Date: cacheDay(26), Subject: "Hi", Labels: []string{`\Inbox`, "Work"}, MessageID: "hi@example.net"})
	src := mailstore.Source{Folder: r.Folder, UIDValidity: r.UIDValidity}
	hs, err := s.FetchHeaders(t.Context(), src, []uint32{r.UID})
	if err != nil || len(hs) != 1 {
		t.Fatalf("FetchHeaders = %v, %v", hs, err)
	}
	sums, err := s.FetchSummaries(t.Context(), src, []uint32{r.UID}, mailstore.SummaryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h := hs[0]
	if !reflect.DeepEqual(h.Summary, sums[0]) {
		t.Errorf("summary = %+v, want %+v", h.Summary, sums[0])
	}
	if h.Summary.Ref.GmailMsgID == 0 || h.Summary.ThreadID != strconv.FormatUint(h.Summary.Ref.GmailMsgID, 16) || !slices.Contains(h.Summary.Labels, "Work") {
		t.Errorf("summary = %+v, want an X-GM-MSGID, its own thread and the Work label", h.Summary)
	}
}

func TestFakeSearchFolders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		gmail   bool
		in      string
		want    []string
		wantErr error
	}{
		{false, "", []string{"INBOX", "Archive", "Drafts", "Sent"}, nil},
		{false, "anywhere", []string{"INBOX", "Archive", "Drafts", "Junk", "Sent", "Trash"}, nil},
		{false, "trash", []string{"Trash"}, nil},
		{false, "junk", []string{"Junk"}, nil},
		{false, "spam", []string{"Junk"}, nil},
		{false, "inbox", []string{"INBOX"}, nil},
		{false, "Archive", []string{"Archive"}, nil},
		{false, "Nope", nil, mailstore.ErrNotFound},
		{true, "", []string{"[Gmail]/All Mail"}, nil},
		{true, "inbox", []string{"[Gmail]/All Mail"}, nil},
		{true, "trash", []string{"[Gmail]/Trash"}, nil},
		{true, "spam", []string{"[Gmail]/Spam"}, nil},
		{true, "anywhere", []string{"[Gmail]/All Mail", "[Gmail]/Trash", "[Gmail]/Spam"}, nil},
		{true, "Work", []string{"[Gmail]/All Mail"}, nil},
	}
	for _, tt := range tests {
		s := New("acct", tt.gmail)
		got, err := s.SearchFolders(t.Context(), tt.in)
		if tt.wantErr != nil {
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("gmail=%v: SearchFolders(%q) = %q, %v; want %v", tt.gmail, tt.in, got, err, tt.wantErr)
			}
			if _, serr := s.Search(t.Context(), mailstore.SearchRequest{In: tt.in}); !errors.Is(serr, tt.wantErr) {
				t.Errorf("gmail=%v: Search(in:%q) = %v, want the same %v", tt.gmail, tt.in, serr, tt.wantErr)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("gmail=%v: SearchFolders(%q) = %q, %v; want %q", tt.gmail, tt.in, got, err, tt.want)
		}
		res, err := s.Search(t.Context(), mailstore.SearchRequest{In: tt.in, CountOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		var searched []string
		for _, r := range res {
			searched = append(searched, r.Source.Folder)
		}
		if !slices.Equal(got, searched) {
			t.Errorf("gmail=%v: SearchFolders(%q) = %q, but Search ran in %q", tt.gmail, tt.in, got, searched)
		}
	}
}

// A folder added later is in the next answer: the fake keeps no list.
func TestFakeSearchFoldersSeesNewFolders(t *testing.T) {
	t.Parallel()
	s := New("acct", false)
	s.AddFolder("Projects", mailstore.RoleNone)
	got, err := s.SearchFolders(t.Context(), "")
	if want := []string{"INBOX", "Archive", "Drafts", "Projects", "Sent"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("SearchFolders = %q, %v; want %q", got, err, want)
	}
}

func TestFakeSearchFoldersFail(t *testing.T) {
	t.Parallel()
	s := New("acct", false)
	boom := errors.New("boom")
	s.Fail("SearchFolders", boom)
	if _, err := s.SearchFolders(t.Context(), ""); !errors.Is(err, boom) {
		t.Errorf("SearchFolders with Fail set = %v, want %v", err, boom)
	}
	if n := s.Calls("SearchFolders"); n != 1 {
		t.Errorf("Calls(SearchFolders) = %d, want 1", n)
	}
}
