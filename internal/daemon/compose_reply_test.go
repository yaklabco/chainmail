package daemon_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/emersion/go-smtp"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
	"github.com/jamesainslie/chainmail/internal/testutil/smtptest"
)

func (e sendEnv) reply(t *testing.T, req *chainmailv1.ReplyRequest) (*chainmailv1.ComposeResult, error) {
	t.Helper()
	resp, err := e.Client.Compose.Reply(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetResult(), nil
}

func (e sendEnv) forward(t *testing.T, req *chainmailv1.ForwardRequest) (*chainmailv1.ComposeResult, error) {
	t.Helper()
	resp, err := e.Client.Compose.Forward(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetResult(), nil
}

// original adds the message replies and forwards answer to the host INBOX:
// from Alice to the account and Bob, copied to Carol, with an attachment.
func (e sendEnv) original(t *testing.T) ref.Ref {
	t.Helper()
	return e.host.Add(fake.Message{
		Folder:     "INBOX",
		Date:       time.Date(2026, 10, 29, 9, 30, 0, 0, time.UTC),
		From:       []mailstore.Address{{Name: "Alice", Email: "alice@example.net"}},
		To:         []mailstore.Address{{Email: "ME@example.com"}, {Name: "Bob", Email: "bob@example.net"}},
		Cc:         []mailstore.Address{{Email: "carol@example.net"}, {Email: "me@example.com"}},
		Subject:    "Plans",
		Body:       "Original line one\r\nOriginal line two\r\n",
		MessageID:  "<orig@example.net>",
		References: []string{"<root@example.net>"},
		Attachments: []fake.Attachment{
			{Part: "2", Filename: "agenda.pdf", ContentType: "application/pdf", Data: []byte("%PDF agenda bytes")},
		},
	})
}

// htmlOriginal is a raw message with Reply-To and an HTML alternative.
const htmlOriginal = "From: Alice <alice@example.net>\r\n" +
	"Reply-To: The List <list@example.net>\r\n" +
	"To: me@example.com\r\n" +
	"Subject: Lunch\r\n" +
	"Date: Thu, 29 Oct 2026 09:30:00 +0000\r\n" +
	"Message-ID: <lunch@example.net>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/alternative; boundary=\"b\"\r\n" +
	"\r\n" +
	"--b\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Plain lunch\r\n" +
	"--b\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>Rich <b>lunch</b></p><script>steal()</script>\r\n" +
	"--b--\r\n"

func replyContent(body string) *chainmailv1.Content {
	return &chainmailv1.Content{Body: markdown(body)}
}

func TestReply(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	orig := e.original(t)
	res, err := e.reply(t, &chainmailv1.ReplyRequest{Ref: orig.String(), Content: replyContent("Sounds good"), IdempotencyKey: "reply-1"})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	sent := res.GetSent()
	if sent == nil || sent.GetMessageId() == "" {
		t.Fatalf("result = %+v", res)
	}
	msgs := srv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if want := []string{"alice@example.net"}; !slices.Equal(m.To, want) {
		t.Errorf("RCPT TO = %v, want %v", m.To, want)
	}
	h := header(t, m.Data)
	if subj, _ := h.Subject(); subj != "Re: Plans" {
		t.Errorf("Subject = %q", subj)
	}
	if to := h.Get("To"); !strings.Contains(to, "alice@example.net") || strings.Contains(to, "bob@") {
		t.Errorf("To = %q", to)
	}
	if h.Has("Cc") {
		t.Errorf("Cc = %q, want none without all", h.Get("Cc"))
	}
	if irt, _ := h.MsgIDList("In-Reply-To"); !slices.Equal(irt, []string{"orig@example.net"}) {
		t.Errorf("In-Reply-To = %v", irt)
	}
	if refs, _ := h.MsgIDList("References"); !slices.Equal(refs, []string{"root@example.net", "orig@example.net"}) {
		t.Errorf("References = %v", refs)
	}
	text := leaves(t, m.Data)["text/plain"]
	if !strings.Contains(text, "Sounds good") || !strings.Contains(text, "> Original line one") ||
		!strings.Contains(text, "Alice <alice@example.net> wrote:") {
		t.Errorf("text part = %q", text)
	}
	if strings.Index(text, "Sounds good") > strings.Index(text, "> Original line one") {
		t.Errorf("the new text is not above the quote: %q", text)
	}
	if !slices.Contains(e.host.Flags(orig), `\Answered`) {
		t.Errorf("original flags = %v, want \\Answered", e.host.Flags(orig))
	}
	if e.idemState(t, "host", "reply-1") != "done" {
		t.Errorf("idempotency state = %q", e.idemState(t, "host", "reply-1"))
	}
	last := lastAudit(t, e.Harness)
	if last.Action != "reply" || last.Account != "host" || last.Result != "ok" ||
		!strings.Contains(last.Targets, orig.String()) || !strings.Contains(last.Targets, sent.GetMessageId()) {
		t.Errorf("audit = %+v", last)
	}
	e.checkAuditPrivate(t, "alice@", "bob@", "carol@", "Plans")
}

func TestReplyAll(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	orig := e.original(t)
	req := &chainmailv1.ReplyRequest{
		Ref: orig.String(), All: true, Content: replyContent("All of you"),
		Cc: []string{"Dan <dan@example.net>", "bob@example.net"}, Bcc: []string{"hidden@example.net"},
		IdempotencyKey: "reply-all",
	}
	if _, err := e.reply(t, req); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	m := srv.Messages()[0]
	want := []string{"alice@example.net", "bob@example.net", "carol@example.net", "dan@example.net", "hidden@example.net"}
	if !slices.Equal(m.To, want) {
		t.Errorf("RCPT TO = %v, want %v", m.To, want)
	}
	h := header(t, m.Data)
	cc := h.Get("Cc")
	if !strings.Contains(cc, "bob@example.net") || !strings.Contains(cc, "carol@example.net") || !strings.Contains(cc, "dan@example.net") {
		t.Errorf("Cc = %q", cc)
	}
	if strings.Count(cc, "bob@example.net") != 1 {
		t.Errorf("Cc = %q, want bob once", cc)
	}
	if strings.Contains(strings.ToLower(h.Get("To")+cc), "me@example.com") {
		t.Errorf("the account's own address is a recipient: To %q Cc %q", h.Get("To"), cc)
	}
	if bytes.Contains(m.Data, []byte("hidden@")) {
		t.Error("the Bcc recipient is in the message bytes")
	}
}

func TestReplyHonoursReplyToAndQuotesHTML(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	orig, err := e.host.Append(context.Background(), "INBOX", nil, time.Time{}, []byte(htmlOriginal))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.reply(t, &chainmailv1.ReplyRequest{Ref: orig.String(), Content: replyContent("Yes"), IdempotencyKey: "reply-to"}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	m := srv.Messages()[0]
	if want := []string{"list@example.net"}; !slices.Equal(m.To, want) {
		t.Errorf("RCPT TO = %v, want %v", m.To, want)
	}
	parts := leaves(t, m.Data)
	if !strings.Contains(parts["text/plain"], "> Plain lunch") {
		t.Errorf("text part = %q", parts["text/plain"])
	}
	htm := parts["text/html"]
	if !strings.Contains(htm, `<blockquote type="cite"`) || !strings.Contains(htm, "Rich <b>lunch</b>") {
		t.Errorf("html part = %q", htm)
	}
	if strings.Contains(htm, "steal()") {
		t.Errorf("html part kept the original's script: %q", htm)
	}
}

func TestReplyFailureLeavesOriginalUnanswered(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{FailRcpt: map[string]*smtp.SMTPError{
		"alice@example.net": {Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such user here"},
	}})
	orig := e.original(t)
	_, err := e.reply(t, &chainmailv1.ReplyRequest{Ref: orig.String(), Content: replyContent("Hi"), IdempotencyKey: "reply-fail"})
	if reasonOf(err) != api.ReasonInvalidArgument {
		t.Fatalf("err = %v, want INVALID_ARGUMENT", err)
	}
	if !strings.Contains(err.Error(), "alice@example.net") || strings.Contains(err.Error(), "Alice") || strings.Contains(err.Error(), "no such user") {
		t.Errorf("err = %v, want the refused address only", err)
	}
	if slices.Contains(e.host.Flags(orig), `\Answered`) {
		t.Error("a failed reply marked the original answered")
	}
	if len(srv.Messages()) != 0 || len(e.host.Appended()) != 0 {
		t.Error("a failed reply was sent or stored")
	}
	if last := lastAudit(t, e.Harness); last.Action != "reply" || last.Result != "error" {
		t.Errorf("audit = %+v", last)
	}
}

func TestReplyDraftAndDryRun(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	orig := e.original(t)

	res, err := e.reply(t, &chainmailv1.ReplyRequest{Ref: orig.String(), Content: replyContent("Draft reply"), Draft: true})
	if err != nil {
		t.Fatalf("Reply draft: %v", err)
	}
	d := res.GetDraft()
	if d == nil {
		t.Fatalf("result = %+v", res)
	}
	r := parseRef(t, d.GetRef())
	if r.Folder != "Drafts" || !slices.Contains(e.host.Flags(r), `\Draft`) || !slices.Contains(e.host.Flags(r), `\Seen`) {
		t.Errorf("draft %v flags %v", r, e.host.Flags(r))
	}
	h := header(t, e.host.RawMessage(r))
	if irt, _ := h.MsgIDList("In-Reply-To"); !slices.Equal(irt, []string{"orig@example.net"}) {
		t.Errorf("draft In-Reply-To = %v", irt)
	}
	if subj, _ := h.Subject(); subj != "Re: Plans" {
		t.Errorf("draft Subject = %q", subj)
	}
	if slices.Contains(e.host.Flags(orig), `\Answered`) {
		t.Error("a reply draft marked the original answered")
	}
	if last := lastAudit(t, e.Harness); last.Action != "draft.create" || last.Result != "ok" || !strings.Contains(last.Targets, orig.String()) {
		t.Errorf("audit = %+v", last)
	}

	res, err = e.reply(t, &chainmailv1.ReplyRequest{Ref: orig.String(), Content: replyContent("Dry"), DryRun: true})
	if err != nil {
		t.Fatalf("Reply dry run: %v", err)
	}
	if dry := res.GetDryRun(); dry == nil || !slices.Equal(dry.GetEnvelopeTo(), []string{"alice@example.net"}) {
		t.Errorf("dry run = %+v", res)
	}
	if n := e.proxy.accepts.Load(); n != 0 || len(srv.Messages()) != 0 {
		t.Errorf("connections = %d, messages = %d, want none", n, len(srv.Messages()))
	}
	if _, err := e.reply(t, &chainmailv1.ReplyRequest{Ref: orig.String(), Content: replyContent("x"), DryRun: true, Draft: true}); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("dry run and draft: err = %v, want INVALID_ARGUMENT", err)
	}
	e.checkAuditPrivate(t, "alice@", "Plans")
}

func TestReplyRefusals(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	e.smtp(t, smtptest.Options{})
	orig := e.original(t)
	gone := orig
	gone.UID += 100
	tests := []struct {
		name   string
		req    *chainmailv1.ReplyRequest
		reason string
	}{
		{"no key", &chainmailv1.ReplyRequest{Ref: orig.String(), Content: replyContent("x")}, api.ReasonInvalidArgument},
		{"bad ref", &chainmailv1.ReplyRequest{Ref: "not a ref", Content: replyContent("x"), IdempotencyKey: "k1"}, api.ReasonInvalidArgument},
		{"bad extra recipient", &chainmailv1.ReplyRequest{Ref: orig.String(), To: []string{"nope"}, Content: replyContent("x"), IdempotencyKey: "k2"}, api.ReasonInvalidArgument},
		{"gone", &chainmailv1.ReplyRequest{Ref: gone.String(), Content: replyContent("x"), IdempotencyKey: "k3"}, api.ReasonStaleRef},
	}
	for _, tt := range tests {
		if _, err := e.reply(t, tt.req); reasonOf(err) != tt.reason {
			t.Errorf("%s: err = %v, want %s", tt.name, err, tt.reason)
		}
	}

	// A message the account sent to itself alone has nobody to reply to.
	self := e.host.Add(fake.Message{
		Folder: "INBOX", From: []mailstore.Address{{Email: "me@example.com"}}, To: []mailstore.Address{{Email: "me@example.com"}},
		Subject: "note", Body: "x", MessageID: "<self@example.com>",
	})
	if _, err := e.reply(t, &chainmailv1.ReplyRequest{Ref: self.String(), Content: replyContent("x"), IdempotencyKey: "k4"}); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("reply to self: err = %v, want INVALID_ARGUMENT", err)
	}
}

func TestForward(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	orig := e.original(t)
	res, err := e.forward(t, &chainmailv1.ForwardRequest{
		Ref: orig.String(), To: []string{"Zed <zed@example.org>"}, Content: replyContent("FYI"), IdempotencyKey: "fwd-1",
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	m := srv.Messages()[0]
	if want := []string{"zed@example.org"}; !slices.Equal(m.To, want) {
		t.Errorf("RCPT TO = %v, want %v", m.To, want)
	}
	h := header(t, m.Data)
	if subj, _ := h.Subject(); subj != "Fwd: Plans" {
		t.Errorf("Subject = %q", subj)
	}
	if h.Has("In-Reply-To") || h.Has("References") {
		t.Errorf("a forward has In-Reply-To %q References %q", h.Get("In-Reply-To"), h.Get("References"))
	}
	parts := leaves(t, m.Data)
	text := parts["text/plain"]
	if !strings.Contains(text, "FYI") || !strings.Contains(text, "---------- Forwarded message ---------") ||
		!strings.Contains(text, "Original line one") || strings.Contains(text, "> Original line one") {
		t.Errorf("text part = %q", text)
	}
	if parts["application/pdf"] != "%PDF agenda bytes" {
		t.Errorf("attachment = %q, want the original's", parts["application/pdf"])
	}
	if !bytes.Contains(m.Data, []byte("agenda.pdf")) {
		t.Error("the attachment lost its filename")
	}
	if !slices.Contains(e.host.Flags(orig), "$Forwarded") {
		t.Errorf("original flags = %v, want $Forwarded", e.host.Flags(orig))
	}
	if last := lastAudit(t, e.Harness); last.Action != "forward" || last.Result != "ok" ||
		!strings.Contains(last.Targets, orig.String()) || !strings.Contains(last.Targets, res.GetSent().GetMessageId()) {
		t.Errorf("audit = %+v", last)
	}
	e.checkAuditPrivate(t, "zed@", "agenda", "Plans")
}

func TestForwardWithoutAttachments(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	orig := e.original(t)
	if _, err := e.forward(t, &chainmailv1.ForwardRequest{
		Ref: orig.String(), To: []string{"zed@example.org"}, Content: replyContent("FYI"), NoAttachments: true, IdempotencyKey: "fwd-2",
	}); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	m := srv.Messages()[0]
	if _, ok := leaves(t, m.Data)["application/pdf"]; ok || bytes.Contains(m.Data, []byte("agenda.pdf")) {
		t.Error("no_attachments still attached the original's file")
	}
	if n := e.host.Calls("StreamPart"); n != 0 {
		t.Errorf("streamed %d parts, want none", n)
	}
}

func TestForwardRefusals(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	orig := e.original(t)
	if _, err := e.forward(t, &chainmailv1.ForwardRequest{Ref: orig.String(), Content: replyContent("x"), IdempotencyKey: "f1"}); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("no recipients: err = %v, want INVALID_ARGUMENT", err)
	}
	// An original whose attachments are over the account's limit is refused
	// before anything is sent.
	big := e.small.Add(fake.Message{
		Folder: "INBOX", From: []mailstore.Address{{Email: "alice@example.net"}}, Subject: "big", Body: "x", MessageID: "<big@example.net>",
		Attachments: []fake.Attachment{{Part: "2", Filename: "big.bin", ContentType: "application/octet-stream", Data: bytes.Repeat([]byte("b"), 32<<10)}},
	})
	_, err := e.forward(t, &chainmailv1.ForwardRequest{Ref: big.String(), To: []string{"zed@example.org"}, Content: replyContent("x"), IdempotencyKey: "f2"})
	if reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("oversized: err = %v, want INVALID_ARGUMENT", err)
	}
	if len(srv.Messages()) != 0 {
		t.Error("a refused forward was sent")
	}
}

func TestForwardFlagFailureDoesNotFailTheSend(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	orig := e.original(t)
	e.host.FailWrite("StoreFlags", fmt.Errorf("%w: private server text", mailstore.ErrUnavailable))
	res, err := e.forward(t, &chainmailv1.ForwardRequest{Ref: orig.String(), To: []string{"zed@example.org"}, Content: replyContent("x"), IdempotencyKey: "f3"})
	if err != nil || res.GetSent() == nil {
		t.Fatalf("Forward: %v %+v", err, res)
	}
	if len(srv.Messages()) != 1 {
		t.Error("the forward was not sent")
	}
	if slices.Contains(e.host.Flags(orig), "$Forwarded") {
		t.Error("the failed flag store still set $Forwarded")
	}
}

func TestForwardDraft(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	e.gmail.AddFolder("[Gmail]/Drafts", mailstore.RoleDrafts)
	srv := e.smtp(t, smtptest.Options{})
	orig := e.gmail.Add(fake.Message{
		Folder: "[Gmail]/All Mail", From: []mailstore.Address{{Email: "alice@example.net"}}, Subject: "Report", Body: "see attached",
		MessageID: "<report@example.net>", Labels: []string{`\Inbox`},
		Attachments: []fake.Attachment{{Part: "2", Filename: "r.csv", ContentType: "text/csv", Data: []byte("a,b\n1,2\n")}},
	})
	res, err := e.forward(t, &chainmailv1.ForwardRequest{Ref: orig.String(), To: []string{"zed@example.org"}, Content: replyContent("fwd"), Draft: true})
	if err != nil {
		t.Fatalf("Forward draft: %v", err)
	}
	r := parseRef(t, res.GetDraft().GetRef())
	raw := e.gmail.RawMessage(r)
	if subj, _ := header(t, raw).Subject(); subj != "Fwd: Report" {
		t.Errorf("draft Subject = %q", subj)
	}
	if !strings.Contains(leaves(t, raw)["text/csv"], "1,2") {
		t.Error("the draft lost the original's attachment")
	}
	if len(srv.Messages()) != 0 || slices.Contains(e.gmail.Flags(orig), "$Forwarded") {
		t.Error("a forward draft was sent or marked the original")
	}
}

// TestReplyAndForwardReplayAfterUploadExpires repeats settled replies and
// forwards whose uploaded attachment expired. Each stored outcome comes
// back without a connection.
func TestReplyAndForwardReplayAfterUploadExpires(t *testing.T) {
	t.Parallel()
	clock := &settableClock{now: time.Now()}
	e := startSend(t, func(o *daemontest.Options) { o.Now = clock.Now })
	e.smtp(t, smtptest.Options{})
	orig := e.original(t)
	up, err := e.upload(t, &chainmailv1.UploadHeader{Account: "host", Filename: "a.txt", ContentType: "text/plain"}, []byte("attached"))
	if err != nil {
		t.Fatal(err)
	}
	content := func() *chainmailv1.Content {
		return &chainmailv1.Content{Body: markdown("see attached"), Attachments: []string{up.GetHandle()}}
	}
	reply := &chainmailv1.ReplyRequest{Ref: orig.String(), Content: content(), IdempotencyKey: "r-done"}
	forward := &chainmailv1.ForwardRequest{Ref: orig.String(), To: []string{"bob@example.net"}, Content: content(), IdempotencyKey: "f-done"}
	firstReply, err := e.reply(t, reply)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	firstForward, err := e.forward(t, forward)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	e.smtp(t, smtptest.Options{DropAfterDataEnd: true})
	unknown := &chainmailv1.ReplyRequest{Ref: orig.String(), Content: content(), IdempotencyKey: "r-unknown"}
	if _, err := e.reply(t, unknown); reasonOf(err) != api.ReasonSendOutcomeUnknown {
		t.Fatalf("lost reply: err = %v, want SEND_OUTCOME_UNKNOWN", err)
	}

	clock.Add(2 * time.Hour)
	conns := e.proxy.accepts.Load()
	if res, err := e.reply(t, reply); err != nil || !res.GetSent().GetDuplicate() || res.GetSent().GetMessageId() != firstReply.GetSent().GetMessageId() {
		t.Errorf("reply after expiry: %+v, %v, want the first result as a duplicate", res, err)
	}
	if res, err := e.forward(t, forward); err != nil || !res.GetSent().GetDuplicate() || res.GetSent().GetMessageId() != firstForward.GetSent().GetMessageId() {
		t.Errorf("forward after expiry: %+v, %v, want the first result as a duplicate", res, err)
	}
	if _, err := e.reply(t, unknown); reasonOf(err) != api.ReasonSendOutcomeUnknown {
		t.Errorf("unknown after expiry: err = %v, want SEND_OUTCOME_UNKNOWN", err)
	}
	if e.proxy.accepts.Load() != conns {
		t.Error("a repeat after expiry connected")
	}
	for action, want := range map[string]int{"reply": 2, "forward": 1} {
		d := e.decisions(t, action)
		if n := len(d); n < want || slices.ContainsFunc(d[n-want:], func(s string) bool { return s != "duplicate" }) {
			t.Errorf("%s decisions = %v, want the repeats audited as duplicates", action, d)
		}
	}
}

// clipText makes the fake's FetchMessages report every text part clipped,
// keeping keep bytes of the body, as a store does at TextPartCap.
func clipText(h *hookStore, keep int) {
	h.fetch = func(ctx context.Context, refs []ref.Ref) []mailstore.MessageResult {
		res := h.Store.FetchMessages(ctx, refs)
		for _, r := range res {
			if m := r.Message; m != nil {
				if i := bytes.Index(m.Raw, []byte("\r\n\r\n")); i >= 0 && len(m.Raw) > i+4+keep {
					m.Raw = m.Raw[:i+4+keep]
				}
				m.TextClipped = true
			}
		}
		return res
	}
}

// TestReplyFailsWhenTheOriginalCannotBeRead clips the original's text, so
// the whole message must be streamed, and fails that stream. The reply and
// the forward fail instead of quoting part of the original, and send
// nothing.
func TestReplyFailsWhenTheOriginalCannotBeRead(t *testing.T) {
	t.Parallel()
	hs := &hookStore{}
	e := startSend(t, hooked("host", hs))
	srv := e.smtp(t, smtptest.Options{})
	clipText(hs, 1<<20)
	hs.stream = func(ctx context.Context, r ref.Ref, part string) (mailstore.PartInfo, io.ReadCloser, error) {
		if part == "" {
			return mailstore.PartInfo{}, nil, fmt.Errorf("%w: the connection dropped", mailstore.ErrUnavailable)
		}
		return hs.Store.StreamPart(ctx, r, part)
	}
	orig := e.original(t)
	if _, err := e.reply(t, &chainmailv1.ReplyRequest{Ref: orig.String(), Content: replyContent("hi"), IdempotencyKey: "r"}); reasonOf(err) != api.ReasonServerUnavailable {
		t.Errorf("Reply: err = %v, want SERVER_UNAVAILABLE", err)
	}
	if _, err := e.forward(t, &chainmailv1.ForwardRequest{Ref: orig.String(), To: []string{"bob@example.net"}, IdempotencyKey: "f"}); reasonOf(err) != api.ReasonServerUnavailable {
		t.Errorf("Forward: err = %v, want SERVER_UNAVAILABLE", err)
	}
	if n := len(srv.Messages()); n != 0 {
		t.Errorf("captured %d messages, want none", n)
	}
	if slices.Contains(e.host.Flags(orig), `\Answered`) {
		t.Error("the original was marked answered")
	}
	for _, key := range []string{"r", "f"} {
		if got := e.idemState(t, "host", key); got != "retryable" {
			t.Errorf("%s: idempotency state = %q, want retryable", key, got)
		}
	}
}

// truncatedMarker ends a quote whose original text was cut short.
const truncatedMarker = "[quoted text truncated]"

// TestReplyMarksATruncatedQuote replies to an original whose text the store
// clipped and whose whole message is over the account's limit, so only the
// clipped text can be quoted. The quote says so in both parts.
func TestReplyMarksATruncatedQuote(t *testing.T) {
	t.Parallel()
	hs := &hookStore{}
	e := startSend(t, hooked("small", hs))
	srv := e.smtp(t, smtptest.Options{})
	clipText(hs, 1<<10)
	orig := e.small.Add(fake.Message{
		Folder:    "INBOX",
		Date:      time.Date(2026, 10, 29, 9, 30, 0, 0, time.UTC),
		From:      []mailstore.Address{{Name: "Alice", Email: "alice@example.net"}},
		To:        []mailstore.Address{{Email: "small@example.com"}},
		Subject:   "Long",
		Body:      strings.Repeat("a long original line\r\n", 1000),
		MessageID: "<long@example.net>",
	})
	if _, err := e.reply(t, &chainmailv1.ReplyRequest{Ref: orig.String(), Content: replyContent("short answer"), IdempotencyKey: "r"}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	msgs := srv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages, want 1", len(msgs))
	}
	parts := leaves(t, msgs[0].Data)
	text := strings.TrimRight(parts["text/plain"], "\r\n")
	if last := text[strings.LastIndexByte(text, '\n')+1:]; strings.TrimLeft(last, "> ") != truncatedMarker {
		t.Errorf("the text part ends %q, want %q", last, truncatedMarker)
	}
	if !strings.Contains(text, "> a long original line") {
		t.Errorf("text part = %q, want the clipped quote", text)
	}
	htm := parts["text/html"]
	if i := strings.LastIndex(htm, truncatedMarker); i < 0 || strings.Contains(htm[i:], "a long original line") {
		t.Errorf("the HTML part does not end its quote with %q: %q", truncatedMarker, htm)
	}
}

// TestReplyExtraRecipientsAreNotRepeated adds extra recipients a reply
// already has: each address shows once across To and Cc, To first.
func TestReplyExtraRecipientsAreNotRepeated(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	orig := e.original(t)
	if _, err := e.reply(t, &chainmailv1.ReplyRequest{Ref: orig.String(), Content: replyContent("one"), Cc: []string{"ALICE@example.net"}, IdempotencyKey: "a"}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if _, err := e.reply(t, &chainmailv1.ReplyRequest{Ref: orig.String(), All: true, Content: replyContent("two"), To: []string{"bob@example.net"}, IdempotencyKey: "b"}); err != nil {
		t.Fatalf("Reply all: %v", err)
	}
	msgs := srv.Messages()
	if len(msgs) != 2 {
		t.Fatalf("captured %d messages, want 2", len(msgs))
	}
	if h := header(t, msgs[0].Data); strings.Contains(strings.ToLower(h.Get("Cc")), "alice@") {
		t.Errorf("reply: To %q, Cc %q, want alice only in To", h.Get("To"), h.Get("Cc"))
	}
	h := header(t, msgs[1].Data)
	if !strings.Contains(h.Get("To"), "bob@example.net") || strings.Contains(h.Get("Cc"), "bob@") {
		t.Errorf("reply all: To %q, Cc %q, want bob only in To", h.Get("To"), h.Get("Cc"))
	}
}
