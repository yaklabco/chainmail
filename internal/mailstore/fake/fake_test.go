package fake

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/mail"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/query"
	"github.com/jamesainslie/chainmail/internal/ref"
)

func mustParse(t *testing.T, s string) *query.Node {
	t.Helper()
	n, err := query.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	_, rest := query.Split(n)
	return rest
}

func TestFakeSearchAndFetch(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	s.Add(Message{
		Folder: "INBOX", Date: now.Add(-time.Hour), Subject: "Invoice 7", Body: "pay me",
		From: []mailstore.Address{{Name: "Acme", Email: "billing@acme.example"}},
	})
	r2 := s.Add(Message{Folder: "INBOX", Date: now, Subject: "Lunch", Body: "invoice attached", Flags: []string{`\Seen`}})
	s.Add(Message{Folder: "Trash", Date: now, Subject: "Invoice old"})

	res, err := s.Search(t.Context(), mailstore.SearchRequest{Query: mustParse(t, "invoice")})
	if err != nil {
		t.Fatal(err)
	}
	var total int
	for _, r := range res {
		if r.Source.Folder == "Trash" {
			t.Error("Trash searched without in:trash")
		}
		total += len(r.UIDs)
	}
	if total != 2 {
		t.Errorf("matches = %d, want 2", total)
	}

	got := s.FetchMessages(t.Context(), []ref.Ref{r2})
	if got[0].Err != nil || got[0].Message.Summary.Subject != "Lunch" {
		t.Fatalf("FetchMessages = %+v", got[0])
	}
	s.SetUIDValidity("INBOX", 99)
	if got := s.FetchMessages(t.Context(), []ref.Ref{r2}); !errors.Is(got[0].Err, mailstore.ErrStaleRef) {
		t.Errorf("after a UIDVALIDITY change: %v, want ErrStaleRef", got[0].Err)
	}
}

func TestFakeRefusesGmailOnlyOperators(t *testing.T) {
	t.Parallel()
	for _, q := range []string{"has:attachment", "filename:pdf", "label:x"} {
		if _, err := New("host", false).Search(t.Context(), mailstore.SearchRequest{Query: mustParse(t, q)}); !errors.Is(err, mailstore.ErrUnsupportedQuery) {
			t.Errorf("%s on a standard account: %v", q, err)
		}
		if _, err := New("gmail", true).Search(t.Context(), mailstore.SearchRequest{Query: mustParse(t, q)}); err != nil {
			t.Errorf("%s on Gmail: %v", q, err)
		}
	}
}

func TestFakeStreamPart(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	r := s.Add(Message{Folder: "INBOX", Subject: "s", Body: "b", Attachments: []Attachment{{Part: "2", Filename: "a.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.7")}}})
	info, rc, err := s.StreamPart(t.Context(), r, "2")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if info.Filename != "a.pdf" || string(data) != "%PDF-1.7" {
		t.Errorf("StreamPart = %+v %q", info, data)
	}
	if _, _, err := s.StreamPart(t.Context(), r, "9"); !errors.Is(err, mailstore.ErrNotFound) {
		t.Errorf("missing part: %v", err)
	}
}

const allMail = "[Gmail]/All Mail"

var errBoom = errors.New("boom")

func day(n int) time.Time { return time.Date(2026, 10, n, 12, 0, 0, 0, time.UTC) }

func search(t *testing.T, s *Store, req mailstore.SearchRequest) []mailstore.SourceResult {
	t.Helper()
	res, err := s.Search(t.Context(), req)
	if err != nil {
		t.Fatalf("Search(%+v): %v", req, err)
	}
	return res
}

func folders(res []mailstore.SourceResult) []string {
	out := make([]string, len(res))
	for i, r := range res {
		out[i] = r.Source.Folder
	}
	return out
}

// subjects maps each source of res to the subjects it matched, in UID order.
func subjects(t *testing.T, s *Store, res []mailstore.SourceResult) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, r := range res {
		if r.Err != nil {
			t.Fatalf("%s: %v", r.Source.Folder, r.Err)
		}
		sums, err := s.FetchSummaries(t.Context(), r.Source, r.UIDs, mailstore.SummaryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		list := []string{}
		for _, sum := range sums {
			list = append(list, sum.Subject)
		}
		out[r.Source.Folder] = list
	}
	return out
}

func summary(t *testing.T, s *Store, r ref.Ref) mailstore.Summary {
	t.Helper()
	got := s.FetchMessages(t.Context(), []ref.Ref{r})
	if got[0].Err != nil {
		t.Fatalf("FetchMessages(%v): %v", r, got[0].Err)
	}
	return got[0].Message.Summary
}

func threadSubjects(t *testing.T, s *Store, r ref.Ref) []string {
	t.Helper()
	refs, err := s.Thread(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, res := range s.FetchMessages(t.Context(), refs) {
		if res.Err != nil {
			t.Fatal(res.Err)
		}
		out = append(out, res.Message.Summary.Subject)
	}
	return out
}

func mustPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s did not panic", name)
		}
	}()
	f()
}

func TestFakeGmailThreadIDs(t *testing.T) {
	t.Parallel()
	s := New("gmail", true)
	root := s.Add(Message{Folder: allMail, Date: day(1), Subject: "root", MessageID: "r@x"})
	reply := s.Add(Message{Folder: allMail, Date: day(2), Subject: "reply", MessageID: "r1@x", References: []string{"<r@x>"}})
	other := s.Add(Message{Folder: allMail, Date: day(3), Subject: "other", MessageID: "o@x"})
	explicit := s.Add(Message{Folder: allMail, Date: day(4), Subject: "explicit", MessageID: "e@x", GmailThreadID: 77})

	rootID := summary(t, s, root).ThreadID
	if _, err := strconv.ParseUint(rootID, 16, 64); err != nil || rootID == "" {
		t.Fatalf("Gmail ThreadID %q is not a hex Gmail thread ID: %v", rootID, err)
	}
	if got := summary(t, s, reply).ThreadID; got != rootID {
		t.Errorf("reply ThreadID = %q, want the root's %q", got, rootID)
	}
	if got := summary(t, s, other).ThreadID; got == rootID || got == "" {
		t.Errorf("unrelated ThreadID = %q, want a fresh one", got)
	}
	if got := summary(t, s, explicit).ThreadID; got != "4d" {
		t.Errorf("explicit ThreadID = %q, want 4d (77 in hex)", got)
	}

	// Gmail threads by its thread ID, not by headers: a message with no
	// References joins when its thread ID matches.
	tid, _ := strconv.ParseUint(rootID, 16, 64)
	s.Add(Message{Folder: allMail, Date: day(0), Subject: "joined", MessageID: "j@x", GmailThreadID: tid})
	s.Add(Message{Folder: "[Gmail]/Trash", Date: day(5), Subject: "trashed", GmailThreadID: tid})
	if got := threadSubjects(t, s, reply); !slices.Equal(got, []string{"joined", "root", "reply"}) {
		t.Errorf("Gmail Thread = %v, want All Mail only, oldest first", got)
	}
	if got := threadSubjects(t, s, other); !slices.Equal(got, []string{"other"}) {
		t.Errorf("Gmail Thread of a lone message = %v", got)
	}
}

func TestFakeHostThread(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	reply := s.Add(Message{Folder: "Sent", Date: day(2), Subject: "reply", MessageID: "r1@x", References: []string{"r@x"}})
	s.Add(Message{Folder: "INBOX", Date: day(3), Subject: "reply2", MessageID: "r2@x", References: []string{"<r@x> <r1@x>"}})
	s.Add(Message{Folder: "INBOX", Date: day(1), Subject: "root", MessageID: "<r@x>"})
	trashed := s.Add(Message{Folder: "Trash", Date: day(4), Subject: "trashed", References: []string{"r@x"}})
	// Matching is on Message-ID and References only, as imapstore searches.
	s.Add(Message{Folder: "INBOX", Date: day(5), Subject: "in-reply-to only", InReplyTo: []string{"r@x"}})
	lone := s.Add(Message{Folder: "INBOX", Date: day(6), Subject: "lone"})

	if got := threadSubjects(t, s, reply); !slices.Equal(got, []string{"root", "reply", "reply2"}) {
		t.Errorf("Thread = %v, want root, reply, reply2", got)
	}
	if got := threadSubjects(t, s, trashed); !slices.Equal(got, []string{"root", "reply", "reply2", "trashed"}) {
		t.Errorf("Thread from Trash = %v, want the seed included", got)
	}
	if got := threadSubjects(t, s, lone); !slices.Equal(got, []string{"lone"}) {
		t.Errorf("Thread without a Message-ID = %v", got)
	}
	if got, want := summary(t, s, reply).ThreadID, mailstore.ThreadKey("r@x"); got != want {
		t.Errorf("host ThreadID = %q, want ThreadKey(root) %q", got, want)
	}
}

func TestFakeGmailFolderModel(t *testing.T) {
	t.Parallel()
	s := New("gmail", true)
	for _, folder := range []string{"INBOX", "[Gmail]/Sent Mail", "nope"} {
		mustPanic(t, "Gmail Add to "+folder, func() { s.Add(Message{Folder: folder}) })
	}
	s.Add(Message{Folder: allMail, Subject: "inbox", Labels: []string{`\Inbox`, "Receipts"}})
	s.Add(Message{Folder: allMail, Subject: "sent", Labels: []string{`\Sent`}})
	s.Add(Message{Folder: allMail, Subject: "draft", Labels: []string{`\Draft`}})
	s.Add(Message{Folder: allMail, Subject: "archived", Labels: []string{"Receipts"}})
	s.Add(Message{Folder: "[Gmail]/Trash", Subject: "trash"})
	s.Add(Message{Folder: "[Gmail]/Spam", Subject: "spam"})

	tests := []struct {
		in   string
		want map[string][]string
	}{
		{"", map[string][]string{allMail: {"archived", "draft", "sent", "inbox"}}},
		{"inbox", map[string][]string{allMail: {"inbox"}}},
		{"INBOX", map[string][]string{allMail: {"inbox"}}},
		{"sent", map[string][]string{allMail: {"sent"}}},
		{"drafts", map[string][]string{allMail: {"draft"}}},
		{"archive", map[string][]string{allMail: {"archived", "draft", "sent"}}},
		{"receipts", map[string][]string{allMail: {"archived", "inbox"}}},
		{"trash", map[string][]string{"[Gmail]/Trash": {"trash"}}},
		{"spam", map[string][]string{"[Gmail]/Spam": {"spam"}}},
		{"junk", map[string][]string{"[Gmail]/Spam": {"spam"}}},
		{"anywhere", map[string][]string{
			allMail: {"archived", "draft", "sent", "inbox"}, "[Gmail]/Trash": {"trash"}, "[Gmail]/Spam": {"spam"},
		}},
	}
	for _, tt := range tests {
		got := subjects(t, s, search(t, s, mailstore.SearchRequest{In: tt.in}))
		if !maps.EqualFunc(got, tt.want, slices.Equal[[]string]) {
			t.Errorf("in:%s = %v, want %v", tt.in, got, tt.want)
		}
	}
	res := search(t, s, mailstore.SearchRequest{In: "anywhere"})
	if got := folders(res); !slices.Equal(got, []string{allMail, "[Gmail]/Trash", "[Gmail]/Spam"}) {
		t.Errorf("in:anywhere sources = %v, want All Mail, Trash, Spam", got)
	}
}

func TestFakeHostInRoles(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	s.AddFolder("Projects", mailstore.RoleNone)
	// A \All folder, such as a Dovecot virtual one, copies every message,
	// so it is searched only when in: names it, as imapstore does.
	s.AddFolder("Virtual/All", mailstore.RoleAll)
	tests := map[string][]string{
		"":            {"INBOX", "Archive", "Drafts", "Projects", "Sent"},
		"anywhere":    {"INBOX", "Archive", "Drafts", "Junk", "Projects", "Sent", "Trash"},
		"Virtual/All": {"Virtual/All"},
		"inbox":       {"INBOX"},
		"sent":        {"Sent"},
		"drafts":      {"Drafts"},
		"archive":     {"Archive"},
		"trash":       {"Trash"},
		"spam":        {"Junk"},
		"junk":        {"Junk"},
		"Projects":    {"Projects"},
	}
	for in, want := range tests {
		if got := folders(search(t, s, mailstore.SearchRequest{In: in})); !slices.Equal(got, want) {
			t.Errorf("in:%q sources = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"nope", "projects", "all", "none"} {
		if _, err := s.Search(t.Context(), mailstore.SearchRequest{In: in}); !errors.Is(err, mailstore.ErrNotFound) {
			t.Errorf("in:%q = %v, want ErrNotFound", in, err)
		}
	}
}

func TestFakeInternalDate(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	s.SetNow(func() time.Time { return day(29) })
	plain := s.Add(Message{Folder: "INBOX", Date: day(10), Subject: "plain"})
	late := s.Add(Message{Folder: "INBOX", Date: day(1), InternalDate: day(28), Subject: "late"})
	undated := s.Add(Message{Folder: "INBOX", InternalDate: day(20), Subject: "undated"})

	if sum := summary(t, s, plain); !sum.Date.Equal(day(10)) || !sum.InternalDate.Equal(day(10)) {
		t.Errorf("plain dates = %v, %v, want both the Date", sum.Date, sum.InternalDate)
	}
	if sum := summary(t, s, late); !sum.Date.Equal(day(1)) || !sum.InternalDate.Equal(day(28)) {
		t.Errorf("late dates = %v, %v", sum.Date, sum.InternalDate)
	}
	if sum := summary(t, s, undated); !sum.Date.Equal(day(20)) || !sum.InternalDate.Equal(day(20)) {
		t.Errorf("undated dates = %v, %v, want InternalDate for both", sum.Date, sum.InternalDate)
	}
	// Relative ages compare the internal date, as IMAP SINCE does.
	got := subjects(t, s, search(t, s, mailstore.SearchRequest{Query: mustParse(t, "newer_than:2d"), In: "inbox"}))
	if !slices.Equal(got["INBOX"], []string{"late"}) {
		t.Errorf("newer_than:2d = %v, want late", got)
	}
}

func TestFakeSizes(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	data := []byte("%PDF-1.7")
	r := s.Add(Message{Folder: "INBOX", Subject: "s", Body: "b", Attachments: []Attachment{{Part: "2", Filename: "a.pdf", ContentType: "application/pdf", Data: data}}})
	encoded := int64(base64.StdEncoding.EncodedLen(len(data)))
	raw := int64(len(s.RawMessage(r)))

	msg := s.FetchMessages(t.Context(), []ref.Ref{r})[0].Message
	if got := msg.Attachments[0].Size; got != encoded {
		t.Errorf("Attachment.Size = %d, want the encoded %d", got, encoded)
	}
	if msg.Summary.Size != raw {
		t.Errorf("Summary.Size = %d, want the raw length %d", msg.Summary.Size, raw)
	}
	info, rc, err := s.StreamPart(t.Context(), r, "2")
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if info.Size != encoded {
		t.Errorf("PartInfo.Size = %d, want the encoded %d", info.Size, encoded)
	}
	info, rc, err = s.StreamPart(t.Context(), r, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if info.Size != raw {
		t.Errorf("whole-message PartInfo.Size = %d, want %d", info.Size, raw)
	}
	// The body is one byte, the raw message far more: larger: and smaller:
	// compare the raw length, as RFC822.SIZE does.
	for q, want := range map[string]int{"larger:10": 1, "smaller:10": 0, fmt.Sprintf("smaller:%d", raw+1): 1} {
		var n int
		for _, res := range search(t, s, mailstore.SearchRequest{Query: mustParse(t, q)}) {
			n += len(res.UIDs)
		}
		if n != want {
			t.Errorf("%s matched %d, want %d", q, n, want)
		}
	}
}

func TestFakeListFolders(t *testing.T) {
	t.Parallel()
	host := New("host", false)
	host.AddFolder("Projects", mailstore.RoleNone)
	host.AddFolder("A", mailstore.RoleNone)
	host.Add(Message{Folder: "INBOX", Subject: "unseen"})
	host.Add(Message{Folder: "INBOX", Subject: "seen", Flags: []string{`\Seen`}})
	host.Add(Message{Folder: "Trash", Subject: "gone", Flags: []string{`\Seen`}})
	got, err := host.ListFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string][2]uint32{}
	var names []string
	for _, f := range got {
		names = append(names, f.Name)
		counts[f.Name] = [2]uint32{f.Messages, f.Unseen}
	}
	if want := []string{"INBOX", "A", "Archive", "Drafts", "Junk", "Projects", "Sent", "Trash"}; !slices.Equal(names, want) {
		t.Errorf("host folders = %v, want %v", names, want)
	}
	if counts["INBOX"] != [2]uint32{2, 1} || counts["Trash"] != [2]uint32{1, 0} || counts["Sent"] != [2]uint32{0, 0} {
		t.Errorf("host counts = %v", counts)
	}

	gmail := New("gmail", true)
	gmail.AddFolder("Receipts", mailstore.RoleNone)
	gmail.Add(Message{Folder: allMail, Subject: "in", Labels: []string{`\Inbox`, "Receipts"}})
	gmail.Add(Message{Folder: allMail, Subject: "out", Labels: []string{`\Sent`}, Flags: []string{`\Seen`}})
	gmail.Add(Message{Folder: "[Gmail]/Trash", Subject: "gone"})
	got, err = gmail.ListFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	names, counts = nil, map[string][2]uint32{}
	for _, f := range got {
		names = append(names, f.Name)
		counts[f.Name] = [2]uint32{f.Messages, f.Unseen}
	}
	if want := []string{"INBOX", allMail, "[Gmail]/Sent Mail", "[Gmail]/Spam", "[Gmail]/Trash", "Receipts"}; !slices.Equal(names, want) {
		t.Errorf("Gmail folders = %v, want %v", names, want)
	}
	want := map[string][2]uint32{
		"INBOX": {1, 1}, "Receipts": {1, 1}, allMail: {2, 1}, "[Gmail]/Sent Mail": {1, 0}, "[Gmail]/Spam": {0, 0}, "[Gmail]/Trash": {1, 1},
	}
	if !maps.Equal(counts, want) {
		t.Errorf("Gmail counts = %v, want %v (label folders count All Mail by label)", counts, want)
	}
}

func TestFakeTestHelpers(t *testing.T) {
	t.Parallel()
	host := New("host", false)
	mustPanic(t, "Add to an unknown folder", func() { host.Add(Message{Folder: "nope"}) })
	mustPanic(t, "AddFolder of an existing name", func() { host.AddFolder("INBOX", mailstore.RoleInbox) })
	mustPanic(t, "SetUIDValidity of an unknown folder", func() { host.SetUIDValidity("Inbox", 9) })
	mustPanic(t, "FailFolder of an unknown folder", func() { host.FailFolder("nope", errors.New("x")) })

	gmail := New("gmail", true)
	explicit := gmail.Add(Message{Folder: allMail, Subject: "explicit", GmailMsgID: 1001})
	first := gmail.Add(Message{Folder: allMail, Subject: "first"})
	second := gmail.Add(Message{Folder: allMail, Subject: "second"})
	if explicit.GmailMsgID != 1001 || first.GmailMsgID != 1000 || second.GmailMsgID != 1002 {
		t.Errorf("X-GM-MSGIDs = %d, %d, %d, want 1001, 1000, 1002", explicit.GmailMsgID, first.GmailMsgID, second.GmailMsgID)
	}
	mustPanic(t, "Add with a GmailMsgID in use", func() { gmail.Add(Message{Folder: allMail, GmailMsgID: 1002}) })
}

func TestFakeSearchBounds(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	for i := range 5 {
		s.Add(Message{Folder: "INBOX", Date: day(10 - i), Subject: fmt.Sprint("m", i+1)})
	}
	s.Add(Message{Folder: "Sent", Subject: "sent"})

	first := search(t, s, mailstore.SearchRequest{In: "inbox"})
	if len(first) != 1 || !slices.Equal(first[0].UIDs, []uint32{5, 4, 3, 2, 1}) || first[0].Count != 5 {
		t.Fatalf("first page = %+v, want UIDs newest first", first)
	}
	// A continuation searches exactly the bounded folders, even when In
	// would no longer resolve.
	for _, in := range []string{"", "inbox", "nope"} {
		res := search(t, s, mailstore.SearchRequest{In: in, Bounds: map[string]mailstore.Bound{"INBOX": {UIDValidity: 1, Below: 3}}})
		if len(res) != 1 || res[0].Source.Folder != "INBOX" || !slices.Equal(res[0].UIDs, []uint32{2, 1}) {
			t.Errorf("continuation with in:%q = %+v, want INBOX UIDs 2, 1", in, res)
		}
	}
	res := search(t, s, mailstore.SearchRequest{Bounds: map[string]mailstore.Bound{"INBOX": {UIDValidity: 1}, "Sent": {UIDValidity: 1, Below: 1}}})
	if got := folders(res); !slices.Equal(got, []string{"INBOX", "Sent"}) || len(res[0].UIDs) != 5 || len(res[1].UIDs) != 0 {
		t.Errorf("Below 0 and Below 1 = %+v", res)
	}

	s.SetUIDValidity("INBOX", 9)
	res = search(t, s, mailstore.SearchRequest{Bounds: map[string]mailstore.Bound{"INBOX": {UIDValidity: 1, Below: 3}, "Sent": {UIDValidity: 1}, "Gone": {UIDValidity: 1}}})
	byFolder := map[string]mailstore.SourceResult{}
	for _, r := range res {
		byFolder[r.Source.Folder] = r
	}
	if !errors.Is(byFolder["INBOX"].Err, mailstore.ErrStaleRef) || byFolder["Sent"].Err != nil || len(byFolder["Sent"].UIDs) != 1 {
		t.Errorf("stale bound = %+v, want a per-source ErrStaleRef", res)
	}
	if !errors.Is(byFolder["Gone"].Err, mailstore.ErrNotFound) {
		t.Errorf("bound on a missing folder = %v, want ErrNotFound", byFolder["Gone"].Err)
	}

	gmail := New("gmail", true)
	gmail.Add(Message{Folder: allMail, Subject: "a", Labels: []string{`\Inbox`}})
	gmail.Add(Message{Folder: allMail, Subject: "b"})
	gmail.Add(Message{Folder: allMail, Subject: "c", Labels: []string{`\Inbox`}})
	res = search(t, gmail, mailstore.SearchRequest{In: "inbox", Bounds: map[string]mailstore.Bound{allMail: {UIDValidity: 1, Below: 3}}})
	if got := subjects(t, gmail, res); !slices.Equal(got[allMail], []string{"a"}) {
		t.Errorf("Gmail continuation of in:inbox = %v, want a", got)
	}
}

func TestFakeCountOnly(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	for range 3 {
		s.Add(Message{Folder: "INBOX", Subject: "invoice"})
	}
	s.Add(Message{Folder: "Sent", Subject: "other"})
	res := search(t, s, mailstore.SearchRequest{Query: mustParse(t, "invoice"), CountOnly: true})
	var total uint32
	for _, r := range res {
		if r.UIDs != nil {
			t.Errorf("%s: CountOnly returned UIDs %v", r.Source.Folder, r.UIDs)
		}
		total += r.Count
	}
	if total != 3 {
		t.Errorf("count = %d, want 3", total)
	}
}

func TestFakeGmailMsgIDRecovery(t *testing.T) {
	t.Parallel()
	s := New("gmail", true)
	r := s.Add(Message{Folder: allMail, Subject: "kept", Attachments: []Attachment{{Part: "2", Filename: "a", Data: []byte("x")}}})
	s.SetUIDValidity(allMail, 9)

	got := s.FetchMessages(t.Context(), []ref.Ref{r})[0]
	if got.Err != nil || got.Message.Summary.Ref.UIDValidity != 9 || got.Message.Summary.Ref.GmailMsgID != r.GmailMsgID {
		t.Fatalf("FetchMessages after a UIDVALIDITY change = %+v, want recovery by X-GM-MSGID", got)
	}
	if _, rc, err := s.StreamPart(t.Context(), r, "2"); err != nil {
		t.Errorf("StreamPart after a UIDVALIDITY change: %v", err)
	} else {
		_ = rc.Close()
	}
	if refs, err := s.Thread(t.Context(), r); err != nil || len(refs) != 1 || refs[0].UIDValidity != 9 {
		t.Errorf("Thread after a UIDVALIDITY change = %v, %v", refs, err)
	}
	gone := r
	gone.GmailMsgID = 4242
	if got := s.FetchMessages(t.Context(), []ref.Ref{gone})[0]; !errors.Is(got.Err, mailstore.ErrStaleRef) {
		t.Errorf("unknown X-GM-MSGID = %v, want ErrStaleRef", got.Err)
	}
}

func TestFakeFailures(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	r := s.Add(Message{Folder: "INBOX", Subject: "a", Attachments: []Attachment{{Part: "2", Filename: "a", Data: []byte("abcdefgh")}}})
	s.Add(Message{Folder: "Sent", Subject: "b"})

	s.FailFolder("INBOX", errBoom)
	res := search(t, s, mailstore.SearchRequest{})
	for _, r := range res {
		switch r.Source.Folder {
		case "INBOX":
			if !errors.Is(r.Err, errBoom) || r.UIDs != nil {
				t.Errorf("failed folder = %+v", r)
			}
		case "Sent":
			if r.Err != nil || len(r.UIDs) != 1 {
				t.Errorf("healthy folder = %+v", r)
			}
		}
	}
	s.FailFolder("INBOX", nil)
	if res := search(t, s, mailstore.SearchRequest{In: "inbox"}); res[0].Err != nil {
		t.Errorf("FailFolder(nil) did not clear: %v", res[0].Err)
	}

	s.FailStreamAfter(3, errBoom)
	_, rc, err := s.StreamPart(t.Context(), r, "2")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if string(data) != "abc" || !errors.Is(err, errBoom) {
		t.Errorf("stream = %q, %v, want abc then boom", data, err)
	}

	s.Fail("Thread", errBoom)
	if _, err := s.Thread(t.Context(), r); !errors.Is(err, errBoom) || s.Calls("Thread") != 1 {
		t.Errorf("Fail(Thread) = %v, calls %d", err, s.Calls("Thread"))
	}
}

func TestFakeFetchSummaries(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	s.Add(Message{Folder: "INBOX", Subject: "one"})
	s.Add(Message{Folder: "INBOX", Subject: "two"})
	src := mailstore.Source{Folder: "INBOX", UIDValidity: 1}
	sums, err := s.FetchSummaries(t.Context(), src, []uint32{2, 99, 1}, mailstore.SummaryOptions{Snippets: true})
	if err != nil || len(sums) != 2 || sums[0].Subject != "two" || sums[1].Subject != "one" || sums[0].Sample == nil {
		t.Errorf("FetchSummaries = %+v, %v, want two then one, the missing UID left out", sums, err)
	}
	s.SetUIDValidity("INBOX", 2)
	if _, err := s.FetchSummaries(t.Context(), src, []uint32{1}, mailstore.SummaryOptions{}); !errors.Is(err, mailstore.ErrStaleRef) {
		t.Errorf("FetchSummaries after a UIDVALIDITY change = %v, want ErrStaleRef", err)
	}
}

func TestFakeDefensiveCopies(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	in := Message{
		Folder: "INBOX", Subject: "s", MessageID: "m@x",
		From: []mailstore.Address{{Email: "a@x"}}, To: []mailstore.Address{{Email: "b@x"}}, Cc: []mailstore.Address{{Email: "c@x"}},
		InReplyTo: []string{"p@x"}, References: []string{"r@x", "p@x"},
	}
	r := s.Add(in)
	in.From[0].Email = "changed"

	msg := s.FetchMessages(t.Context(), []ref.Ref{r})[0].Message
	msg.Summary.From[0].Email = "changed"
	msg.Summary.To[0].Email = "changed"
	msg.Cc[0].Email = "changed"
	msg.InReplyTo[0] = "changed"
	msg.References[0] = "changed"

	again := s.FetchMessages(t.Context(), []ref.Ref{r})[0].Message
	if again.Summary.From[0].Email != "a@x" || again.Summary.To[0].Email != "b@x" || again.Cc[0].Email != "c@x" ||
		again.InReplyTo[0] != "p@x" || again.References[0] != "r@x" {
		t.Errorf("the fake shares slices with callers: %+v", again)
	}
}

func TestFakeRawMessageHeaders(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	r := s.Add(Message{Folder: "INBOX", Subject: "s", MessageID: "m@x", InReplyTo: []string{"p@x"}, References: []string{"<r@x> <q@x>", "p@x"}})
	for _, raw := range [][]byte{s.RawMessage(r), s.FetchMessages(t.Context(), []ref.Ref{r})[0].Message.Raw} {
		m, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if got := m.Header.Get("In-Reply-To"); got != "<p@x>" {
			t.Errorf("In-Reply-To = %q", got)
		}
		if got := m.Header.Get("References"); got != "<r@x> <q@x> <p@x>" {
			t.Errorf("References = %q", got)
		}
	}
}

func TestFakeFetchSummariesUnknownFolder(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	if _, err := s.FetchSummaries(t.Context(), mailstore.Source{Folder: "nope"}, []uint32{1}, mailstore.SummaryOptions{}); !errors.Is(err, mailstore.ErrNotFound) {
		t.Errorf("FetchSummaries on a missing folder = %v, want ErrNotFound", err)
	}
}

func TestFakeRawAttachmentFilename(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	r := s.Add(Message{Folder: "INBOX", Subject: "s", Attachments: []Attachment{{Part: "2", Filename: `Été "q".pdf`, ContentType: "application/pdf", Data: []byte("x")}}})
	m, err := mail.ReadMessage(bytes.NewReader(s.RawMessage(r)))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	part, err := mr.NextPart()
	for err == nil && part.FileName() == "" {
		part, err = mr.NextPart()
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := part.FileName(); got != `Été "q".pdf` {
		t.Errorf("attachment filename in the raw message = %q", got)
	}
}

// count is the number of matches for q across every source.
func count(t *testing.T, s *Store, q, in string) int {
	t.Helper()
	var n int
	for _, r := range search(t, s, mailstore.SearchRequest{Query: mustParse(t, q), In: in}) {
		n += len(r.UIDs)
	}
	return n
}

func TestFakeBcc(t *testing.T) {
	t.Parallel()
	// A received message has no Bcc: bcc: matches nothing and -bcc:
	// everything, on both kinds of account.
	for _, gmail := range []bool{false, true} {
		s := New("acct", gmail)
		folder := "INBOX"
		if gmail {
			folder = "[Gmail]/All Mail"
		}
		s.Add(Message{Folder: folder, Subject: "s", To: []mailstore.Address{{Email: "x@example.com"}}})
		if n := count(t, s, "bcc:x", ""); n != 0 {
			t.Errorf("gmail=%v: bcc:x matched %d, want 0", gmail, n)
		}
		if n := count(t, s, "-bcc:x", ""); n != 1 {
			t.Errorf("gmail=%v: -bcc:x matched %d, want 1", gmail, n)
		}
	}
}

func TestFakeHostAgesAreDays(t *testing.T) {
	t.Parallel()
	// IMAP SINCE and BEFORE compare whole days, so on a standard account
	// newer_than:1d is since the start of yesterday, as imapstore sends it.
	at := func(d, h int) time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, time.UTC) }
	host := New("host", false)
	host.SetNow(func() time.Time { return at(29, 12) })
	host.Add(Message{Folder: "INBOX", InternalDate: at(28, 6), Subject: "early yesterday"})
	host.Add(Message{Folder: "INBOX", InternalDate: at(27, 23), Subject: "late two days ago"})
	if n := count(t, host, "newer_than:1d", ""); n != 1 {
		t.Errorf("host newer_than:1d matched %d, want 1", n)
	}
	if n := count(t, host, "older_than:1d", ""); n != 1 {
		t.Errorf("host older_than:1d matched %d, want 1", n)
	}
	// Gmail compares the time itself.
	gmail := New("gmail", true)
	gmail.SetNow(func() time.Time { return at(29, 12) })
	gmail.Add(Message{Folder: "[Gmail]/All Mail", InternalDate: at(28, 6), Subject: "early yesterday"})
	if n := count(t, gmail, "newer_than:1d", ""); n != 0 {
		t.Errorf("gmail newer_than:1d matched %d, want 0", n)
	}
}

func TestFakeMonthEndAges(t *testing.T) {
	t.Parallel()
	// A month back from 31 March is 28 February, not 3 March.
	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	for _, gmail := range []bool{false, true} {
		s := New("acct", gmail)
		folder := "INBOX"
		if gmail {
			folder = "[Gmail]/All Mail"
		}
		s.SetNow(func() time.Time { return now })
		s.Add(Message{Folder: folder, InternalDate: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC), Subject: "2 March"})
		s.Add(Message{Folder: folder, InternalDate: time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC), Subject: "27 February"})
		if n := count(t, s, "newer_than:1m", ""); n != 1 {
			t.Errorf("gmail=%v: newer_than:1m on 31 March matched %d, want 1", gmail, n)
		}
	}
	leap := New("host", false)
	leap.SetNow(func() time.Time { return time.Date(2028, 2, 29, 12, 0, 0, 0, time.UTC) })
	leap.Add(Message{Folder: "INBOX", InternalDate: time.Date(2027, 2, 28, 1, 0, 0, 0, time.UTC), Subject: "28 February"})
	if n := count(t, leap, "newer_than:1y", ""); n != 1 {
		t.Errorf("newer_than:1y on 29 February matched %d, want 1", n)
	}
}

func TestFakeHostRefusesOutOfRange(t *testing.T) {
	t.Parallel()
	now := func() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) }
	// RFC 3501 sizes are 32 bits, and dates go back to 1970, as in imapstore.
	for _, q := range []string{
		"larger:4294967296", "smaller:4096M", "a OR -larger:5000M",
		"before:1969-12-31", "after:0001-01-01", "older_than:57y", "newer_than:9999y", "a OR -older_than:100y",
	} {
		host := New("host", false)
		host.SetNow(now)
		if _, err := host.Search(t.Context(), mailstore.SearchRequest{Query: mustParse(t, q)}); !errors.Is(err, mailstore.ErrUnsupportedQuery) {
			t.Errorf("%s on a standard account: %v, want ErrUnsupportedQuery", q, err)
		}
		gmail := New("gmail", true)
		gmail.SetNow(now)
		if _, err := gmail.Search(t.Context(), mailstore.SearchRequest{Query: mustParse(t, q)}); err != nil {
			t.Errorf("%s on Gmail: %v", q, err)
		}
	}
	// The limits themselves are allowed.
	host := New("host", false)
	host.SetNow(now)
	for _, q := range []string{"larger:4294967295", "after:1970-01-01", "newer_than:56y"} {
		if _, err := host.Search(t.Context(), mailstore.SearchRequest{Query: mustParse(t, q)}); err != nil {
			t.Errorf("%s on a standard account: %v", q, err)
		}
	}
}
