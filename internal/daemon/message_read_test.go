package daemon_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
)

const quotedReply = "Thanks.\n\nOn Mon, 26 Oct 2026 at 09:00, Alex Doe <alex@example.com> wrote:\n> are we on?"

// bigBody is a body of 6 MiB: two fit in a 16 MiB response, three do not.
var bigBody = strings.Repeat("y", 6<<20)

func (m fakeMail) get(t *testing.T, full bool, refs ...string) *chainmailv1.GetResponse {
	t.Helper()
	resp, err := m.Client.Message.Get(t.Context(), connect.NewRequest(&chainmailv1.GetRequest{Refs: refs, Full: full}))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return resp.Msg
}

func TestGetBatchWithPerItemErrors(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	a := m.host.Add(fake.Message{Folder: "INBOX", Subject: "A", Body: quotedReply})
	b := m.gmail.Add(fake.Message{Folder: "[Gmail]/All Mail", Subject: "B", Body: "B body"})
	stale := ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: 9, UID: 1}
	other := ref.Ref{Account: "other", Folder: "INBOX", UIDValidity: 1, UID: 1}
	resp := m.get(t, false, a.String(), b.String(), stale.String(), "nope", other.String())
	items := resp.GetItems()
	if len(items) != 5 {
		t.Fatalf("items = %d", len(items))
	}
	if body := items[0].GetMessage().GetBody(); body.GetUntrusted() != "Thanks." || !body.GetQuotedTrimmed() || body.GetTrimmedBytes() == 0 {
		t.Errorf("A body = %v", body)
	}
	if items[1].GetMessage().GetSubject().GetUntrusted() != "B" || len(items[1].GetMessage().GetLabels()) != 0 {
		t.Errorf("B = %v", items[1].GetMessage())
	}
	for i, want := range map[int]string{2: api.ReasonStaleRef, 3: api.ReasonInvalidArgument, 4: api.ReasonNotFound} {
		if got := items[i].GetError().GetReason(); got != want {
			t.Errorf("item %d reason = %q, want %s", i, got, want)
		}
	}
	if items[2].GetError().GetHint() != "search again" {
		t.Errorf("stale hint = %q", items[2].GetError().GetHint())
	}
	if resp.GetSource() != "server" || resp.GetFetchedAt().AsTime() != testNow {
		t.Errorf("source %q, fetched at %v", resp.GetSource(), resp.GetFetchedAt().AsTime())
	}
	row := lastMailAudit(t, m)
	wantTargets := fmt.Sprintf(`[%q,%q,%q,"(invalid)",%q,"messages=2","errors=3"]`, a.String(), b.String(), stale.String(), other.String())
	if row.Action != "get" || row.Account != "" || row.Targets != wantTargets || row.Result != "ok" {
		t.Errorf("audit = %+v, want targets %s", row, wantTargets)
	}
	if strings.Contains(row.Targets, "Thanks") || strings.Contains(row.Targets, "B body") {
		t.Errorf("audit holds content: %s", row.Targets)
	}
}

func TestGetRepeatedRef(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	r := m.host.Add(fake.Message{Folder: "INBOX", Subject: "twice", Body: quotedReply, Flags: []string{`\Flagged`}})
	items := m.get(t, false, r.String(), r.String()).GetItems()
	for i, it := range items {
		if it.GetMessage().GetSubject().GetUntrusted() != "twice" || it.GetMessage().GetBody().GetUntrusted() != "Thanks." {
			t.Errorf("item %d = %v", i, it)
		}
	}
	if row := lastMailAudit(t, m); row.Account != "host" || row.Targets != fmt.Sprintf(`[%q,%q,"messages=2","errors=0"]`, r.String(), r.String()) {
		t.Errorf("audit = %+v", row)
	}
}

func TestGetRefCount(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	for _, n := range []int{0, 51} {
		refs := make([]string, n)
		for i := range refs {
			refs[i] = ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: 1, UID: uint32(i + 1)}.String()
		}
		_, err := m.Client.Message.Get(t.Context(), connect.NewRequest(&chainmailv1.GetRequest{Refs: refs}))
		if reasonOf(err) != api.ReasonInvalidArgument {
			t.Errorf("%d refs: %v, want INVALID_ARGUMENT", n, err)
		}
	}
}

func TestGetTruncatesAtBodyLimit(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	r := m.host.Add(fake.Message{Folder: "INBOX", Subject: "long", Body: strings.Repeat("x", 3000)})
	body := m.get(t, false, r.String()).GetItems()[0].GetMessage().GetBody()
	if !body.GetTruncated() || len(body.GetUntrusted()) != 1024 {
		t.Errorf("limited body: truncated %t, %d bytes", body.GetTruncated(), len(body.GetUntrusted()))
	}
	body = m.get(t, true, r.String()).GetItems()[0].GetMessage().GetBody()
	if body.GetTruncated() || len(body.GetUntrusted()) != 3000 {
		t.Errorf("full body: truncated %t, %d bytes", body.GetTruncated(), len(body.GetUntrusted()))
	}
}

func TestGetUntrustedEnvelope(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	r := m.host.Add(fake.Message{
		Folder: "INBOX", Subject: "Hi\u202e there", Body: "red \x1b[31malert",
		From: []mailstore.Address{{Name: "Ali\u200bce", Email: "alice@example.com"}},
	})
	resp := m.get(t, false, r.String())
	js, err := protojson.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"subject":{"untrusted":"Hi there"}`, `"name":{"untrusted":"Alice"}`, `"untrusted":"red [31malert"`} {
		if !strings.Contains(string(js), want) {
			t.Errorf("response lacks %s:\n%s", want, js)
		}
	}
}

// TestGetResponseCap fills a response past 16 MiB: the item that would
// cross the cap fails, and so does every later one, even one that would fit.
func TestGetResponseCap(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	var refs []string
	for i := range 3 {
		refs = append(refs, m.host.Add(fake.Message{Folder: "INBOX", Subject: fmt.Sprint("big ", i), Body: bigBody}).String())
	}
	refs = append(refs, m.host.Add(fake.Message{Folder: "INBOX", Subject: "small", Body: "small"}).String())
	items := m.get(t, true, refs...).GetItems()
	for i, it := range items[:2] {
		if len(it.GetMessage().GetBody().GetUntrusted()) != len(bigBody) {
			t.Errorf("item %d = %d body bytes, error %v", i, len(it.GetMessage().GetBody().GetUntrusted()), it.GetError())
		}
	}
	for i, it := range items[2:] {
		e := it.GetError()
		if e.GetReason() != api.ReasonInvalidArgument || e.GetHint() != "response too large: fetch fewer refs, or use get --raw for one message" {
			t.Errorf("item %d error = %v", i+2, e)
		}
		if it.GetRef() != refs[i+2] {
			t.Errorf("item %d ref = %q", i+2, it.GetRef())
		}
	}
	if row := lastMailAudit(t, m); !strings.HasSuffix(row.Targets, `"messages=2","errors=2"]`) || row.Result != "ok" {
		t.Errorf("audit = %+v", row)
	}
}

func TestGetRaw(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	r := m.host.Add(fake.Message{Folder: "INBOX", Subject: "raw", Body: strings.Repeat("line\r\n", 20000)})
	stream, err := m.Client.Message.GetRaw(t.Context(), connect.NewRequest(&chainmailv1.GetRawRequest{Ref: r.String()}))
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	chunks := 0
	for stream.Receive() {
		got.Write(stream.Msg().GetData())
		chunks++
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if !bytes.Equal(got.Bytes(), m.host.RawMessage(r)) {
		t.Errorf("raw = %d bytes, want %d", got.Len(), len(m.host.RawMessage(r)))
	}
	if want := (got.Len() + 64<<10 - 1) / (64 << 10); chunks != want {
		t.Errorf("chunks = %d, want %d of 64 KiB", chunks, want)
	}
	row := lastMailAudit(t, m)
	if row.Action != "get.raw" || row.Account != "host" || row.Targets != fmt.Sprintf(`[%q,"bytes=%d"]`, r.String(), got.Len()) {
		t.Errorf("audit = %+v", row)
	}
}

func TestGetRawErrors(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	gone := ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: 1, UID: 99}
	for s, want := range map[string]string{"nope": api.ReasonInvalidArgument, gone.String(): api.ReasonStaleRef} {
		stream, err := m.Client.Message.GetRaw(t.Context(), connect.NewRequest(&chainmailv1.GetRawRequest{Ref: s}))
		if err != nil {
			t.Fatal(err)
		}
		for stream.Receive() {
		}
		if reasonOf(stream.Err()) != want {
			t.Errorf("ref %q: %v, want %s", s, stream.Err(), want)
		}
		_ = stream.Close()
	}
	rows := m.AuditRows(t)
	targets := map[string]bool{}
	for _, row := range rows {
		targets[row.Targets] = row.Action == "get.raw" && row.Result == "error"
	}
	for _, want := range []string{`["(invalid)","bytes=0"]`, fmt.Sprintf(`[%q,"bytes=0"]`, gone.String())} {
		if !targets[want] {
			t.Errorf("no get.raw error row with targets %s in %+v", want, rows)
		}
	}
}

func TestGetRawFailsMidStream(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	r := m.host.Add(fake.Message{Folder: "INBOX", Subject: "raw", Body: "hello"})
	m.host.FailStreamAfter(5, fmt.Errorf("%w: connection reset", mailstore.ErrUnavailable))
	stream, err := m.Client.Message.GetRaw(t.Context(), connect.NewRequest(&chainmailv1.GetRawRequest{Ref: r.String()}))
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	for stream.Receive() {
		got.Write(stream.Msg().GetData())
	}
	if got.Len() != 5 || reasonOf(stream.Err()) != api.ReasonServerUnavailable {
		t.Errorf("got %d bytes then %v; want 5 bytes then SERVER_UNAVAILABLE", got.Len(), stream.Err())
	}
	_ = stream.Close()
	if row := lastMailAudit(t, m); row.Result != "error" || row.Targets != fmt.Sprintf(`[%q,"bytes=5"]`, r.String()) {
		t.Errorf("audit = %+v", row)
	}
}

// TestStreamFailureAfterBytesIsUnavailable checks that a failure after bytes
// went out ends the stream with SERVER_UNAVAILABLE, whatever the store said.
func TestStreamFailureAfterBytesIsUnavailable(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	r := m.host.Add(fake.Message{Folder: "INBOX", Subject: "raw", Body: "hello"})
	m.host.FailStreamAfter(5, fmt.Errorf("%w: part vanished", mailstore.ErrNotFound))
	stream, err := m.Client.Message.GetRaw(t.Context(), connect.NewRequest(&chainmailv1.GetRawRequest{Ref: r.String()}))
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
	}
	if reasonOf(stream.Err()) != api.ReasonServerUnavailable {
		t.Errorf("err = %v, want SERVER_UNAVAILABLE", stream.Err())
	}
	_ = stream.Close()
}

func TestGetThreadOldestFirst(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(3), Subject: "plan", MessageID: "r@x", Body: "plan?"})
	reply := m.host.Add(fake.Message{Folder: "Sent", Date: daysAgo(2), Subject: "re", MessageID: "r1@x", References: []string{"r@x"}, Body: quotedReply})
	m.host.Add(fake.Message{Folder: "INBOX", Date: daysAgo(1), Subject: "re re", MessageID: "r2@x", References: []string{"r@x", "r1@x"}, Body: "ok"})
	m.host.Add(fake.Message{Folder: "Trash", Date: daysAgo(1), Subject: "trashed", References: []string{"r@x"}})
	resp, err := m.Client.Message.GetThread(t.Context(), connect.NewRequest(&chainmailv1.GetThreadRequest{Ref: reply.String()}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, msg := range resp.Msg.GetMessages() {
		got = append(got, msg.GetSubject().GetUntrusted())
	}
	if strings.Join(got, ",") != "plan,re,re re" || resp.Msg.GetThreadId() != mailstore.ThreadKey("r@x") {
		t.Errorf("thread = %v, id %q", got, resp.Msg.GetThreadId())
	}
	if resp.Msg.GetTruncated() || len(resp.Msg.GetOmittedRefs()) != 0 {
		t.Errorf("truncated %t, omitted %v", resp.Msg.GetTruncated(), resp.Msg.GetOmittedRefs())
	}
	if resp.Msg.GetMessages()[1].GetBody().GetUntrusted() != "Thanks." {
		t.Errorf("thread bodies are not trimmed: %q", resp.Msg.GetMessages()[1].GetBody().GetUntrusted())
	}
	if row := lastMailAudit(t, m); row.Action != "thread" || row.Account != "host" || row.Targets != fmt.Sprintf(`[%q,"messages=3"]`, reply.String()) {
		t.Errorf("audit = %+v", row)
	}
}

func TestGetThreadErrors(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	stale := ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: 9, UID: 1}
	for s, want := range map[string]string{"nope": api.ReasonInvalidArgument, stale.String(): api.ReasonStaleRef} {
		_, err := m.Client.Message.GetThread(t.Context(), connect.NewRequest(&chainmailv1.GetThreadRequest{Ref: s}))
		if reasonOf(err) != want {
			t.Errorf("ref %q: %v, want %s", s, err, want)
		}
	}
}

// TestGetThreadResponseCap checks that a thread too large for one response
// returns its oldest messages that fit, flagged truncated, with the refs of
// the rest.
func TestGetThreadResponseCap(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	var refs []string
	for i := range 3 {
		refs = append(refs, m.host.Add(fake.Message{
			Folder: "INBOX", Date: daysAgo(3 - i), Subject: fmt.Sprint("part ", i),
			MessageID: fmt.Sprintf("p%d@x", i), References: []string{"p0@x"}, Body: bigBody,
		}).String())
	}
	resp, err := m.Client.Message.GetThread(t.Context(), connect.NewRequest(&chainmailv1.GetThreadRequest{Ref: refs[2], Full: true}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, msg := range resp.Msg.GetMessages() {
		got = append(got, msg.GetSubject().GetUntrusted())
	}
	if strings.Join(got, ",") != "part 0,part 1" || !resp.Msg.GetTruncated() || strings.Join(resp.Msg.GetOmittedRefs(), ",") != refs[2] {
		t.Errorf("thread = %v, truncated %t, omitted %v", got, resp.Msg.GetTruncated(), resp.Msg.GetOmittedRefs())
	}
	if resp.Msg.GetThreadId() != mailstore.ThreadKey("p0@x") {
		t.Errorf("thread id = %q", resp.Msg.GetThreadId())
	}
	if row := lastMailAudit(t, m); row.Targets != fmt.Sprintf(`[%q,"messages=2","omitted=1"]`, refs[2]) {
		t.Errorf("audit = %+v", row)
	}
}

func TestDownloadAttachment(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	data := []byte("%PDF-1.7")
	r := m.gmail.Add(fake.Message{Folder: "[Gmail]/All Mail", Subject: "pdf", Attachments: []fake.Attachment{{Part: "2", Filename: "a.pdf", ContentType: "application/pdf", Data: data}}})
	stream, err := m.Client.Message.DownloadAttachment(t.Context(), connect.NewRequest(&chainmailv1.DownloadAttachmentRequest{Ref: r.String(), Part: "2"}))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() {
		t.Fatalf("no header: %v", stream.Err())
	}
	h := stream.Msg().GetHeader()
	if h.GetFilename().GetUntrusted() != "a.pdf" || h.GetContentType() != "application/pdf" || h.GetSize() != int64(base64.StdEncoding.EncodedLen(len(data))) {
		t.Errorf("header = %v", h)
	}
	var got bytes.Buffer
	for stream.Receive() {
		got.Write(stream.Msg().GetData())
	}
	if err := stream.Err(); err != nil || !bytes.Equal(got.Bytes(), data) {
		t.Errorf("data = %q, %v", got.Bytes(), err)
	}
	_ = stream.Close()
	if row := lastMailAudit(t, m); row.Action != "attachment" || row.Account != "gmail" || row.Targets != fmt.Sprintf(`[%q,"part=2","bytes=8"]`, r.String()) {
		t.Errorf("audit = %+v", row)
	}
	for part, want := range map[string]string{"x": api.ReasonInvalidArgument, "": api.ReasonInvalidArgument, "9": api.ReasonNotFound} {
		stream, err := m.Client.Message.DownloadAttachment(t.Context(), connect.NewRequest(&chainmailv1.DownloadAttachmentRequest{Ref: r.String(), Part: part}))
		if err != nil {
			t.Fatal(err)
		}
		for stream.Receive() {
		}
		if reasonOf(stream.Err()) != want {
			t.Errorf("part %q: %v, want %s", part, stream.Err(), want)
		}
		_ = stream.Close()
	}
	targets := map[string]bool{}
	for _, row := range m.AuditRows(t) {
		targets[row.Targets] = row.Action == "attachment" && row.Result == "error"
	}
	for _, want := range []string{fmt.Sprintf(`[%q,"part=(invalid)","bytes=0"]`, r.String()), fmt.Sprintf(`[%q,"part=9","bytes=0"]`, r.String())} {
		if !targets[want] {
			t.Errorf("no attachment error row with targets %s", want)
		}
	}
}

func TestAttachmentContentTypeNormalized(t *testing.T) {
	t.Parallel()
	m := startFakeMail(t)
	r := m.host.Add(fake.Message{Folder: "INBOX", Subject: "types", Attachments: []fake.Attachment{
		{Part: "2", Filename: "a.pdf", ContentType: `Application/PDF; name="a.pdf"`, Data: []byte("a")},
		{Part: "3", Filename: "b.bin", ContentType: "not a type\x1b[31m", Data: []byte("b")},
		{Part: "4", Filename: "c.txt", ContentType: "text", Data: []byte("c")},
	}})
	want := map[string]string{"2": "application/pdf", "3": "application/octet-stream", "4": "application/octet-stream"}
	for _, a := range m.get(t, false, r.String()).GetItems()[0].GetMessage().GetAttachments() {
		if a.GetContentType() != want[a.GetPart()] {
			t.Errorf("get part %s content type = %q, want %q", a.GetPart(), a.GetContentType(), want[a.GetPart()])
		}
	}
	for part, ct := range want {
		stream, err := m.Client.Message.DownloadAttachment(t.Context(), connect.NewRequest(&chainmailv1.DownloadAttachmentRequest{Ref: r.String(), Part: part}))
		if err != nil {
			t.Fatal(err)
		}
		if !stream.Receive() {
			t.Fatalf("part %s: no header: %v", part, stream.Err())
		}
		if got := stream.Msg().GetHeader().GetContentType(); got != ct {
			t.Errorf("download part %s content type = %q, want %q", part, got, ct)
		}
		for stream.Receive() {
		}
		_ = stream.Close()
	}
}
