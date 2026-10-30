package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/compose"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/sender"
	"github.com/jamesainslie/chainmail/internal/upload"
)

// maxFilenameBytes caps an upload's filename.
const maxFilenameBytes = 255

// composeService sends mail and manages drafts. Calls not yet implemented
// answer CodeUnimplemented through the embedded handler.
type composeService struct {
	chainmailv1connect.UnimplementedComposeServiceHandler
	s *Server
	// dial opens SMTP connections. Tests replace it.
	dial dialFunc
	// outcomeTries and outcomeInterval shape the Sent search for a send
	// whose outcome is unknown.
	outcomeTries    int
	outcomeInterval time.Duration

	// mu guards owners, the account of each live upload.
	mu     sync.Mutex
	owners map[string]uploadOwner
}

func newComposeService(s *Server) *composeService {
	interval := s.opts.OutcomeSearchInterval
	if interval <= 0 {
		interval = defaultOutcomeInterval
	}
	return &composeService{
		s:               s,
		dial:            sender.Dial,
		outcomeTries:    outcomeTries,
		outcomeInterval: interval,
		owners:          map[string]uploadOwner{},
	}
}

// Send implements chainmailv1connect.ComposeServiceHandler.
func (c *composeService) Send(ctx context.Context, req *connect.Request[chainmailv1.SendRequest]) (*connect.Response[chainmailv1.SendResponse], error) {
	cl, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, composeTimeout)
	defer cancel()
	res, err := c.send(ctx, cl, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.SendResponse{Result: res}), nil
}

// send runs a SendRequest: a dry run, a draft, or a keyed send.
func (c *composeService) send(ctx context.Context, cl caller, req *chainmailv1.SendRequest) (*chainmailv1.ComposeResult, error) {
	p := req.GetSpec()
	entry := audit.Entry{Action: "send"}
	if req.GetDraft() {
		entry.Action = "draft.create"
	}
	target, err := accountTarget(p.GetAccount())
	entry.Account = target
	if err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	if req.GetDryRun() && req.GetDraft() {
		return nil, c.record(ctx, cl, entry, errDryRunAndDraft)
	}
	acct, err := c.account(target)
	if err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	if !req.GetDryRun() && !req.GetDraft() {
		if res, ok, err := c.replaySettled(ctx, cl, acct, req.GetIdempotencyKey(), entry); ok {
			return res, err
		}
	}
	spec, err := c.specFromProto(ctx, p)
	if err == nil {
		err = checkSize(acct, inputSize(spec))
	}
	if err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	build := func(context.Context) (compose.Message, error) { return compose.Build(spec) }
	switch {
	case req.GetDryRun():
		return c.dryRun(ctx, cl, acct, entry, build)
	case req.GetDraft():
		return c.draft(ctx, cl, acct, entry, withBcc(build, spec.Bcc))
	}
	hash, err := requestHash(req, uploadSums(p.GetContent(), spec))
	if err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	return c.run(ctx, sendJob{acct: acct, key: req.GetIdempotencyKey(), hash: hash, build: build, action: entry.Action, caller: cl})
}

// dryRun builds the message and returns it with its envelope, without
// dialing or taking an idempotency key.
func (c *composeService) dryRun(ctx context.Context, cl caller, acct config.ResolvedAccount, entry audit.Entry,
	build func(context.Context) (compose.Message, error),
) (*chainmailv1.ComposeResult, error) {
	entry.Decision = decisionDryRun
	msg, err := build(ctx)
	if err == nil {
		err = checkSize(acct, int64(len(msg.Raw)))
	}
	if err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	entry.Targets = append(entry.Targets, msg.MessageID)
	if err := c.record(ctx, cl, entry, nil); err != nil {
		return nil, err
	}
	return &chainmailv1.ComposeResult{Result: &chainmailv1.ComposeResult_DryRun{DryRun: &chainmailv1.DryRun{
		Raw: msg.Raw, EnvelopeFrom: msg.EnvelopeFrom, EnvelopeTo: msg.EnvelopeTo,
	}}}, nil
}

// draft builds the message and saves it to Drafts instead of sending it.
func (c *composeService) draft(ctx context.Context, cl caller, acct config.ResolvedAccount, entry audit.Entry,
	build func(context.Context) (compose.Message, error),
) (*chainmailv1.ComposeResult, error) {
	msg, err := build(ctx)
	if err == nil {
		err = checkSize(acct, int64(len(msg.Raw)))
	}
	if err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	base := entry.Targets
	entry.Targets = slices.Concat(base, []string{msg.MessageID})
	d, err := c.saveDraft(ctx, acct, msg)
	if err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	entry.Targets = appendTargets(slices.Clone(base), d.GetRef(), msg.MessageID)
	if err := c.record(ctx, cl, entry, nil); err != nil {
		return nil, err
	}
	return &chainmailv1.ComposeResult{Result: &chainmailv1.ComposeResult_Draft{Draft: d}}, nil
}

// SendRaw implements chainmailv1connect.ComposeServiceHandler.
func (c *composeService) SendRaw(ctx context.Context, req *connect.Request[chainmailv1.SendRawRequest]) (*connect.Response[chainmailv1.SendRawResponse], error) {
	cl, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, composeTimeout)
	defer cancel()
	res, err := c.sendRaw(ctx, cl, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.SendRawResponse{Result: res}), nil
}

func (c *composeService) sendRaw(ctx context.Context, cl caller, req *chainmailv1.SendRawRequest) (*chainmailv1.ComposeResult, error) {
	const action = "send_raw"
	target, err := accountTarget(req.GetAccount())
	entry := audit.Entry{Account: target, Action: action}
	if err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	acct, err := c.account(target)
	if err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	if res, ok, err := c.replaySettled(ctx, cl, acct, req.GetIdempotencyKey(), entry); ok {
		return res, err
	}
	if err := checkSize(acct, int64(len(req.GetRaw()))); err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	msg, err := prepareRaw(acct, req.GetRaw(), req.GetEnvelopeTo())
	if err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	hash, err := requestHash(req, nil)
	if err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	build := func(context.Context) (compose.Message, error) { return msg, nil }
	return c.run(ctx, sendJob{acct: acct, key: req.GetIdempotencyKey(), hash: hash, build: build, action: action, caller: cl})
}

// errRawUnparsable is a SendRaw message whose header does not parse. The
// parser's own text quotes the bytes, so it is dropped.
var errRawUnparsable = &argError{
	msg:  "the message does not parse: its header is malformed",
	hint: "send a whole RFC 5322 message: header fields, a blank line, then the body",
}

// prepareRaw checks a SendRaw message and returns it ready to send: it must
// parse, have a Message-ID and one From field, the account's address. Bcc
// is removed from the bytes. The envelope is envelopeTo, or else the To, Cc
// and Bcc of the message, every field of each read when one is repeated.
func prepareRaw(acct config.ResolvedAccount, raw []byte, envelopeTo []string) (compose.Message, error) {
	br := bufio.NewReader(bytes.NewReader(raw))
	th, err := textproto.ReadHeader(br)
	if err != nil || th.Len() == 0 {
		return compose.Message{}, errRawUnparsable
	}
	body, err := io.ReadAll(br)
	if err != nil {
		return compose.Message{}, errRawUnparsable
	}
	h := gomail.Header{Header: message.Header{Header: th}}
	id, err := h.MessageID()
	if err != nil || id == "" {
		return compose.Message{}, &argError{msg: "the message has no usable Message-ID", hint: "add a Message-ID header, as <unique@domain>"}
	}
	if len(h.Values("From")) > 1 {
		return compose.Message{}, &argError{
			msg:  "the message has more than one From field",
			hint: "keep one From field, the address of the account you send from",
		}
	}
	from, err := addressFields(h, "From")
	if err != nil || len(from) != 1 || !strings.EqualFold(from[0].Address, acct.Address) {
		return compose.Message{}, &argError{
			msg:  "From must be the account's address, " + acct.Address,
			hint: "set the From header to the address of the account you send from",
		}
	}
	var to []string
	if len(envelopeTo) > 0 {
		addrs, err := parseAddresses("envelope_to", envelopeTo)
		if err != nil {
			return compose.Message{}, err
		}
		for _, a := range addrs {
			to = append(to, a.Address)
		}
	} else {
		for _, key := range []string{"To", "Cc", "Bcc"} {
			list, err := addressFields(h, key)
			if err != nil {
				return compose.Message{}, &argError{
					msg:  "the " + key + " header is not a list of addresses",
					hint: "fix the header, or pass envelope_to",
				}
			}
			for _, a := range list {
				to = append(to, a.Address)
			}
		}
	}
	to = uniqueFold(to)
	if len(to) == 0 {
		return compose.Message{}, &argError{msg: "the message has no recipients", hint: "add To, Cc or Bcc, or pass envelope_to"}
	}
	h.Del("Bcc")
	var buf bytes.Buffer
	if err := textproto.WriteHeader(&buf, h.Header.Header); err != nil {
		return compose.Message{}, fmt.Errorf("writing the message header: %w", err)
	}
	buf.Write(body)
	return compose.Message{Raw: buf.Bytes(), MessageID: id, EnvelopeFrom: acct.Address, EnvelopeTo: to}, nil
}

// addressFields parses every key field of h, in order, into one list: a
// header with the field repeated means the addresses of all of them. A
// field that does not parse is an error.
func addressFields(h gomail.Header, key string) ([]*gomail.Address, error) {
	var out []*gomail.Address
	for _, v := range h.Values(key) {
		if strings.TrimSpace(v) == "" {
			continue
		}
		list, err := gomail.ParseAddressList(v)
		if err != nil {
			return nil, err
		}
		out = append(out, list...)
	}
	return out, nil
}

// uniqueFold keeps the first of addresses that differ only in case.
func uniqueFold(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range list {
		k := strings.ToLower(a)
		if !seen[k] {
			seen[k] = true
			out = append(out, a)
		}
	}
	return out
}

// errNoUploadHeader is an upload that does not start with its header.
var errNoUploadHeader = &argError{
	msg:  "an upload starts with its header",
	hint: "send the header (account, filename, content type) first, then the data",
}

// errSecondHeader is an upload with a header after the first part.
var errSecondHeader = &argError{msg: "an upload has one header", hint: "send the header once, first"}

// UploadAttachment implements chainmailv1connect.ComposeServiceHandler.
func (c *composeService) UploadAttachment(ctx context.Context, stream *connect.ClientStream[chainmailv1.UploadAttachmentRequest]) (*connect.Response[chainmailv1.UploadAttachmentResponse], error) {
	cl, err := callerOf(ctx, stream.RequestHeader())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, streamTimeout)
	defer cancel()
	entry := audit.Entry{Action: "upload"}
	info, upErr := c.upload(ctx, stream, &entry)
	if err := c.record(ctx, cl, entry, upErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.UploadAttachmentResponse{
		Handle: info.Handle,
		Sha256: hex.EncodeToString(info.SHA256[:]),
		Size:   info.Size,
	}), nil
}

// upload stores one upload stream. Its cap is the account's message size limit.
func (c *composeService) upload(ctx context.Context, stream *connect.ClientStream[chainmailv1.UploadAttachmentRequest], entry *audit.Entry) (upload.Info, error) {
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return upload.Info{}, err
		}
		return upload.Info{}, errNoUploadHeader
	}
	h := stream.Msg().GetHeader()
	if h == nil {
		return upload.Info{}, errNoUploadHeader
	}
	target, err := accountTarget(h.GetAccount())
	entry.Account = target
	if err != nil {
		return upload.Info{}, err
	}
	acct, err := c.account(target)
	if err != nil {
		return upload.Info{}, err
	}
	if err := checkUploadHeader(h); err != nil {
		return upload.Info{}, err
	}
	if c.s.uploads == nil {
		return upload.Info{}, errors.New("the daemon has no state directory for uploads")
	}
	info, err := c.s.uploads.Put(ctx, h.GetFilename(), h.GetContentType(), &uploadReader{stream: stream}, acct.MaxMessageSize)
	if err != nil {
		return upload.Info{}, err
	}
	c.own(info.Handle, acct.Name)
	entry.Targets = []string{info.Handle, tally("bytes", int(info.Size))}
	return info, nil
}

// checkUploadHeader checks the filename and declared media type.
func checkUploadHeader(h *chainmailv1.UploadHeader) error {
	name := h.GetFilename()
	if name == "" || len(name) > maxFilenameBytes || !utf8.ValidString(name) || strings.ContainsFunc(name, isControl) {
		return &argError{
			msg:  "the filename must be 1 to 255 bytes of UTF-8 without control characters",
			hint: "pass the file's base name",
		}
	}
	if ct := h.GetContentType(); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if typ, sub, ok := strings.Cut(mt, "/"); err != nil || !ok || typ == "" || sub == "" {
			return &argError{msg: "the content type is not a type/subtype", hint: "pass a media type such as application/pdf, or leave it empty"}
		}
	}
	return nil
}

// uploadReader reads the data parts of an upload stream after its header.
type uploadReader struct {
	stream *connect.ClientStream[chainmailv1.UploadAttachmentRequest]
	buf    []byte
}

func (r *uploadReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if !r.stream.Receive() {
			if err := r.stream.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		data, ok := r.stream.Msg().GetPart().(*chainmailv1.UploadAttachmentRequest_Data)
		if !ok {
			return 0, errSecondHeader
		}
		r.buf = data.Data
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
