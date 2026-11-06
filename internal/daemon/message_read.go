package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"regexp"
	"slices"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/audit"
	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/render"
)

// partRE matches IMAP part numbers such as 2 or 1.2.
var partRE = regexp.MustCompile(`^[1-9][0-9]{0,3}(\.[1-9][0-9]{0,3}){0,15}$`)

// errTooLarge fails a Get item that would push the response past maxResponseBytes.
var errTooLarge = &argError{
	msg:  "the response would be larger than 16 MiB",
	hint: "response too large: fetch fewer refs, or use get --raw for one message",
}

// defaultContentType stands in for a sender's content type that is not a
// type/subtype pair.
const defaultContentType = "application/octet-stream"

// mediaType reduces a sender's content type to a lowercase type/subtype pair.
func mediaType(s string) string {
	mt, _, err := mime.ParseMediaType(s)
	if err != nil && !errors.Is(err, mime.ErrInvalidMediaParameter) {
		return defaultContentType
	}
	typ, sub, ok := strings.Cut(mt, "/")
	if !ok || typ == "" || sub == "" {
		return defaultContentType
	}
	return mt
}

// itemError is the API form of a failure of one item. Its message is
// messageErr's fixed text.
func itemError(err error) *chainmailv1.ItemError {
	cerr := toConnectError(messageErr(err))
	d := api.Detail(cerr)
	return &chainmailv1.ItemError{Reason: d.GetReason(), Message: cerr.Message(), Hint: d.GetHint()}
}

// errUnparsable is a render failure, reason INTERNAL. Parse errors quote the
// bytes they choke on, so the render error itself is dropped.
var errUnparsable = errors.New("the message could not be parsed")

// apiReason returns the API reason err maps to.
func apiReason(err error) string { return api.Detail(toConnectError(err)).GetReason() }

// clientText is the whole message a client sees for a failure of one
// message, of one search source, or of a stream, by reason. A store's error
// can carry the mail server's text or a decoder's, and either can quote
// sender bytes, which are an injection channel, so none of it leaves the
// daemon. INVALID_ARGUMENT has no entry: the daemon writes those messages
// itself, from the request and never from message bytes. The hint, which
// the daemon also writes, is kept.
var clientText = map[string]string{
	api.ReasonStaleRef:          "the message is gone; search again",
	api.ReasonNotFound:          "not found",
	api.ReasonServerUnavailable: "the mail server failed while reading",
	api.ReasonMalformedResponse: "the mail server sent a response chainmail cannot read",
	api.ReasonTimeout:           "timed out",
	api.ReasonAuthRequired:      "the account needs authentication",
	api.ReasonAuthRejected:      "the server rejected the credentials",
	api.ReasonInternal:          "the message could not be read",
}

// fixedTextError reads as msg and unwraps to err, so err's reason still maps.
type fixedTextError struct {
	msg string
	err error
}

func (e *fixedTextError) Error() string { return e.msg }

func (e *fixedTextError) Unwrap() error { return e.err }

// messageErr returns the error to report for a failure of one message, of
// one search source, of a stream or of a whole MessageService call: err
// with its text replaced by clientText for its reason, or err itself for
// INVALID_ARGUMENT. Text the daemon already fixed, a fixedTextError in err's
// chain, is kept, and any text wrapped around it is dropped. Every such
// error passes through it before it reaches a client.
func messageErr(err error) error {
	if err == nil {
		return nil
	}
	if fixed, ok := errors.AsType[*fixedTextError](err); ok {
		return fixed
	}
	if nf, ok := errors.AsType[*mailstore.FolderNotFoundError](err); ok {
		return &fixedTextError{msg: folderMissingText(nf), err: err}
	}
	reason := apiReason(err)
	if reason == api.ReasonInvalidArgument {
		return err
	}
	msg, ok := clientText[reason]
	if !ok {
		msg = clientText[api.ReasonInternal]
	}
	return &fixedTextError{msg: msg, err: err}
}

// clientGone is the error for a failed send on a stream: the client went
// away, which maps to TIMEOUT like any other cancellation.
func clientGone() error { return fmt.Errorf("%w: client went away", context.Canceled) }

// callLevel reports whether a failure of one message means the whole call
// failed: the server or the login is down, or time ran out, so the other
// messages are no more readable than this one.
func callLevel(err error) bool {
	switch apiReason(err) {
	case api.ReasonServerUnavailable, api.ReasonTimeout, api.ReasonAuthRequired, api.ReasonAuthRejected:
		return true
	default:
		return false
	}
}

// gone reports whether err says the message went away since its ref
// was listed.
func gone(err error) bool {
	return errors.Is(err, mailstore.ErrNotFound) || errors.Is(err, mailstore.ErrStaleRef)
}

// renderMessage renders msg's body with the daemon's body_limit and returns
// the API form. msg may be shared with other items, so it is only read. A
// render error is always errUnparsable.
func (m *messageService) renderMessage(msg *mailstore.Message, full bool) (*chainmailv1.Message, error) {
	body, err := render.Render(msg.Raw, render.Options{Limit: m.s.opts.Config.Daemon.BodyLimitBytes(), Full: full})
	if err != nil {
		return nil, errUnparsable
	}
	return messageProto(msg, body), nil
}

// sameMessage reports whether two refs name one message: by Gmail message
// ID when both have one, since a Gmail message appears in several folders.
func sameMessage(a, b ref.Ref) bool {
	if a.GmailMsgID != 0 && b.GmailMsgID != 0 {
		return a.Account == b.Account && a.GmailMsgID == b.GmailMsgID
	}
	return a.Account == b.Account && a.Folder == b.Folder && a.UIDValidity == b.UIDValidity && a.UID == b.UID
}

// elemSize is the encoded size of an n-byte element of repeated field num.
func elemSize(num protowire.Number, n int) int {
	return protowire.SizeTag(num) + protowire.SizeBytes(n)
}

// fitPrefix returns the largest k such that base, the first k kept sizes and
// the dropped sizes from k on add up to at most limit, or 0 when none does.
// kept[i] is element i as returned and dropped[i] its stand-in once the cap
// cuts it off, which is small: an error item or a ref.
func fitPrefix(base int, kept, dropped []int, limit int) int {
	total := base
	for _, n := range dropped {
		total += n
	}
	k := 0
	for ; k < len(kept); k++ {
		next := total - dropped[k] + kept[k]
		if next > limit {
			break
		}
		total = next
	}
	return k
}

// Get implements chainmailv1connect.MessageServiceHandler.
func (m *messageService) Get(ctx context.Context, req *connect.Request[chainmailv1.GetRequest]) (*connect.Response[chainmailv1.GetResponse], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, callTimeout)
	defer cancel()
	resp, entry, getErr := m.get(ctx, req.Msg)
	if err := m.s.record(ctx, c, entry, messageErr(getErr)); err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (m *messageService) get(ctx context.Context, req *chainmailv1.GetRequest) (*chainmailv1.GetResponse, audit.Entry, error) {
	entry := audit.Entry{Action: "get"}
	refs := req.GetRefs()
	if len(refs) == 0 || len(refs) > maxGetRefs {
		return nil, entry, &argError{msg: fmt.Sprintf("get takes 1 to %d refs, got %d", maxGetRefs, len(refs)), hint: "split the refs into batches of 50"}
	}
	resp := &chainmailv1.GetResponse{FetchedAt: timestamppb.New(m.s.opts.Now()), Items: make([]*chainmailv1.GetItem, len(refs))}
	parsed := make([]ref.Ref, len(refs))
	byAccount := map[string][]int{}
	accts := map[string]config.ResolvedAccount{}
	for i, s := range refs {
		resp.Items[i] = &chainmailv1.GetItem{Ref: s}
		r, err := ref.Parse(s)
		if err != nil {
			resp.Items[i].Result = &chainmailv1.GetItem_Error{Error: itemError(err)}
			entry.Targets = append(entry.Targets, invalidTarget)
			continue
		}
		parsed[i] = r
		entry.Targets = append(entry.Targets, r.String())
		acct, err := m.s.opts.Config.Account(r.Account)
		if err != nil {
			resp.Items[i].Result = &chainmailv1.GetItem_Error{Error: itemError(err)}
			continue
		}
		accts[r.Account] = acct
		byAccount[r.Account] = append(byAccount[r.Account], i)
	}
	var wg sync.WaitGroup
	for name, idx := range byAccount {
		wg.Go(func() {
			rs := make([]ref.Ref, len(idx))
			for j, i := range idx {
				rs[j] = parsed[i]
			}
			results, sources := m.reader(accts[name]).FetchMessagesFrom(ctx, rs)
			for j, res := range results {
				m.fill(resp.Items[idx[j]], res, req.GetFull(), sources[j])
			}
		})
	}
	wg.Wait()
	capItems(resp)
	messages := 0
	var answered []string
	for _, it := range resp.Items {
		if msg := it.GetMessage(); msg != nil {
			messages++
			answered = append(answered, msg.GetSource())
		}
	}
	resp.Source = cache.CombineSource(answered...)
	if len(byAccount) == 1 {
		for name := range byAccount {
			entry.Account = name
		}
	}
	entry.Targets = append(entry.Targets, tally("messages", messages), tally("errors", len(refs)-messages), "source="+resp.GetSource())
	return resp, entry, nil
}

// fill sets item to res rendered, or to its error. source says where res
// came from, and the message carries it.
func (m *messageService) fill(item *chainmailv1.GetItem, res mailstore.MessageResult, full bool, source string) {
	if res.Err != nil {
		item.Result = &chainmailv1.GetItem_Error{Error: itemError(res.Err)}
		return
	}
	msg, err := m.renderMessage(res.Message, full)
	if err != nil {
		item.Result = &chainmailv1.GetItem_Error{Error: itemError(err)}
		return
	}
	msg.Source = source
	item.Result = &chainmailv1.GetItem_Message{Message: msg}
}

// capItems keeps resp within maxResponseBytes: the message item that would
// push it past the cap fails with errTooLarge, and so does every later
// message item, so a caller can retry from the first failed item on. An
// item that already failed keeps its own error, which is small.
func capItems(resp *chainmailv1.GetResponse) {
	tooLarge := func(it *chainmailv1.GetItem) *chainmailv1.GetItem {
		return &chainmailv1.GetItem{Ref: it.GetRef(), Result: &chainmailv1.GetItem_Error{Error: itemError(errTooLarge)}}
	}
	kept := make([]int, len(resp.Items))
	dropped := make([]int, len(resp.Items))
	for i, it := range resp.Items {
		kept[i] = elemSize(1, proto.Size(it))
		dropped[i] = kept[i]
		if it.GetMessage() != nil {
			dropped[i] = elemSize(1, proto.Size(tooLarge(it)))
		}
	}
	items := resp.Items
	resp.Items = nil
	k := fitPrefix(sizeWithSource(resp, &resp.Source), kept, dropped, maxResponseBytes)
	for i := k; i < len(items); i++ {
		if items[i].GetMessage() != nil {
			items[i] = tooLarge(items[i])
		}
	}
	resp.Items = items
}

// parseTarget parses a ref and resolves its account, recording the ref as
// the audit target, or invalidTarget when it does not parse.
func (m *messageService) parseTarget(s string, entry *audit.Entry) (ref.Ref, config.ResolvedAccount, error) {
	r, err := ref.Parse(s)
	if err != nil {
		entry.Targets = []string{invalidTarget}
		return ref.Ref{}, config.ResolvedAccount{}, err
	}
	entry.Account, entry.Targets = r.Account, []string{r.String()}
	acct, err := m.s.opts.Config.Account(r.Account)
	return r, acct, err
}

// GetThread implements chainmailv1connect.MessageServiceHandler.
func (m *messageService) GetThread(ctx context.Context, req *connect.Request[chainmailv1.GetThreadRequest]) (*connect.Response[chainmailv1.GetThreadResponse], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, callTimeout)
	defer cancel()
	entry := audit.Entry{Action: "thread"}
	resp, threadErr := m.thread(ctx, req.Msg, &entry)
	if err := m.s.record(ctx, c, entry, messageErr(threadErr)); err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (m *messageService) thread(ctx context.Context, req *chainmailv1.GetThreadRequest, entry *audit.Entry) (*chainmailv1.GetThreadResponse, error) {
	requested, acct, err := m.parseTarget(req.GetRef(), entry)
	if err != nil {
		return nil, err
	}
	rd := m.reader(acct)
	// seed is the ref the thread was found from: requested, or where a
	// stale requested ref was recovered to. It picks the thread ID and
	// decides whether a vanished member fails the call; the audit keeps
	// requested.
	refs, seed, membership, err := rd.ThreadFrom(ctx, requested)
	if err != nil {
		return nil, err
	}
	// member is a message of the thread and where it was read from.
	type member struct {
		msg    *mailstore.Message
		source string
	}
	var members []member
	var unreadable []string
	var lost error
	results, sources := rd.FetchMessagesFrom(ctx, refs)
	for i, res := range results {
		if res.Err == nil {
			members = append(members, member{msg: res.Message, source: sources[i]})
			continue
		}
		err := messageErr(res.Err)
		switch {
		case callLevel(err):
			return nil, err
		case gone(err) && sameMessage(refs[i], seed):
			return nil, err
		case gone(err):
			lost = err // a member that went away since Thread is left out
		default:
			// A permanent failure of one message is hostile or broken
			// input: it is listed in omitted_refs rather than failing the
			// thread.
			unreadable = append(unreadable, refs[i].String())
		}
	}
	if len(members) == 0 && len(unreadable) == 0 && lost != nil {
		return nil, lost
	}
	slices.SortStableFunc(members, func(a, b member) int {
		if c := a.msg.Summary.Date.Compare(b.msg.Summary.Date); c != 0 {
			return c
		}
		return a.msg.Summary.InternalDate.Compare(b.msg.Summary.InternalDate)
	})
	resp := &chainmailv1.GetThreadResponse{FetchedAt: timestamppb.New(m.s.opts.Now())}
	rendered := make([]*chainmailv1.Message, 0, len(members))
	for _, mb := range members {
		if sameMessage(mb.msg.Summary.Ref, seed) || resp.ThreadId == "" {
			resp.ThreadId = mb.msg.Summary.ThreadID
		}
		pm, err := m.renderMessage(mb.msg, req.GetFull())
		if err != nil {
			unreadable = append(unreadable, mb.msg.Summary.Ref.String())
			continue
		}
		pm.Source = mb.source
		rendered = append(rendered, pm)
	}
	capThread(resp, rendered, unreadable)
	// The thread's source covers its membership and every message it returns.
	answered := []string{membership}
	for _, pm := range resp.Messages {
		answered = append(answered, pm.GetSource())
	}
	resp.Source = cache.CombineSource(answered...)
	entry.Targets = append(entry.Targets, tally("messages", len(resp.Messages)))
	if resp.Truncated {
		entry.Targets = append(entry.Targets, tally("omitted", len(resp.OmittedRefs)))
	}
	entry.Targets = append(entry.Targets, "source="+resp.GetSource())
	return resp, nil
}

// capThread sets resp.Messages to the oldest of msgs that fit in
// maxResponseBytes, and OmittedRefs to the refs of the rest, oldest first,
// followed by unreadable, the refs of members that could not be read. It
// sets Truncated when any ref is omitted, so the caller can Get them.
func capThread(resp *chainmailv1.GetThreadResponse, msgs []*chainmailv1.Message, unreadable []string) {
	kept := make([]int, len(msgs))
	dropped := make([]int, len(msgs))
	for i, pm := range msgs {
		kept[i] = elemSize(2, proto.Size(pm))
		dropped[i] = elemSize(6, len(pm.GetRef()))
	}
	// Both are counted in the base, so a cut response still fits.
	resp.Truncated, resp.OmittedRefs = true, unreadable
	k := fitPrefix(sizeWithSource(resp, &resp.Source), kept, dropped, maxResponseBytes)
	resp.Messages, resp.OmittedRefs = msgs[:k], nil
	for _, pm := range msgs[k:] {
		resp.OmittedRefs = append(resp.OmittedRefs, pm.GetRef())
	}
	resp.OmittedRefs = append(resp.OmittedRefs, unreadable...)
	resp.Truncated = len(resp.OmittedRefs) > 0
}

// GetRaw implements chainmailv1connect.MessageServiceHandler.
func (m *messageService) GetRaw(ctx context.Context, req *connect.Request[chainmailv1.GetRawRequest], stream *connect.ServerStream[chainmailv1.GetRawResponse]) error {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return err
	}
	ctx, cancel := withDeadline(ctx, streamTimeout)
	defer cancel()
	entry := audit.Entry{Action: "get.raw"}
	sent, rawErr := m.raw(ctx, req.Msg.GetRef(), &entry, stream)
	entry.Targets = append(entry.Targets, tally("bytes", sent))
	return m.s.record(ctx, c, entry, messageErr(rawErr))
}

func (m *messageService) raw(ctx context.Context, s string, entry *audit.Entry, stream *connect.ServerStream[chainmailv1.GetRawResponse]) (int, error) {
	r, acct, err := m.parseTarget(s, entry)
	if err != nil {
		return 0, err
	}
	_, rc, err := m.store(acct).StreamPart(ctx, r, "")
	if err != nil {
		return 0, err
	}
	defer func() { _ = rc.Close() }() // the store's reader only returns its connection
	return pump(rc, func(b []byte) error { return stream.Send(&chainmailv1.GetRawResponse{Data: b}) })
}

// DownloadAttachment implements chainmailv1connect.MessageServiceHandler.
func (m *messageService) DownloadAttachment(ctx context.Context, req *connect.Request[chainmailv1.DownloadAttachmentRequest], stream *connect.ServerStream[chainmailv1.DownloadAttachmentResponse]) error {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return err
	}
	ctx, cancel := withDeadline(ctx, streamTimeout)
	defer cancel()
	entry := audit.Entry{Action: "attachment"}
	sent, dlErr := m.download(ctx, req.Msg, &entry, stream)
	entry.Targets = append(entry.Targets, tally("bytes", sent))
	return m.s.record(ctx, c, entry, messageErr(dlErr))
}

func (m *messageService) download(ctx context.Context, req *chainmailv1.DownloadAttachmentRequest, entry *audit.Entry, stream *connect.ServerStream[chainmailv1.DownloadAttachmentResponse]) (int, error) {
	r, acct, err := m.parseTarget(req.GetRef(), entry)
	if err != nil {
		return 0, err
	}
	part := req.GetPart()
	if !partRE.MatchString(part) {
		entry.Targets = append(entry.Targets, "part="+invalidTarget)
		return 0, &argError{msg: "part must look like 2 or 1.2", hint: "take the part from the attachments of: chainmail get <ref>"}
	}
	entry.Targets = append(entry.Targets, "part="+part)
	info, rc, err := m.store(acct).StreamPart(ctx, r, part)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rc.Close() }() // as in raw
	header := &chainmailv1.AttachmentHeader{Filename: untrusted(info.Filename), ContentType: mediaType(info.ContentType), Size: info.Size}
	if err := stream.Send(&chainmailv1.DownloadAttachmentResponse{Chunk: &chainmailv1.DownloadAttachmentResponse_Header{Header: header}}); err != nil {
		return 0, clientGone()
	}
	return pump(rc, func(b []byte) error {
		return stream.Send(&chainmailv1.DownloadAttachmentResponse{Chunk: &chainmailv1.DownloadAttachmentResponse_Data{Data: b}})
	})
}

// pump copies r to send in chunks of up to chunkBytes and returns the bytes
// sent. Bytes read before an error are still sent, and a failed send means
// the client went away. A read error after bytes went out is never
// NOT_FOUND or STALE_REF: the client holds part of the data, and the stream
// is never resumed, since the store never fetches bytes again. A deadline
// or a cancellation stays TIMEOUT, a part that does not decode stays
// INTERNAL, since no retry reads it, and anything else is
// SERVER_UNAVAILABLE.
func pump(r io.Reader, send func([]byte) error) (int, error) {
	buf := make([]byte, chunkBytes)
	total, fill := 0, 0
	for {
		n, err := r.Read(buf[fill:])
		fill += n
		if fill == len(buf) || (err != nil && fill > 0) {
			if send(slices.Clone(buf[:fill])) != nil {
				return total, clientGone()
			}
			total += fill
			fill = 0
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF):
			return total, nil
		case total == 0:
			return total, err
		case errors.Is(err, context.DeadlineExceeded):
			return total, fmt.Errorf("the stream stopped after %d bytes: %w", total, context.DeadlineExceeded)
		case errors.Is(err, context.Canceled):
			return total, fmt.Errorf("the stream stopped after %d bytes: %w", total, context.Canceled)
		case apiReason(err) == api.ReasonInternal:
			return total, err
		default:
			return total, fmt.Errorf("%w: the stream broke after %d bytes", mailstore.ErrUnavailable, total)
		}
	}
}

// longestSource is the longest value a get or thread response's source
// takes. The source is set once the response is capped, so the caps count
// this many bytes for it.
const longestSource = cache.SourceServer

// sizeWithSource is the encoded size of resp with *source set to
// longestSource, leaving *source as it was.
func sizeWithSource(resp proto.Message, source *string) int {
	was := *source
	*source = longestSource
	defer func() { *source = was }()
	return proto.Size(resp)
}
