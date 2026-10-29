package daemon

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/query"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/render"
)

func summaryAt(account, folder string, uid uint32, at time.Time, thread string) mailstore.Summary {
	return mailstore.Summary{Ref: ref.Ref{Account: account, Folder: folder, UIDValidity: 1, UID: uid}, InternalDate: at, ThreadID: thread}
}

func TestMergeTakesTopUIDRunsPerSource(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	a := &sourceState{account: "a", src: mailstore.Source{Folder: "INBOX", UIDValidity: 1}, uids: []uint32{9, 8, 7}, requested: 3}
	b := &sourceState{account: "b", src: mailstore.Source{Folder: "INBOX", UIDValidity: 1}, uids: []uint32{5, 4}, requested: 2}
	a.rows = []mailstore.Summary{
		summaryAt("a", "INBOX", 9, base.Add(5*time.Hour), "t1"),
		// Older than the next row but a higher UID: the merge must still take it first.
		summaryAt("a", "INBOX", 8, base.Add(1*time.Hour), "t2"),
		summaryAt("a", "INBOX", 7, base.Add(2*time.Hour), "t3"),
	}
	b.rows = []mailstore.Summary{
		summaryAt("b", "INBOX", 5, base.Add(4*time.Hour), "t4"),
		summaryAt("b", "INBOX", 4, base.Add(3*time.Hour), "t5"),
	}
	rows := merge([]*sourceState{a, b}, 3, false)
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%s%d", r.sum.Ref.Account, r.sum.Ref.UID))
	}
	if !slices.Equal(got, []string{"a9", "b5", "b4"}) {
		t.Errorf("merge = %v", got)
	}
	if a.next != 1 || a.lowest != 9 || b.next != 2 || b.lowest != 4 {
		t.Errorf("consumed a %d (lowest %d), b %d (lowest %d)", a.next, a.lowest, b.next, b.lowest)
	}
	p := &searchPlan{fingerprint: ref.Fingerprint("q")}
	c, err := ref.DecodeCursor(nextCursor(p, []*sourceState{a, b}, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := []ref.CursorSource{
		{Account: "a", Folder: "INBOX", UIDValidity: 1, Below: 9},
		{Account: "b", Folder: "INBOX", UIDValidity: 1, Below: 4, Done: true},
	}
	if !slices.Equal(c.Sources, want) {
		t.Errorf("cursor sources = %+v, want %+v", c.Sources, want)
	}
}

func TestMergeThreads(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s := &sourceState{account: "a", uids: []uint32{4, 3, 2, 1}, requested: 4}
	s.rows = []mailstore.Summary{
		summaryAt("a", "INBOX", 4, base.Add(4*time.Hour), "x"),
		summaryAt("a", "INBOX", 3, base.Add(3*time.Hour), "y"),
		summaryAt("a", "INBOX", 2, base.Add(2*time.Hour), "x"),
		summaryAt("a", "INBOX", 1, base.Add(1*time.Hour), "z"),
	}
	rows := merge([]*sourceState{s}, 2, true)
	if len(rows) != 2 || rows[0].sum.Ref.UID != 4 || rows[0].count != 2 || rows[1].sum.Ref.UID != 3 || s.next != 3 {
		t.Errorf("thread merge = %+v, consumed %d", rows, s.next)
	}
}

func TestNextCursorEndsWhenEverySourceIsDone(t *testing.T) {
	t.Parallel()
	s := &sourceState{
		account: "a", src: mailstore.Source{Folder: "INBOX", UIDValidity: 1}, uids: []uint32{2}, requested: 1,
		rows: []mailstore.Summary{summaryAt("a", "INBOX", 2, time.Now(), "")},
	}
	merge([]*sourceState{s}, 10, false)
	if got := nextCursor(&searchPlan{fingerprint: ref.Fingerprint("q")}, []*sourceState{s}, nil); got != "" {
		t.Errorf("cursor = %q, want none", got)
	}
}

func TestMostSevere(t *testing.T) {
	t.Parallel()
	errs := []error{
		fmt.Errorf("%w: x", mailstore.ErrUnavailable),
		fmt.Errorf("%w: y", mailstore.ErrUnsupportedQuery),
		&auth.RequiredError{Account: "a", Hint: "h"},
		fmt.Errorf("%w: z", mailstore.ErrStaleRef),
	}
	var required *auth.RequiredError
	if got := mostSevere(errs); !errors.As(got, &required) {
		t.Errorf("mostSevere = %v, want the AUTH_REQUIRED error", got)
	}
}

func TestQueryProtoRoundTrip(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"invoice", "a b OR c -d", `from:"Alex Doe" (x OR -y) in:inbox`, "-(a b)"} {
		n, err := query.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		back, err := queryFromProto(queryToProto(n), 1)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if !back.Equal(n) {
			t.Errorf("%q round trip = %s", s, back)
		}
	}
}

func TestWithDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := withDeadline(t.Context(), callTimeout)
	defer cancel()
	if d, ok := ctx.Deadline(); !ok || time.Until(d) > callTimeout || time.Until(d) < callTimeout-time.Minute {
		t.Errorf("default deadline = %v, %v, want about %v", d, ok, callTimeout)
	}
	clientCtx, clientCancel := context.WithTimeout(t.Context(), time.Hour)
	defer clientCancel()
	want, _ := clientCtx.Deadline()
	ctx, cancel = withDeadline(clientCtx, callTimeout)
	defer cancel()
	if d, _ := ctx.Deadline(); !d.Equal(want) {
		t.Errorf("deadline = %v, want the client's %v", d, want)
	}
}

func TestSpecLimits(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct{ got, want int64 }{
		"default search limit": {defaultSearchLimit, 50},
		"max search limit":     {maxSearchLimit, 500},
		"max get refs":         {maxGetRefs, 50},
		"call timeout":         {int64(callTimeout), int64(30 * time.Second)},
		"stream timeout":       {int64(streamTimeout), int64(10 * time.Minute)},
		"max response bytes":   {maxResponseBytes, 16 << 20},
		"chunk bytes":          {chunkBytes, 64 << 10},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %d, want %d", name, tt.got, tt.want)
		}
	}
}

func hostileMessage() *mailstore.Message {
	return &mailstore.Message{
		Summary: mailstore.Summary{
			Ref:     ref.Ref{Account: "a", Folder: "INBOX", UIDValidity: 1, UID: 2},
			Subject: "hi\r\n\tthere\u200b",
			From:    []mailstore.Address{{Name: "Al\nex", Email: "a@x.example"}},
		},
		Cc:          []mailstore.Address{{Name: "C\rc", Email: "c@x.example"}},
		MessageID:   "<m@x>\n",
		InReplyTo:   []string{"<p@x>\n"},
		References:  []string{"<r@x>", "<p@x>\t"},
		TextClipped: true,
		Attachments: []mailstore.Attachment{{Part: "2", Filename: "a\nb.pdf", ContentType: "application/pdf", Size: 9}},
	}
}

func TestMessageProtoWrapsTextAndLeavesTheMessageAlone(t *testing.T) {
	t.Parallel()
	msg := hostileMessage()
	body := render.Body{Text: "hello\u200b there", QuotedTrimmed: true, TrimmedBytes: 12}
	out := messageProto(msg, body)
	if !reflect.DeepEqual(msg, hostileMessage()) {
		t.Errorf("messageProto changed its input: %+v", msg)
	}
	headers := []string{
		out.GetSubject().GetUntrusted(), out.GetFrom()[0].GetName().GetUntrusted(), out.GetCc()[0].GetName().GetUntrusted(),
		out.GetMessageId().GetUntrusted(), out.GetAttachments()[0].GetFilename().GetUntrusted(),
	}
	for _, u := range append(out.GetInReplyTo(), out.GetReferences()...) {
		headers = append(headers, u.GetUntrusted())
	}
	for _, h := range headers {
		if h == "" || h != render.CleanHeader(h) {
			t.Errorf("header %q is not one clean line", h)
		}
	}
	if len(out.GetReferences()) != 2 || out.GetRef() != msg.Summary.Ref.String() {
		t.Errorf("references %v, ref %q", out.GetReferences(), out.GetRef())
	}
	b := out.GetBody()
	if b.GetUntrusted() != render.CleanText(body.Text) || !b.GetTruncated() || !b.GetQuotedTrimmed() || b.GetTrimmedBytes() != 12 {
		t.Errorf("body = %v", b)
	}
}

func TestFitPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		base          int
		kept, dropped []int
		limit, want   int
	}{
		{"all fit", 10, []int{20, 20}, []int{1, 1}, 100, 2},
		{"exactly at the limit", 10, []int{20, 20}, []int{1, 1}, 50, 2},
		{"the second crosses", 10, []int{20, 30}, []int{1, 1}, 50, 1},
		{"a later small one is cut too", 10, []int{20, 30, 1}, []int{1, 1, 1}, 50, 1},
		{"the first crosses", 10, []int{60, 1}, []int{1, 1}, 50, 0},
		{"stand-ins count", 10, []int{38}, []int{5}, 50, 1},
		{"nothing", 10, nil, nil, 50, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := fitPrefix(tt.base, tt.kept, tt.dropped, tt.limit); got != tt.want {
				t.Errorf("fitPrefix = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMediaType(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"application/pdf":             "application/pdf",
		`Text/HTML; charset="utf-8"`:  "text/html",
		"image/png; name=a b":         "image/png",
		"text":                        "application/octet-stream",
		"":                            "application/octet-stream",
		"text/":                       "application/octet-stream",
		"te xt/plain":                 "application/octet-stream",
		"text/plain\x1b[31m":          "application/octet-stream",
		"application/vnd.ms-excel":    "application/vnd.ms-excel",
		"multipart/mixed; boundary=x": "multipart/mixed",
	} {
		if got := mediaType(in); got != want {
			t.Errorf("mediaType(%q) = %q, want %q", in, got, want)
		}
	}
}
