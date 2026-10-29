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

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/render"
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

func itemError(err error) *chainmailv1.ItemError {
	cerr := toConnectError(err)
	d := api.Detail(cerr)
	return &chainmailv1.ItemError{Reason: d.GetReason(), Message: cerr.Message(), Hint: d.GetHint()}
}

// renderMessage renders msg's body with the daemon's body_limit and returns
// the API form. msg may be shared with other items, so it is only read.
func (m *messageService) renderMessage(msg *mailstore.Message, full bool) (*chainmailv1.Message, error) {
	body, err := render.Render(msg.Raw, render.Options{Limit: m.s.opts.Config.Daemon.BodyLimitBytes(), Full: full})
	if err != nil {
		return nil, fmt.Errorf("rendering %s: %w", msg.Summary.Ref, err)
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
	if err := m.s.record(ctx, c, entry, getErr); err != nil {
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
	resp := &chainmailv1.GetResponse{Source: sourceServer, FetchedAt: timestamppb.New(m.s.opts.Now()), Items: make([]*chainmailv1.GetItem, len(refs))}
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
			for j, res := range m.store(accts[name]).FetchMessages(ctx, rs) {
				m.fill(resp.Items[idx[j]], res, req.GetFull())
			}
		})
	}
	wg.Wait()
	capItems(resp)
	messages := 0
	for _, it := range resp.Items {
		if it.GetMessage() != nil {
			messages++
		}
	}
	if len(byAccount) == 1 {
		for name := range byAccount {
			entry.Account = name
		}
	}
	entry.Targets = append(entry.Targets, tally("messages", messages), tally("errors", len(refs)-messages))
	return resp, entry, nil
}

func (m *messageService) fill(item *chainmailv1.GetItem, res mailstore.MessageResult, full bool) {
	if res.Err != nil {
		item.Result = &chainmailv1.GetItem_Error{Error: itemError(res.Err)}
		return
	}
	msg, err := m.renderMessage(res.Message, full)
	if err != nil {
		item.Result = &chainmailv1.GetItem_Error{Error: itemError(err)}
		return
	}
	item.Result = &chainmailv1.GetItem_Message{Message: msg}
}

// capItems keeps resp within maxResponseBytes: the item that would push it
// past the cap fails with errTooLarge, and so does every later one, so a
// caller can retry from the first failed item on.
func capItems(resp *chainmailv1.GetResponse) {
	tooLarge := func(it *chainmailv1.GetItem) *chainmailv1.GetItem {
		return &chainmailv1.GetItem{Ref: it.GetRef(), Result: &chainmailv1.GetItem_Error{Error: itemError(errTooLarge)}}
	}
	kept := make([]int, len(resp.Items))
	dropped := make([]int, len(resp.Items))
	for i, it := range resp.Items {
		kept[i] = elemSize(1, proto.Size(it))
		dropped[i] = elemSize(1, proto.Size(tooLarge(it)))
	}
	items := resp.Items
	resp.Items = nil
	k := fitPrefix(proto.Size(resp), kept, dropped, maxResponseBytes)
	for i := k; i < len(items); i++ {
		items[i] = tooLarge(items[i])
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
	if err := m.s.record(ctx, c, entry, threadErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (m *messageService) thread(ctx context.Context, req *chainmailv1.GetThreadRequest, entry *audit.Entry) (*chainmailv1.GetThreadResponse, error) {
	seed, acct, err := m.parseTarget(req.GetRef(), entry)
	if err != nil {
		return nil, err
	}
	st := m.store(acct)
	refs, err := st.Thread(ctx, seed)
	if err != nil {
		return nil, err
	}
	var msgs []*mailstore.Message
	var vanished error
	for i, res := range st.FetchMessages(ctx, refs) {
		switch {
		case res.Err == nil:
			msgs = append(msgs, res.Message)
		case sameMessage(refs[i], seed):
			return nil, res.Err
		case errors.Is(res.Err, mailstore.ErrNotFound), errors.Is(res.Err, mailstore.ErrStaleRef):
			vanished = res.Err // a member that went away since Thread is left out
		default:
			return nil, res.Err
		}
	}
	if len(msgs) == 0 && vanished != nil {
		return nil, vanished
	}
	slices.SortStableFunc(msgs, func(a, b *mailstore.Message) int {
		if c := a.Summary.Date.Compare(b.Summary.Date); c != 0 {
			return c
		}
		return a.Summary.InternalDate.Compare(b.Summary.InternalDate)
	})
	resp := &chainmailv1.GetThreadResponse{Source: sourceServer, FetchedAt: timestamppb.New(m.s.opts.Now())}
	rendered := make([]*chainmailv1.Message, len(msgs))
	for i, msg := range msgs {
		if rendered[i], err = m.renderMessage(msg, req.GetFull()); err != nil {
			return nil, err
		}
		if sameMessage(msg.Summary.Ref, seed) || resp.ThreadId == "" {
			resp.ThreadId = msg.Summary.ThreadID
		}
	}
	capThread(resp, rendered)
	entry.Targets = append(entry.Targets, tally("messages", len(resp.Messages)))
	if resp.Truncated {
		entry.Targets = append(entry.Targets, tally("omitted", len(resp.OmittedRefs)))
	}
	return resp, nil
}

// capThread sets resp.Messages to the oldest of msgs that fit in
// maxResponseBytes. When some do not, it sets Truncated and lists their refs
// in OmittedRefs, oldest first, so the caller can Get them.
func capThread(resp *chainmailv1.GetThreadResponse, msgs []*chainmailv1.Message) {
	kept := make([]int, len(msgs))
	dropped := make([]int, len(msgs))
	for i, pm := range msgs {
		kept[i] = elemSize(2, proto.Size(pm))
		dropped[i] = elemSize(6, len(pm.GetRef()))
	}
	resp.Truncated = true // counted in the base, so a cut response still fits
	k := fitPrefix(proto.Size(resp), kept, dropped, maxResponseBytes)
	resp.Messages = msgs[:k]
	resp.Truncated = k < len(msgs)
	for _, pm := range msgs[k:] {
		resp.OmittedRefs = append(resp.OmittedRefs, pm.GetRef())
	}
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
	return m.s.record(ctx, c, entry, rawErr)
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
	return m.s.record(ctx, c, entry, dlErr)
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
		return 0, err
	}
	return pump(rc, func(b []byte) error {
		return stream.Send(&chainmailv1.DownloadAttachmentResponse{Chunk: &chainmailv1.DownloadAttachmentResponse_Data{Data: b}})
	})
}

// pump copies r to send in chunks of up to chunkBytes and returns the bytes
// sent. Bytes read before an error are still sent. A read error after bytes
// went out is ErrUnavailable whatever its cause: the client holds part of
// the data, and the store never fetches it again.
func pump(r io.Reader, send func([]byte) error) (int, error) {
	buf := make([]byte, chunkBytes)
	total, fill := 0, 0
	for {
		n, err := r.Read(buf[fill:])
		fill += n
		if fill == len(buf) || (err != nil && fill > 0) {
			if serr := send(slices.Clone(buf[:fill])); serr != nil {
				return total, serr
			}
			total += fill
			fill = 0
		}
		switch {
		case errors.Is(err, io.EOF):
			return total, nil
		case err != nil && total > 0 && !errors.Is(err, mailstore.ErrUnavailable):
			return total, fmt.Errorf("%w: the stream broke after %d bytes: %s", mailstore.ErrUnavailable, total, err.Error())
		case err != nil:
			return total, err
		}
	}
}
