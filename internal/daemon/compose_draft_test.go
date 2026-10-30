package daemon_test

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/smtptest"
)

func (e sendEnv) createDraft(t *testing.T, spec *chainmailv1.ComposeSpec) *chainmailv1.Draft {
	t.Helper()
	resp, err := e.Client.Compose.CreateDraft(t.Context(), connect.NewRequest(&chainmailv1.CreateDraftRequest{Spec: spec}))
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	return resp.Msg.GetDraft()
}

func (e sendEnv) updateDraft(t *testing.T, r string, spec *chainmailv1.ComposeSpec) (*chainmailv1.Draft, error) {
	t.Helper()
	resp, err := e.Client.Compose.UpdateDraft(t.Context(), connect.NewRequest(&chainmailv1.UpdateDraftRequest{Ref: r, Spec: spec}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetDraft(), nil
}

func (e sendEnv) sendDraft(t *testing.T, r, key string) (*chainmailv1.ComposeResult, error) {
	t.Helper()
	resp, err := e.Client.Compose.SendDraft(t.Context(), connect.NewRequest(&chainmailv1.SendDraftRequest{Ref: r, IdempotencyKey: key}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetResult(), nil
}

func (e sendEnv) deleteDraft(t *testing.T, r string) error {
	t.Helper()
	_, err := e.Client.Compose.DeleteDraft(t.Context(), connect.NewRequest(&chainmailv1.DeleteDraftRequest{Ref: r}))
	return err
}

// gone reports whether r no longer resolves in st.
func gone(t *testing.T, st *fake.Store, r ref.Ref) bool {
	t.Helper()
	res := st.FetchMessages(context.Background(), []ref.Ref{r})
	return res[0].Err != nil
}

func TestCreateDraft(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	d := e.createDraft(t, hostSpec())
	r := parseRef(t, d.GetRef())
	if r.Folder != "Drafts" {
		t.Errorf("draft folder = %q", r.Folder)
	}
	if f := e.host.Flags(r); !slices.Contains(f, `\Draft`) || !slices.Contains(f, `\Seen`) {
		t.Errorf("draft flags = %v", f)
	}
	h := header(t, e.host.RawMessage(r))
	if id, _ := h.MessageID(); id != d.GetMessageId() {
		t.Errorf("Message-ID = %q, want %q", id, d.GetMessageId())
	}
	// A draft keeps its Bcc, as mail clients' drafts do, so sending it
	// later still reaches the blind-copied recipient.
	if !strings.Contains(h.Get("Bcc"), "hidden@example.net") {
		t.Errorf("draft Bcc = %q", h.Get("Bcc"))
	}
	if n := e.proxy.accepts.Load(); n != 0 || len(srv.Messages()) != 0 {
		t.Errorf("connections = %d, messages = %d, want none", n, len(srv.Messages()))
	}
	if last := lastAudit(t, e.Harness); last.Action != "draft.create" || last.Result != "ok" ||
		!strings.Contains(last.Targets, d.GetRef()) || !strings.Contains(last.Targets, d.GetMessageId()) {
		t.Errorf("audit = %+v", last)
	}
	e.checkAuditPrivate(t)
}

func TestUpdateDraft(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	old := e.createDraft(t, hostSpec())
	spec := hostSpec()
	spec.Subject = "Quarterly numbers, revised"
	spec.Content.Body = markdown("Second version")
	d, err := e.updateDraft(t, old.GetRef(), spec)
	if err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}
	oldRef, newRef := parseRef(t, old.GetRef()), parseRef(t, d.GetRef())
	if !gone(t, e.host, oldRef) {
		t.Error("the old version is still there")
	}
	if newRef.Folder != "Drafts" || oldRef == newRef {
		t.Errorf("new ref = %v, old %v", newRef, oldRef)
	}
	raw := e.host.RawMessage(newRef)
	if !strings.Contains(leaves(t, raw)["text/plain"], "Second version") {
		t.Errorf("new draft = %s", raw)
	}
	if f := e.host.Flags(newRef); !slices.Contains(f, `\Draft`) {
		t.Errorf("new draft flags = %v", f)
	}
	last := lastAudit(t, e.Harness)
	if last.Action != "draft.update" || last.Result != "ok" ||
		!strings.Contains(last.Targets, old.GetRef()) || !strings.Contains(last.Targets, d.GetRef()) {
		t.Errorf("audit = %+v", last)
	}
	e.checkAuditPrivate(t)
}

func TestUpdateDraftKeepsReplyHeaders(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	orig := e.original(t)
	res, err := e.reply(t, &chainmailv1.ReplyRequest{Ref: orig.String(), Content: replyContent("first"), Draft: true})
	if err != nil {
		t.Fatalf("Reply draft: %v", err)
	}
	spec := &chainmailv1.ComposeSpec{Account: "host", To: []string{"alice@example.net"}, Subject: "Re: Plans", Content: replyContent("second")}
	d, err := e.updateDraft(t, res.GetDraft().GetRef(), spec)
	if err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}
	h := header(t, e.host.RawMessage(parseRef(t, d.GetRef())))
	if irt, _ := h.MsgIDList("In-Reply-To"); !slices.Equal(irt, []string{"orig@example.net"}) {
		t.Errorf("In-Reply-To = %v", irt)
	}
	if refs, _ := h.MsgIDList("References"); !slices.Equal(refs, []string{"root@example.net", "orig@example.net"}) {
		t.Errorf("References = %v", refs)
	}
}

func TestSendDraftByRef(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	// A draft saved a while ago: sending it dates it now.
	raw := []byte("From: Me Myself <me@example.com>\r\n" +
		"To: you@example.net\r\n" +
		"Bcc: hidden@example.net\r\n" +
		"Subject: Quarterly draft\r\n" +
		"Date: Mon, 05 Jan 2015 10:00:00 +0000\r\n" +
		"Message-ID: <draft.1@example.com>\r\n" +
		"\r\n" + "draft body\r\n")
	dr, err := e.host.Append(context.Background(), "Drafts", []string{`\Draft`, `\Seen`}, time.Time{}, raw)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.sendDraft(t, dr.String(), "draft-send")
	if err != nil {
		t.Fatalf("SendDraft: %v", err)
	}
	sent := res.GetSent()
	if sent.GetMessageId() != "draft.1@example.com" {
		t.Errorf("result = %+v, want the draft's Message-ID", sent)
	}
	m := srv.Messages()[0]
	if want := []string{"you@example.net", "hidden@example.net"}; !slices.Equal(m.To, want) {
		t.Errorf("RCPT TO = %v, want %v", m.To, want)
	}
	h := header(t, m.Data)
	if id, _ := h.MessageID(); id != "draft.1@example.com" {
		t.Errorf("Message-ID = %q", id)
	}
	if date, _ := h.Date(); date.Year() < 2026 {
		t.Errorf("Date = %v, want it refreshed", date)
	}
	if h.Has("Bcc") || strings.Contains(string(m.Data), "hidden@") {
		t.Error("the Bcc header was sent")
	}
	if !gone(t, e.host, dr) {
		t.Error("the draft is still in Drafts")
	}
	if r := parseRef(t, sent.GetSentRef()); r.Folder != "Sent" {
		t.Errorf("sent_ref = %v", r)
	}
	last := lastAudit(t, e.Harness)
	if last.Action != "draft.send" || last.Result != "ok" || !strings.Contains(last.Targets, dr.String()) || !strings.Contains(last.Targets, "draft.1@example.com") {
		t.Errorf("audit = %+v", last)
	}

	// The same key again is a duplicate: nothing is sent and the draft's
	// absence does not matter.
	again, err := e.sendDraft(t, dr.String(), "draft-send")
	if err != nil {
		t.Fatalf("repeat SendDraft: %v", err)
	}
	if !again.GetSent().GetDuplicate() || again.GetSent().GetMessageId() != "draft.1@example.com" {
		t.Errorf("repeat = %+v", again)
	}
	if len(srv.Messages()) != 1 {
		t.Errorf("captured %d messages, want 1", len(srv.Messages()))
	}
	if d := e.decisions(t, "draft.send"); !slices.Equal(d, []string{"", "duplicate"}) {
		t.Errorf("decisions = %v", d)
	}
	e.checkAuditPrivate(t)
}

func TestSendDraftCreatedByCreateDraft(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	d := e.createDraft(t, hostSpec())
	res, err := e.sendDraft(t, d.GetRef(), "k")
	if err != nil {
		t.Fatalf("SendDraft: %v", err)
	}
	if res.GetSent().GetMessageId() != d.GetMessageId() {
		t.Errorf("sent %q, draft %q", res.GetSent().GetMessageId(), d.GetMessageId())
	}
	if want := []string{"you@example.net", "cc@example.net", "hidden@example.net"}; !slices.Equal(srv.Messages()[0].To, want) {
		t.Errorf("RCPT TO = %v, want %v", srv.Messages()[0].To, want)
	}
}

func TestSendDraftFailureKeepsTheDraft(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	e.smtp(t, smtptest.Options{DropBeforeDataEnd: true})
	d := e.createDraft(t, hostSpec())
	if _, err := e.sendDraft(t, d.GetRef(), "k"); reasonOf(err) != api.ReasonServerUnavailable {
		t.Fatalf("err = %v, want SERVER_UNAVAILABLE", err)
	}
	if gone(t, e.host, parseRef(t, d.GetRef())) {
		t.Error("a failed send removed the draft")
	}
}

func TestDeleteDraft(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	d := e.createDraft(t, hostSpec())
	if err := e.deleteDraft(t, d.GetRef()); err != nil {
		t.Fatalf("DeleteDraft: %v", err)
	}
	if !gone(t, e.host, parseRef(t, d.GetRef())) {
		t.Error("the draft is still there")
	}
	if last := lastAudit(t, e.Harness); last.Action != "draft.delete" || last.Result != "ok" || !strings.Contains(last.Targets, d.GetRef()) {
		t.Errorf("audit = %+v", last)
	}
	if err := e.deleteDraft(t, d.GetRef()); reasonOf(err) != api.ReasonStaleRef {
		t.Errorf("second delete: err = %v, want STALE_REF", err)
	}
}

// TestDraftCallsRefuseRefsOutsideDrafts checks that UpdateDraft, SendDraft
// and DeleteDraft never touch a message that is not a draft.
func TestDraftCallsRefuseRefsOutsideDrafts(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	srv := e.smtp(t, smtptest.Options{})
	orig := e.original(t)
	if _, err := e.updateDraft(t, orig.String(), hostSpec()); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("UpdateDraft: err = %v, want INVALID_ARGUMENT", err)
	}
	if _, err := e.sendDraft(t, orig.String(), "k"); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("SendDraft: err = %v, want INVALID_ARGUMENT", err)
	}
	if err := e.deleteDraft(t, orig.String()); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("DeleteDraft: err = %v, want INVALID_ARGUMENT", err)
	}
	if gone(t, e.host, orig) || len(e.host.Appended()) != 0 || len(srv.Messages()) != 0 {
		t.Error("a draft call changed or sent a message outside Drafts")
	}
	if slices.Contains(e.host.Ops(), "Expunge") {
		t.Error("a draft call expunged")
	}
	// A draft's ref must name an account the spec of UpdateDraft agrees with.
	d := e.createDraft(t, hostSpec())
	spec := hostSpec()
	spec.Account = "small"
	if _, err := e.updateDraft(t, d.GetRef(), spec); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("UpdateDraft across accounts: err = %v, want INVALID_ARGUMENT", err)
	}
	if gone(t, e.host, parseRef(t, d.GetRef())) {
		t.Error("a refused update removed the draft")
	}
}

func TestGmailDrafts(t *testing.T) {
	t.Parallel()
	e := startSend(t)
	e.gmail.AddFolder("[Gmail]/Drafts", mailstore.RoleDrafts)
	var filed atomic.Pointer[ref.Ref]
	srv := e.smtp(t, smtptest.Options{
		// Gmail files sent mail itself.
		OnMessage: func(env smtptest.Envelope) {
			r, err := e.gmail.Append(context.Background(), "[Gmail]/Sent Mail", []string{`\Seen`}, time.Time{}, env.Data)
			if err != nil {
				t.Errorf("filing the message: %v", err)
				return
			}
			filed.Store(&r)
		},
	})
	spec := hostSpec()
	spec.Account = "gmail"
	d := e.createDraft(t, spec)
	dr := parseRef(t, d.GetRef())
	res, err := e.sendDraft(t, d.GetRef(), "g")
	if err != nil {
		t.Fatalf("SendDraft: %v", err)
	}
	if len(srv.Messages()) != 1 {
		t.Fatalf("captured %d messages", len(srv.Messages()))
	}
	// The Sent copy is the message Gmail filed, never the draft, which
	// has the same Message-ID.
	if r := filed.Load(); r == nil || res.GetSent().GetSentRef() != r.String() {
		t.Errorf("sent_ref = %q, want %v (draft %v)", res.GetSent().GetSentRef(), r, dr)
	}
	if !gone(t, e.gmail, dr) {
		t.Error("the Gmail draft is still there")
	}

	// A Gmail message without the Drafts label is not a draft.
	other := e.gmail.Add(fake.Message{Folder: "[Gmail]/All Mail", Subject: "x", Body: "x", MessageID: "<x@example.net>", Labels: []string{`\Inbox`}})
	if err := e.deleteDraft(t, other.String()); reasonOf(err) != api.ReasonInvalidArgument {
		t.Errorf("DeleteDraft of a non-draft: err = %v, want INVALID_ARGUMENT", err)
	}
	d2 := e.createDraft(t, spec)
	if err := e.deleteDraft(t, d2.GetRef()); err != nil {
		t.Fatalf("DeleteDraft: %v", err)
	}
	if !gone(t, e.gmail, parseRef(t, d2.GetRef())) {
		t.Error("the deleted Gmail draft is still there")
	}
}
