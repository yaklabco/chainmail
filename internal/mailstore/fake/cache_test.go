package fake

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
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

// fakeInvite is a single-part calendar message, as some servers send a
// cancellation, base64-encoded. Its body, the size a server reports, runs
// to the end of the message, the last line break included.
const fakeInvite = "From: olive@example.net\r\nTo: me@example.test\r\nSubject: Cancelled\r\n" +
	"Date: Wed, 28 Oct 2026 09:00:00 +0000\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: text/calendar; method=cancel; charset=ISO-8859-1\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
	"QkVHSU46VkNBTEVOREFSDQpFTkQ6VkNBTEVOREFS\r\n"

// FetchHeaders lists the calendar parts of a seeded message's attachments
// and of an appended message's MIME tree, and StreamPart opens each.
func TestFakeFetchHeadersCalendarParts(t *testing.T) {
	t.Parallel()
	s := New("acct", false)
	ics := []byte("BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nEND:VCALENDAR\r\n")
	seeded := s.Add(Message{Folder: "INBOX", Date: cacheDay(1), Subject: "seeded", Body: "b", Attachments: []Attachment{
		{Part: "2", Filename: "a.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.7")},
		{Part: "3", Filename: "invite.ics", ContentType: "text/calendar; Method=Request; CHARSET=UTF-8", Data: ics},
	}})
	appended, err := s.Append(t.Context(), "INBOX", nil, cacheDay(2), []byte(fakeInvite))
	if err != nil {
		t.Fatal(err)
	}
	plain := s.Add(Message{Folder: "INBOX", Date: cacheDay(3), Subject: "plain", Body: "b"})
	src := mailstore.Source{Folder: "INBOX", UIDValidity: seeded.UIDValidity}
	hs, err := s.FetchHeaders(t.Context(), src, []uint32{seeded.UID, appended.UID, plain.UID})
	if err != nil || len(hs) != 3 {
		t.Fatalf("FetchHeaders = %+v, %v", hs, err)
	}
	want := [][]mailstore.CalendarPart{
		{{Part: "3", Method: "REQUEST", Charset: "utf-8", Size: encodedSize(ics)}},
		{{Part: "1", Method: "CANCEL", Charset: "iso-8859-1", Size: int64(len("QkVHSU46VkNBTEVOREFSDQpFTkQ6VkNBTEVOREFS\r\n"))}},
		nil,
	}
	for i, h := range hs {
		if !reflect.DeepEqual(h.CalendarParts, want[i]) {
			t.Errorf("header %d calendar parts = %+v, want %+v", i, h.CalendarParts, want[i])
		}
	}
	read := func(r ref.Ref, part string) (mailstore.PartInfo, string) {
		t.Helper()
		info, rc, err := s.StreamPart(t.Context(), r, part)
		if err != nil {
			t.Fatalf("StreamPart(%s): %v", part, err)
		}
		defer func() { _ = rc.Close() }()
		data, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return info, string(data)
	}
	if _, data := read(seeded, "3"); data != string(ics) {
		t.Errorf("seeded part 3 = %q, want %q", data, ics)
	}
	info, data := read(appended, "1")
	wantInfo := mailstore.PartInfo{ContentType: "text/calendar", Params: map[string]string{"method": "cancel", "charset": "ISO-8859-1"}, Size: 42}
	if data != "BEGIN:VCALENDAR\r\nEND:VCALENDAR" || !reflect.DeepEqual(info, wantInfo) {
		t.Errorf("appended part 1 = %+v %q, want %+v and the decoded data", info, data, wantInfo)
	}
	if _, _, err := s.StreamPart(t.Context(), appended, "2"); !errors.Is(err, mailstore.ErrNotFound) {
		t.Errorf("StreamPart of a missing part of an appended message = %v, want ErrNotFound", err)
	}
}

// The parts of an appended multipart message are found by their IMAP part
// numbers, with the PartInfo imapstore gives, and only leaves are parts.
func TestFakeStreamPartOfAppendedMessage(t *testing.T) {
	t.Parallel()
	s := New("acct", false)
	raw := "From: a@example.net\r\nSubject: parts\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"m\"\r\n\r\n" +
		"--m\r\nContent-Type: multipart/alternative; boundary=\"a\"\r\n\r\n" +
		"--a\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nCaf=C3=A9\r\n--a--\r\n" +
		"--m\r\nContent-Type: image/png\r\nContent-ID: <img1@example.net>\r\nContent-Transfer-Encoding: base64\r\n\r\naW1n\r\n" +
		"--m\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"=?utf-8?q?R=C3=A9sum=C3=A9.bin?=\"\r\n\r\nraw\r\n--m--\r\n"
	r, err := s.Append(t.Context(), "INBOX", nil, cacheDay(1), []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		part string
		info mailstore.PartInfo
		data string
	}{
		{"1.1", mailstore.PartInfo{ContentType: "text/plain", Params: map[string]string{"charset": "utf-8"}, Size: 9}, "Café"},
		{"2", mailstore.PartInfo{ContentType: "image/png", ContentID: "img1@example.net", Inline: true, Size: 4}, "img"},
		{"3", mailstore.PartInfo{Filename: "Résumé.bin", ContentType: "application/octet-stream", Size: 3}, "raw"},
	}
	for _, tt := range tests {
		info, rc, err := s.StreamPart(t.Context(), r, tt.part)
		if err != nil {
			t.Errorf("StreamPart(%s): %v", tt.part, err)
			continue
		}
		data, _ := io.ReadAll(rc)
		_ = rc.Close()
		if !reflect.DeepEqual(info, tt.info) || string(data) != tt.data {
			t.Errorf("StreamPart(%s) = %+v %q, want %+v %q", tt.part, info, data, tt.info, tt.data)
		}
	}
	for _, part := range []string{"1", "4", "1.2", "x"} {
		if _, _, err := s.StreamPart(t.Context(), r, part); !errors.Is(err, mailstore.ErrNotFound) {
			t.Errorf("StreamPart(%s) = %v, want ErrNotFound", part, err)
		}
	}
}

// forwardedInvite carries its own calendar part and, attached, a message
// with another invite. Only its own part is listed.
const forwardedInvite = "From: olive@example.net\r\nTo: me@example.test\r\nSubject: Fwd: invite\r\n" +
	"Date: Wed, 28 Oct 2026 09:00:00 +0000\r\nMIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
	"--outer\r\nContent-Type: text/plain\r\n\r\nsee below\r\n" +
	"--outer\r\nContent-Type: text/calendar; method=REQUEST\r\n\r\nBEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n" +
	"--outer\r\nContent-Type: message/rfc822\r\n\r\n" +
	"From: amy@example.org\r\nSubject: invite\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n" +
	"--inner\r\nContent-Type: text/plain\r\n\r\nhi\r\n" +
	"--inner\r\nContent-Type: text/calendar; method=REQUEST\r\n\r\nBEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nEND:VCALENDAR\r\n" +
	"--inner--\r\n" +
	"--outer--\r\n"

func TestFakeCalendarPartsSkipAttachedMessages(t *testing.T) {
	t.Parallel()
	s := New("acct", false)
	r, err := s.Append(t.Context(), "INBOX", nil, cacheDay(1), []byte(forwardedInvite))
	if err != nil {
		t.Fatal(err)
	}
	hs, err := s.FetchHeaders(t.Context(), mailstore.Source{Folder: "INBOX", UIDValidity: r.UIDValidity}, []uint32{r.UID})
	if err != nil || len(hs) != 1 {
		t.Fatalf("FetchHeaders = %+v, %v", hs, err)
	}
	want := []mailstore.CalendarPart{{Part: "2", Method: "REQUEST", Size: int64(len("BEGIN:VCALENDAR\r\nEND:VCALENDAR"))}}
	if !reflect.DeepEqual(hs[0].CalendarParts, want) {
		t.Errorf("calendar parts = %+v, want %+v", hs[0].CalendarParts, want)
	}
}
