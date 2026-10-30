package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/compose"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/render"
	"github.com/jamesainslie/chainmail/internal/version"
)

const (
	// flagAnswered and flagForwarded mark an original after a reply or a forward.
	flagAnswered  = `\Answered`
	flagForwarded = "$Forwarded"
	// maxHTMLBytes caps the HTML part read from an original for quoting.
	maxHTMLBytes = 16 << 20
	// maxMIMEDepth bounds the walk for an original's HTML part.
	maxMIMEDepth = 16
	// referencesKept is how many Message-IDs a References header keeps.
	referencesKept = 20
)

// buildFunc makes the message of a send-type call.
type buildFunc = func(ctx context.Context) (compose.Message, error)

// Reply implements chainmailv1connect.ComposeServiceHandler.
func (c *composeService) Reply(ctx context.Context, req *connect.Request[chainmailv1.ReplyRequest]) (*connect.Response[chainmailv1.ReplyResponse], error) {
	cl, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, composeTimeout)
	defer cancel()
	m := req.Msg
	res, err := c.answer(ctx, cl, answer{
		action: "reply", ref: m.GetRef(), content: m.GetContent(),
		to: m.GetTo(), cc: m.GetCc(), bcc: m.GetBcc(),
		dryRun: m.GetDryRun(), draft: m.GetDraft(), key: m.GetIdempotencyKey(), req: m,
		flag: flagAnswered,
		parts: func(spec *compose.Spec, orig compose.Original, self string) error {
			p, err := compose.Reply(orig, []string{self}, m.GetAll(), nil)
			if err != nil {
				return err
			}
			spec.To = mergeAddresses(p.To, spec.To)
			// An address already in To is not repeated in Cc.
			spec.Cc = slices.DeleteFunc(mergeAddresses(p.Cc, spec.Cc), func(a mail.Address) bool { return hasAddress(spec.To, a) })
			spec.Subject, spec.InReplyTo, spec.References, spec.Quote = p.Subject, p.InReplyTo, p.References, p.Quote
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.ReplyResponse{Result: res}), nil
}

// Forward implements chainmailv1connect.ComposeServiceHandler.
func (c *composeService) Forward(ctx context.Context, req *connect.Request[chainmailv1.ForwardRequest]) (*connect.Response[chainmailv1.ForwardResponse], error) {
	cl, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, composeTimeout)
	defer cancel()
	m := req.Msg
	res, err := c.answer(ctx, cl, answer{
		action: "forward", ref: m.GetRef(), content: m.GetContent(),
		to: m.GetTo(), cc: m.GetCc(), bcc: m.GetBcc(), needRecipients: true,
		dryRun: m.GetDryRun(), draft: m.GetDraft(), key: m.GetIdempotencyKey(), req: m,
		flag: flagForwarded, attachments: !m.GetNoAttachments(),
		parts: func(spec *compose.Spec, orig compose.Original, _ string) error {
			p := compose.Forward(orig, nil)
			spec.Subject, spec.Quote = p.Subject, p.Quote
			// The caller's own attachments come first, then the original's.
			spec.Attachments = slices.Concat(spec.Attachments, p.Attachments)
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.ForwardResponse{Result: res}), nil
}

// answer is a reply or a forward, after the handler read its request.
type answer struct {
	// action is the audit action of a send or dry run.
	action        string
	ref           string
	content       *chainmailv1.Content
	to, cc, bcc   []string
	dryRun, draft bool
	key           string
	// req is the whole request, for the idempotency hash.
	req proto.Message
	// needRecipients refuses a call without to, cc or bcc before any I/O.
	needRecipients bool
	// flag goes on the original once the message is sent.
	flag string
	// attachments reads the original's attachments.
	attachments bool
	// parts fills spec from the original. self is the account's address.
	parts func(spec *compose.Spec, orig compose.Original, self string) error
}

// answer runs a reply or a forward: the original is read inside build, so
// it is fetched while the SMTP connection is being made.
func (c *composeService) answer(ctx context.Context, cl caller, a answer) (*chainmailv1.ComposeResult, error) {
	entry := audit.Entry{Action: a.action}
	if a.draft {
		entry.Action = "draft.create"
	}
	r, err := ref.Parse(a.ref)
	if err != nil {
		entry.Targets = []string{invalidTarget}
		return nil, c.record(ctx, cl, entry, err)
	}
	entry.Account, entry.Targets = r.Account, []string{r.String()}
	if a.dryRun && a.draft {
		return nil, c.record(ctx, cl, entry, errDryRunAndDraft)
	}
	if a.needRecipients && len(a.to)+len(a.cc)+len(a.bcc) == 0 {
		return nil, c.record(ctx, cl, entry, errNoForwardRecipients)
	}
	acct, err := c.account(r.Account)
	if err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	if !a.dryRun && !a.draft {
		if res, ok, err := c.replaySettled(ctx, cl, acct, a.key, entry); ok {
			return res, err
		}
	}
	spec, err := c.answerSpec(ctx, acct, a)
	if err == nil {
		err = checkSize(acct, inputSize(spec))
	}
	if err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	build := func(ctx context.Context) (compose.Message, error) {
		orig, err := c.original(ctx, acct, r, a.attachments)
		if err != nil {
			return compose.Message{}, err
		}
		s := spec
		if err := a.parts(&s, orig, acct.Address); err != nil {
			return compose.Message{}, err
		}
		return compose.Build(s)
	}
	switch {
	case a.dryRun:
		return c.dryRun(ctx, cl, acct, entry, build)
	case a.draft:
		return c.draft(ctx, cl, acct, entry, withBcc(build, spec.Bcc))
	}
	hash, err := requestHash(a.req, uploadSums(a.content, spec))
	if err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	return c.run(ctx, sendJob{
		acct: acct, key: a.key, hash: hash, build: build,
		after:  func(ctx context.Context) { c.markOriginal(ctx, acct, r, a.flag) },
		action: a.action, targets: []string{r.String()}, caller: cl,
	})
}

// errNoForwardRecipients is a forward to nobody.
var errNoForwardRecipients = &argError{msg: "a forward needs at least one recipient", hint: "pass to, cc or bcc"}

// answerSpec is the spec of a reply or forward before the original is read:
// From, the date, the caller's extra recipients and the caller's content.
func (c *composeService) answerSpec(ctx context.Context, acct config.ResolvedAccount, a answer) (compose.Spec, error) {
	if err := ctx.Err(); err != nil {
		return compose.Spec{}, err
	}
	spec := compose.Spec{
		From:      mail.Address{Name: acct.DisplayName, Address: acct.Address},
		Date:      c.s.opts.Now().Local(),
		UserAgent: "chainmail/" + version.Version,
	}
	var err error
	if spec.To, err = parseAddresses("to", a.to); err != nil {
		return compose.Spec{}, err
	}
	if spec.Cc, err = parseAddresses("cc", a.cc); err != nil {
		return compose.Spec{}, err
	}
	if spec.Bcc, err = parseAddresses("bcc", a.bcc); err != nil {
		return compose.Spec{}, err
	}
	if err := c.applyContent(&spec, acct, a.content); err != nil {
		return compose.Spec{}, err
	}
	return spec, nil
}

// markOriginal sets flag on the message a send answered. A failure is
// logged: the message was sent.
func (c *composeService) markOriginal(ctx context.Context, acct config.ResolvedAccount, r ref.Ref, flag string) {
	for _, res := range c.s.stores.get(acct).StoreFlags(ctx, []ref.Ref{r}, []string{flag}, nil) {
		if res.Err != nil {
			c.s.opts.Logger.Warn("marking the original after sending", "account", acct.Name, "ref", r.String(), "flag", flag, "err", res.Err)
		}
	}
}

// mergeAddresses returns first, then the addresses of extra not already in
// it, comparing without case.
func mergeAddresses(first, extra []mail.Address) []mail.Address {
	out := slices.Clone(first)
	for _, a := range extra {
		if !hasAddress(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// hasAddress reports whether list has a's address, compared without case.
func hasAddress(list []mail.Address, a mail.Address) bool {
	return slices.ContainsFunc(list, func(b mail.Address) bool { return strings.EqualFold(a.Address, b.Address) })
}

// original reads the message r points at, as a reply or forward quotes it:
// the header from FetchMessages, the text and HTML from its raw MIME (the
// whole message streamed when a text part was clipped), and, with
// attachments, every attachment part streamed. A failed read fails the
// call rather than quote part of the original. When the text can only be
// had cut short, because the whole message is over the account's limit or
// a part over a reading cap, the quote ends with truncatedMarker.
func (c *composeService) original(ctx context.Context, acct config.ResolvedAccount, r ref.Ref, attachments bool) (compose.Original, error) {
	st := c.s.stores.get(acct)
	res := st.FetchMessages(ctx, []ref.Ref{r})
	if len(res) != 1 {
		return compose.Original{}, fmt.Errorf("%w: the message could not be read", mailstore.ErrUnavailable)
	}
	if res[0].Err != nil {
		return compose.Original{}, res[0].Err
	}
	msg := res[0].Message
	raw := msg.Raw
	truncated := false
	if msg.TextClipped {
		_, full, err := readPart(ctx, st, r, "", acct.MaxMessageSize)
		switch {
		case errors.Is(err, errPartTooLarge):
			truncated = true
		case err != nil:
			return compose.Original{}, err
		default:
			raw = full
		}
	}
	body, err := render.Render(raw, render.Options{Full: true})
	if err != nil {
		return compose.Original{}, fmt.Errorf("rendering the original: %w", err)
	}
	htm, htmlCut := htmlPart(raw)
	orig := compose.Original{
		MessageID:  validMsgID(mailstore.MessageIDs([]string{msg.MessageID})),
		References: validMsgIDs(mailstore.MessageIDs(msg.References)),
		From:       mailAddresses(msg.Summary.From),
		ReplyTo:    replyTo(raw),
		To:         mailAddresses(msg.Summary.To),
		Cc:         mailAddresses(msg.Cc),
		Subject:    msg.Summary.Subject,
		Date:       msg.Summary.Date,
		Text:       body.Text,
		HTML:       htm,
	}
	if truncated || body.Truncated || htmlCut {
		markTruncated(&orig)
	}
	if attachments {
		if orig.Attachments, err = c.originalAttachments(ctx, st, acct, r, msg.Attachments); err != nil {
			return compose.Original{}, err
		}
	}
	return orig, nil
}

// truncatedMarker is the last line of a quote whose original text is incomplete.
const truncatedMarker = "[quoted text truncated]"

// markTruncated ends the original's text, and its HTML when it has any,
// with truncatedMarker, so the quote in both parts says it is incomplete.
func markTruncated(orig *compose.Original) {
	if t := strings.TrimRight(orig.Text, "\n"); t != "" {
		orig.Text = t + "\n" + truncatedMarker + "\n"
	} else {
		orig.Text = truncatedMarker + "\n"
	}
	if orig.HTML != "" {
		orig.HTML += "\n<p>" + truncatedMarker + "</p>\n"
	}
}

// errOriginalTooLarge is a forward whose original attachments do not fit.
var errOriginalTooLarge = &argError{
	msg:  "the original's attachments are over the account's message size limit",
	hint: "forward with no_attachments, or send the files as links",
}

// originalAttachments streams each attachment of r. Together they must fit
// the account's message size limit, base64-encoded.
func (c *composeService) originalAttachments(ctx context.Context, st mailstore.Store, acct config.ResolvedAccount, r ref.Ref, list []mailstore.Attachment) ([]compose.File, error) {
	var (
		out   []compose.File
		total int64
	)
	for _, a := range list {
		info, data, err := readPart(ctx, st, r, a.Part, acct.MaxMessageSize)
		if errors.Is(err, errPartTooLarge) {
			return nil, errOriginalTooLarge
		}
		if err != nil {
			return nil, err
		}
		if total += base64Len(len(data)); total > acct.MaxMessageSize {
			return nil, errOriginalTooLarge
		}
		name := a.Filename
		if name == "" {
			name = info.Filename
		}
		ct := a.ContentType
		if ct == "" {
			ct = info.ContentType
		}
		out = append(out, compose.File{Filename: render.CleanHeader(name), ContentType: attachmentType(ct), Data: data})
	}
	return out, nil
}

// errPartTooLarge is a part longer than the limit readPart was given.
var errPartTooLarge = errors.New("the part is over the size limit")

// readPart streams part of r, or the whole message when part is "", and
// reads at most limit bytes of it.
func readPart(ctx context.Context, st mailstore.Store, r ref.Ref, part string, limit int64) (mailstore.PartInfo, []byte, error) {
	info, rc, err := st.StreamPart(ctx, r, part)
	if err != nil {
		return mailstore.PartInfo{}, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if cerr := rc.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return mailstore.PartInfo{}, nil, err
	}
	if int64(len(data)) > limit {
		return mailstore.PartInfo{}, nil, errPartTooLarge
	}
	return info, data, nil
}

// attachmentType is ct reduced to what a re-attached part needs: the media
// type, with its charset and boundary. A type that does not parse becomes
// application/octet-stream, so a malformed original still forwards.
func attachmentType(ct string) string {
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil || !strings.Contains(mt, "/") {
		return "application/octet-stream"
	}
	keep := map[string]string{}
	for _, k := range []string{"charset", "boundary"} {
		if v := params[k]; v != "" {
			keep[k] = v
		}
	}
	if out := mime.FormatMediaType(mt, keep); out != "" {
		return out
	}
	return mime.FormatMediaType(mt, nil)
}

// mailAddresses converts addresses, keeping only the bare addr-specs a
// message can be sent to.
func mailAddresses(list []mailstore.Address) []mail.Address {
	var out []mail.Address
	for _, a := range list {
		if p, err := mail.ParseAddress(a.Email); err == nil && p.Name == "" && p.Address == a.Email {
			out = append(out, mail.Address{Name: a.Name, Address: a.Email})
		}
	}
	return out
}

// replyTo reads the Reply-To addresses from raw's header. A missing or
// malformed field gives none, so the reply goes to From.
func replyTo(raw []byte) []mail.Address {
	th, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return nil
	}
	h := gomail.Header{Header: message.Header{Header: th}}
	list, err := h.AddressList("Reply-To")
	if err != nil {
		return nil
	}
	conv := make([]mailstore.Address, 0, len(list))
	for _, a := range list {
		conv = append(conv, mailstore.Address{Name: a.Name, Email: a.Address})
	}
	return mailAddresses(conv)
}

// validMsgID returns the first of ids a header can carry, or "".
func validMsgID(ids []string) string {
	if v := validMsgIDs(ids); len(v) > 0 {
		return v[0]
	}
	return ""
}

// validMsgIDs keeps the ids that can go inside angle brackets in a header:
// an @, and no white space, control characters or brackets.
func validMsgIDs(ids []string) []string {
	var out []string
	for _, id := range ids {
		if strings.Contains(id, "@") && !strings.ContainsFunc(id, func(r rune) bool {
			return r <= ' ' || r == 0x7f || r == '<' || r == '>'
		}) {
			out = append(out, id)
		}
	}
	return out
}

// htmlPart returns the first HTML part of raw that is not an attachment,
// decoded to UTF-8, or "" when it has none. cut says the part is
// incomplete: over maxHTMLBytes, or failing to decode partway.
func htmlPart(raw []byte) (htm string, cut bool) {
	// An unknown charset still leaves a readable entity; no entity at all
	// means there is no HTML to quote.
	e, _ := message.Read(bytes.NewReader(raw))
	if e == nil {
		return "", false
	}
	return findHTML(e, 0)
}

func findHTML(e *message.Entity, depth int) (string, bool) {
	if depth > maxMIMEDepth || leadingType(e.Header.Get("Content-Disposition")) == "attachment" {
		return "", false
	}
	if mr := e.MultipartReader(); mr != nil {
		for {
			p, _ := mr.NextPart() // a part with an unknown charset comes with an error but is still readable
			if p == nil {
				return "", false
			}
			if s, cut := findHTML(p, depth+1); s != "" {
				return s, cut
			}
		}
	}
	if leadingType(e.Header.Get("Content-Type")) != "text/html" {
		return "", false
	}
	b, err := io.ReadAll(io.LimitReader(e.Body, maxHTMLBytes+1)) // a decoding error keeps what was read
	if len(b) > maxHTMLBytes {
		return string(b[:maxHTMLBytes]), true
	}
	return string(b), err != nil
}

// leadingType is the lower-case type of a Content-Type or
// Content-Disposition value, read from its first token only, so broken
// parameters do not hide it.
func leadingType(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.IndexAny(v, "; \t\r\n"); i >= 0 {
		v = v[:i]
	}
	t, _, err := mime.ParseMediaType(v)
	if err != nil {
		return ""
	}
	return t
}

// withBcc returns build with the spec's Bcc written into the message
// header. Drafts keep their Bcc this way, as mail clients' drafts do, so
// SendDraft still sends to it; SendDraft strips the field before sending.
func withBcc(build buildFunc, bcc []mail.Address) buildFunc {
	if len(bcc) == 0 {
		return build
	}
	return func(ctx context.Context) (compose.Message, error) {
		m, err := build(ctx)
		if err != nil {
			return m, err
		}
		m.Raw, err = editHeader(m.Raw, func(h *gomail.Header) {
			list := make([]*mail.Address, len(bcc))
			for i := range bcc {
				list[i] = &bcc[i]
			}
			h.SetAddressList("Bcc", list)
		})
		return m, err
	}
}

// editHeader applies edit to raw's header and returns the message with the
// body unchanged.
func editHeader(raw []byte, edit func(*gomail.Header)) ([]byte, error) {
	br := bufio.NewReader(bytes.NewReader(raw))
	th, err := textproto.ReadHeader(br)
	if err != nil || th.Len() == 0 {
		return nil, errRawUnparsable
	}
	body, err := io.ReadAll(br)
	if err != nil {
		return nil, errRawUnparsable
	}
	h := gomail.Header{Header: message.Header{Header: th}}
	edit(&h)
	var buf bytes.Buffer
	if err := textproto.WriteHeader(&buf, h.Header.Header); err != nil {
		return nil, fmt.Errorf("writing the message header: %w", err)
	}
	buf.Write(body)
	return buf.Bytes(), nil
}
