package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/ref"
)

const (
	statusSent      = "sent"
	statusDraft     = "draft"
	statusHeld      = "held"
	statusDuplicate = "duplicate"

	// keyPrefix starts every idempotency key this process derives.
	keyPrefix = "mcp-"
	// maxKeyLen is the daemon's limit on an idempotency key.
	maxKeyLen = 128
)

var (
	composeHints = hints{openWorld: true}
	heldNote     = "Policy may hold a send: then status is held, the message was NOT sent, it was saved to the account's Drafts for James, and it must not be retried."
	keyNote      = "Repeating the same input within 7 days replays the first result instead of sending twice."
)

// registerComposeTools adds send_email, reply, forward and the draft tools.
func (s *Server) registerComposeTools() {
	addTool(s, "send_email", "Send an email", "Send a new message from an account. "+heldNote+" "+keyNote, composeHints, s.sendEmail)
	addTool(s, "reply", "Reply to a message", "Reply to a message, quoting it under the new text. all replies to everyone. draft saves the reply to Drafts instead. "+heldNote+" "+keyNote, composeHints, s.reply)
	addTool(s, "forward", "Forward a message", "Forward a message with its attachments unless no_attachments. draft saves it to Drafts instead. "+heldNote+" "+keyNote, composeHints, s.forward)
	addTool(s, "create_draft", "Create a draft", "Save a new message to the account's Drafts folder without sending it.", composeHints, s.createDraft)
	addTool(s, "update_draft", "Update a draft", "Replace a draft with a new version. The result's draft_ref is the new ref; the old ref is gone.", composeHints, s.updateDraft)
	addTool(s, "send_draft", "Send a draft", "Send a draft and remove it from Drafts. "+heldNote+" "+keyNote, composeHints, s.sendDraft)
	addTool(s, "delete_draft", "Delete a draft", "Delete a draft. While policy disables expunge, the daemon moves it to Trash.", hints{idempotent: true, openWorld: true}, s.deleteDraft)
}

// contentIn is the body, attachments and options every writing tool takes.
type contentIn struct {
	Body        string            `json:"body,omitempty" jsonschema:"the new text, Markdown unless body_format says otherwise"`
	BodyFormat  string            `json:"body_format,omitempty" jsonschema:"markdown (the default), text or html"`
	HTMLText    string            `json:"html_text,omitempty" jsonschema:"with body_format html: the plain-text part; empty means derived from the HTML"`
	Attachments []string          `json:"attachments,omitempty" jsonschema:"absolute paths of files to attach, in order"`
	Inline      []inlineIn        `json:"inline,omitempty" jsonschema:"images the body shows inline"`
	NoSignature bool              `json:"no_signature,omitempty" jsonschema:"leave out the account's configured signature"`
	Headers     map[string]string `json:"headers,omitempty" jsonschema:"extra headers: Reply-To and X- headers only"`
}

type inlineIn struct {
	Name string `json:"name" jsonschema:"how the body refers to the image, as in ![chart](chart.png)"`
	Path string `json:"path" jsonschema:"absolute path of the image file"`
}

type sendEmailIn struct {
	Account string   `json:"account" jsonschema:"the account to send from, as list_accounts names it"`
	To      []string `json:"to" jsonschema:"recipients, each Name <a@b> or a@b"`
	Cc      []string `json:"cc,omitempty" jsonschema:"copied recipients"`
	Bcc     []string `json:"bcc,omitempty" jsonschema:"blind-copied recipients"`
	Subject string   `json:"subject" jsonschema:"the subject line"`
	contentIn
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"1 to 128 of A-Z a-z 0-9 . _ : -; a repeat with the same key does not send twice; derived from the input when empty"`
}

type replyIn struct {
	Ref string   `json:"ref" jsonschema:"the message to reply to"`
	All bool     `json:"all,omitempty" jsonschema:"reply to everyone: add the original To and Cc, less this account's own address"`
	To  []string `json:"to,omitempty" jsonschema:"more recipients, added to the ones the reply goes to"`
	Cc  []string `json:"cc,omitempty" jsonschema:"more copied recipients"`
	Bcc []string `json:"bcc,omitempty" jsonschema:"blind-copied recipients"`
	contentIn
	Draft          bool   `json:"draft,omitempty" jsonschema:"save the reply to Drafts instead of sending it"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"as in send_email"`
}

type forwardIn struct {
	Ref string   `json:"ref" jsonschema:"the message to forward"`
	To  []string `json:"to" jsonschema:"recipients, each Name <a@b> or a@b"`
	Cc  []string `json:"cc,omitempty" jsonschema:"copied recipients"`
	Bcc []string `json:"bcc,omitempty" jsonschema:"blind-copied recipients"`
	contentIn
	NoAttachments  bool   `json:"no_attachments,omitempty" jsonschema:"leave out the original's attachments"`
	Draft          bool   `json:"draft,omitempty" jsonschema:"save the forward to Drafts instead of sending it"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"as in send_email"`
}

type createDraftIn struct {
	Account string   `json:"account" jsonschema:"the account whose Drafts folder gets it"`
	To      []string `json:"to,omitempty" jsonschema:"recipients, each Name <a@b> or a@b"`
	Cc      []string `json:"cc,omitempty" jsonschema:"copied recipients"`
	Bcc     []string `json:"bcc,omitempty" jsonschema:"blind-copied recipients, kept in a Bcc header until sent"`
	Subject string   `json:"subject,omitempty" jsonschema:"the subject line"`
	contentIn
}

type updateDraftIn struct {
	Ref     string   `json:"ref" jsonschema:"the draft to replace; it must be in Drafts"`
	To      []string `json:"to,omitempty" jsonschema:"recipients of the new version"`
	Cc      []string `json:"cc,omitempty" jsonschema:"copied recipients"`
	Bcc     []string `json:"bcc,omitempty" jsonschema:"blind-copied recipients"`
	Subject string   `json:"subject,omitempty" jsonschema:"the subject line"`
	contentIn
}

type sendDraftIn struct {
	Ref            string `json:"ref" jsonschema:"the draft to send; it must be in Drafts"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"as in send_email"`
}

type deleteDraftIn struct {
	Ref string `json:"ref" jsonschema:"the draft to delete; it must be in Drafts"`
}

type deleteDraftOut struct {
	Ref     string `json:"ref"`
	Deleted bool   `json:"deleted"`
}

// composeOut is what a send, reply, forward or draft call did. A held
// result marshals through heldOut, so its held properties are always present.
type composeOut struct {
	Status            string      `json:"status" jsonschema:"sent, draft, held or duplicate"`
	OriginalStatus    string      `json:"original_status,omitempty" jsonschema:"with duplicate: what the first call with this key did"`
	MessageID         string      `json:"message_id,omitempty" jsonschema:"the Message-ID, without angle brackets"`
	SentRef           string      `json:"sent_ref,omitempty" jsonschema:"the ref of the copy in Sent"`
	SentCopyError     string      `json:"sent_copy_error,omitempty" jsonschema:"why the Sent copy could not be stored; the message itself was sent"`
	DraftRef          string      `json:"draft_ref,omitempty" jsonschema:"the ref of the saved draft; with held always present, empty when the server did not report it"`
	Blocked           []untrusted `json:"blocked,omitempty" jsonschema:"with held, always present: recipients not on the allowlist, possibly taken from the original message"`
	TooManyRecipients bool        `json:"too_many_recipients,omitempty" jsonschema:"with held, always present: more recipients than policy allows"`
	Hint              string      `json:"hint,omitempty" jsonschema:"what to tell James, or how to find the draft"`
	IdempotencyKey    string      `json:"idempotency_key,omitempty" jsonschema:"the key this call used"`
}

// heldOut is the wire form of a held composeOut: draft_ref, blocked,
// too_many_recipients and hint are present even when empty or false.
type heldOut struct {
	Status            string      `json:"status"`
	MessageID         string      `json:"message_id,omitempty"`
	DraftRef          string      `json:"draft_ref"`
	Blocked           []untrusted `json:"blocked"`
	TooManyRecipients bool        `json:"too_many_recipients"`
	Hint              string      `json:"hint"`
	IdempotencyKey    string      `json:"idempotency_key,omitempty"`
}

// MarshalJSON keeps the held properties on a held result and leaves them
// out of sent, duplicate and draft results.
func (o composeOut) MarshalJSON() ([]byte, error) {
	if o.Status != statusHeld {
		type plain composeOut // no methods, so no recursion
		return json.Marshal(plain(o))
	}
	blocked := o.Blocked
	if blocked == nil {
		blocked = []untrusted{}
	}
	return json.Marshal(heldOut{
		Status: o.Status, MessageID: o.MessageID, DraftRef: o.DraftRef, Blocked: blocked,
		TooManyRecipients: o.TooManyRecipients, Hint: o.Hint, IdempotencyKey: o.IdempotencyKey,
	})
}

// deriveKey returns "mcp-" and the lowercase unpadded base32 SHA-256 of the
// tool name, the canonical JSON of in, and the SHA-256 of each uploaded
// file, each length-prefixed. The caller passes in with its key cleared and
// its recipient lists sorted. The key is 56 characters, under maxKeyLen.
func deriveKey(tool string, in any, uploads []uploadedFile) (string, error) {
	canon, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("encoding the input for its idempotency key: %w", err)
	}
	h := sha256.New()
	field := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	field([]byte(tool))
	field(canon)
	for _, u := range uploads {
		field([]byte(u.SHA256))
	}
	key := keyPrefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(h.Sum(nil)))
	if len(key) > maxKeyLen {
		key = key[:maxKeyLen]
	}
	return key, nil
}

// sorted returns a sorted copy, nil when empty, so [] and an absent list key the same.
func sorted(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return slices.Sorted(slices.Values(s))
}

func composeBody(c contentIn) (*chainmailv1.ComposeBody, error) {
	if c.HTMLText != "" && c.BodyFormat != "html" {
		return nil, invalidArg("html_text needs body_format html")
	}
	switch c.BodyFormat {
	case "", "markdown":
		if c.Body == "" {
			return nil, nil
		}
		return &chainmailv1.ComposeBody{Kind: &chainmailv1.ComposeBody_Markdown{Markdown: c.Body}}, nil
	case "text":
		return &chainmailv1.ComposeBody{Kind: &chainmailv1.ComposeBody_Text{Text: c.Body}}, nil
	case "html":
		return &chainmailv1.ComposeBody{Kind: &chainmailv1.ComposeBody_Html{Html: c.Body}, HtmlText: c.HTMLText}, nil
	default:
		return nil, invalidArg("body_format must be markdown, text or html, got %q", c.BodyFormat)
	}
}

// content checks the body, uploads the attachments then the inline images
// for account, and returns the Content and the uploads in that order.
func (s *Server) content(ctx context.Context, account string, c contentIn) (*chainmailv1.Content, []uploadedFile, error) {
	body, err := composeBody(c)
	if err != nil {
		return nil, nil, err
	}
	paths := slices.Clone(c.Attachments)
	for _, im := range c.Inline {
		paths = append(paths, im.Path)
	}
	ups, err := s.uploadAttachments(ctx, account, paths)
	if err != nil {
		return nil, nil, err
	}
	out := &chainmailv1.Content{Body: body, NoSignature: c.NoSignature, Headers: c.Headers}
	for i := range c.Attachments {
		out.Attachments = append(out.Attachments, ups[i].Handle)
	}
	for i, im := range c.Inline {
		out.Inline = append(out.Inline, &chainmailv1.InlineImage{Name: im.Name, Upload: ups[len(c.Attachments)+i].Handle})
	}
	return out, ups, nil
}

// refAccount is the account a ref points into.
func refAccount(s string) (string, error) {
	r, err := ref.Parse(s)
	if err != nil {
		return "", invalidArg("ref is not a chainmail ref: pass refs from search, get_message or a draft result unchanged")
	}
	return r.Account, nil
}

// heldSummary is the trusted text of a held result. Blocked addresses can come
// from the original message, so it gives only their number; the addresses
// themselves appear only inside the untrusted block.
func heldSummary(o composeOut) string {
	var b strings.Builder
	b.WriteString("HELD BY POLICY: the message was NOT sent. It was saved to the account's Drafts folder for James to send, edit or delete")
	if o.DraftRef != "" {
		b.WriteString(" (draft " + o.DraftRef + ")")
	}
	b.WriteString(". Do not retry this send and do not look for another way to send it; tell James it is waiting in Drafts.")
	switch n := len(o.Blocked); {
	case n == 1:
		b.WriteString("\n1 recipient is not on the allowlist; it is listed in blocked below.")
	case n > 1:
		fmt.Fprintf(&b, "\n%d recipients are not on the allowlist; they are listed in blocked below.", n)
	}
	if o.TooManyRecipients {
		b.WriteString("\nMore recipients than policy allows.")
	}
	return b.String()
}

// composeFrom turns a ComposeResult into the tool output and its summary line.
func composeFrom(res *chainmailv1.ComposeResult, key string) (composeOut, string, error) {
	switch {
	case res.GetHeld() != nil:
		h := res.GetHeld()
		hint := h.GetHint()
		if hint == "" {
			hint = h.GetDraft().GetHint()
		}
		out := composeOut{
			Status: statusHeld, DraftRef: h.GetDraft().GetRef(), MessageID: h.GetDraft().GetMessageId(),
			TooManyRecipients: h.GetTooManyRecipients(), Hint: hint, IdempotencyKey: key,
		}
		for _, a := range h.GetBlocked() {
			out.Blocked = append(out.Blocked, untrusted{Untrusted: a})
		}
		return out, heldSummary(out), nil
	case res.GetSent() != nil:
		sent := res.GetSent()
		out := composeOut{Status: statusSent, MessageID: sent.GetMessageId(), SentRef: sent.GetSentRef(), SentCopyError: sent.GetSentCopyError(), IdempotencyKey: key}
		summary := "Sent " + out.MessageID + "."
		if sent.GetDuplicate() {
			out.Status, out.OriginalStatus = statusDuplicate, statusSent
			summary = "Duplicate: this key was already used and nothing was sent again. The first send went out as " + out.MessageID + "."
		}
		return out, summary, nil
	case res.GetDraft() != nil:
		d := res.GetDraft()
		out := composeOut{Status: statusDraft, DraftRef: d.GetRef(), MessageID: d.GetMessageId(), Hint: d.GetHint()}
		return out, "Saved to Drafts, not sent.", nil
	default:
		return composeOut{}, "", errors.New("the daemon returned no result")
	}
}

func (s *Server) composeResult(res *chainmailv1.ComposeResult, key string) (*mcp.CallToolResult, composeOut, error) {
	out, summary, err := composeFrom(res, key)
	if err != nil {
		return nil, composeOut{}, s.errorResult(err)
	}
	if out.Status != statusHeld {
		return &mcp.CallToolResult{Content: plainText(summary, out)}, out, nil
	}
	// Blocked addresses may be message-derived, so a held result is untrusted text.
	text, err := untrustedText(summary, out)
	if err != nil {
		return nil, composeOut{}, s.errorResult(err)
	}
	return &mcp.CallToolResult{Content: text}, out, nil
}

func (s *Server) sendEmail(ctx context.Context, _ *mcp.CallToolRequest, in sendEmailIn) (*mcp.CallToolResult, composeOut, error) {
	content, ups, err := s.content(ctx, in.Account, in.contentIn)
	if err != nil {
		return nil, composeOut{}, s.errorResult(err)
	}
	key := in.IdempotencyKey
	if key == "" {
		canon := in
		canon.To, canon.Cc, canon.Bcc = sorted(in.To), sorted(in.Cc), sorted(in.Bcc)
		if key, err = deriveKey("send_email", canon, ups); err != nil {
			return nil, composeOut{}, s.errorResult(err)
		}
	}
	resp, err := s.daemon.Compose.Send(ctx, connect.NewRequest(&chainmailv1.SendRequest{
		Spec:           &chainmailv1.ComposeSpec{Account: in.Account, To: in.To, Cc: in.Cc, Bcc: in.Bcc, Subject: in.Subject, Content: content},
		IdempotencyKey: key,
	}))
	if err != nil {
		return nil, composeOut{}, s.errorResult(err)
	}
	return s.composeResult(resp.Msg.GetResult(), key)
}

func (s *Server) reply(ctx context.Context, _ *mcp.CallToolRequest, in replyIn) (*mcp.CallToolResult, composeOut, error) {
	account, err := refAccount(in.Ref)
	if err != nil {
		return nil, composeOut{}, s.errorResult(err)
	}
	content, ups, err := s.content(ctx, account, in.contentIn)
	if err != nil {
		return nil, composeOut{}, s.errorResult(err)
	}
	key := in.IdempotencyKey
	if key == "" && !in.Draft {
		canon := in
		canon.To, canon.Cc, canon.Bcc = sorted(in.To), sorted(in.Cc), sorted(in.Bcc)
		if key, err = deriveKey("reply", canon, ups); err != nil {
			return nil, composeOut{}, s.errorResult(err)
		}
	}
	resp, err := s.daemon.Compose.Reply(ctx, connect.NewRequest(&chainmailv1.ReplyRequest{
		Ref: in.Ref, All: in.All, Content: content, To: in.To, Cc: in.Cc, Bcc: in.Bcc, Draft: in.Draft, IdempotencyKey: key,
	}))
	if err != nil {
		return nil, composeOut{}, s.errorResult(err)
	}
	return s.composeResult(resp.Msg.GetResult(), key)
}

func (s *Server) forward(ctx context.Context, _ *mcp.CallToolRequest, in forwardIn) (*mcp.CallToolResult, composeOut, error) {
	account, err := refAccount(in.Ref)
	if err != nil {
		return nil, composeOut{}, s.errorResult(err)
	}
	content, ups, err := s.content(ctx, account, in.contentIn)
	if err != nil {
		return nil, composeOut{}, s.errorResult(err)
	}
	key := in.IdempotencyKey
	if key == "" && !in.Draft {
		canon := in
		canon.To, canon.Cc, canon.Bcc = sorted(in.To), sorted(in.Cc), sorted(in.Bcc)
		if key, err = deriveKey("forward", canon, ups); err != nil {
			return nil, composeOut{}, s.errorResult(err)
		}
	}
	resp, err := s.daemon.Compose.Forward(ctx, connect.NewRequest(&chainmailv1.ForwardRequest{
		Ref: in.Ref, To: in.To, Cc: in.Cc, Bcc: in.Bcc, Content: content, NoAttachments: in.NoAttachments, Draft: in.Draft, IdempotencyKey: key,
	}))
	if err != nil {
		return nil, composeOut{}, s.errorResult(err)
	}
	return s.composeResult(resp.Msg.GetResult(), key)
}

func (s *Server) createDraft(ctx context.Context, _ *mcp.CallToolRequest, in createDraftIn) (*mcp.CallToolResult, composeOut, error) {
	content, _, err := s.content(ctx, in.Account, in.contentIn)
	if err != nil {
		return nil, composeOut{}, s.errorResult(err)
	}
	resp, err := s.daemon.Compose.CreateDraft(ctx, connect.NewRequest(&chainmailv1.CreateDraftRequest{
		Spec: &chainmailv1.ComposeSpec{Account: in.Account, To: in.To, Cc: in.Cc, Bcc: in.Bcc, Subject: in.Subject, Content: content},
	}))
	if err != nil {
		return nil, composeOut{}, s.errorResult(err)
	}
	return s.composeResult(&chainmailv1.ComposeResult{Result: &chainmailv1.ComposeResult_Draft{Draft: resp.Msg.GetDraft()}}, "")
}

func (s *Server) updateDraft(ctx context.Context, _ *mcp.CallToolRequest, in updateDraftIn) (*mcp.CallToolResult, composeOut, error) {
	account, err := refAccount(in.Ref)
	if err != nil {
		return nil, composeOut{}, s.errorResult(err)
	}
	content, _, err := s.content(ctx, account, in.contentIn)
	if err != nil {
		return nil, composeOut{}, s.errorResult(err)
	}
	resp, err := s.daemon.Compose.UpdateDraft(ctx, connect.NewRequest(&chainmailv1.UpdateDraftRequest{
		Ref:  in.Ref,
		Spec: &chainmailv1.ComposeSpec{Account: account, To: in.To, Cc: in.Cc, Bcc: in.Bcc, Subject: in.Subject, Content: content},
	}))
	if err != nil {
		return nil, composeOut{}, s.errorResult(err)
	}
	return s.composeResult(&chainmailv1.ComposeResult{Result: &chainmailv1.ComposeResult_Draft{Draft: resp.Msg.GetDraft()}}, "")
}

func (s *Server) sendDraft(ctx context.Context, _ *mcp.CallToolRequest, in sendDraftIn) (*mcp.CallToolResult, composeOut, error) {
	key := in.IdempotencyKey
	if key == "" {
		var err error
		if key, err = deriveKey("send_draft", sendDraftIn{Ref: in.Ref}, nil); err != nil {
			return nil, composeOut{}, s.errorResult(err)
		}
	}
	resp, err := s.daemon.Compose.SendDraft(ctx, connect.NewRequest(&chainmailv1.SendDraftRequest{Ref: in.Ref, IdempotencyKey: key}))
	if err != nil {
		return nil, composeOut{}, s.errorResult(err)
	}
	return s.composeResult(resp.Msg.GetResult(), key)
}

func (s *Server) deleteDraft(ctx context.Context, _ *mcp.CallToolRequest, in deleteDraftIn) (*mcp.CallToolResult, deleteDraftOut, error) {
	if _, err := s.daemon.Compose.DeleteDraft(ctx, connect.NewRequest(&chainmailv1.DeleteDraftRequest{Ref: in.Ref})); err != nil {
		return nil, deleteDraftOut{}, s.errorResult(err)
	}
	out := deleteDraftOut{Ref: in.Ref, Deleted: true}
	return &mcp.CallToolResult{Content: plainText("Deleted the draft.", out)}, out, nil
}
