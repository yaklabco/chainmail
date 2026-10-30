package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	gomail "github.com/emersion/go-message/mail"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/compose"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// gmailDraftLabel is the Gmail label of a draft.
const gmailDraftLabel = `\Draft`

// errNotDraft is a draft call on a message that is not a draft.
var errNotDraft = &argError{
	msg:  "the ref is not a draft in the Drafts folder",
	hint: "pass the ref CreateDraft or UpdateDraft returned, or one found in the Drafts folder",
}

// CreateDraft implements chainmailv1connect.ComposeServiceHandler. It is
// Send with draft set.
func (c *composeService) CreateDraft(ctx context.Context, req *connect.Request[chainmailv1.CreateDraftRequest]) (*connect.Response[chainmailv1.CreateDraftResponse], error) {
	cl, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, composeTimeout)
	defer cancel()
	res, err := c.send(ctx, cl, &chainmailv1.SendRequest{Spec: req.Msg.GetSpec(), Draft: true})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.CreateDraftResponse{Draft: res.GetDraft()}), nil
}

// UpdateDraft implements chainmailv1connect.ComposeServiceHandler.
func (c *composeService) UpdateDraft(ctx context.Context, req *connect.Request[chainmailv1.UpdateDraftRequest]) (*connect.Response[chainmailv1.UpdateDraftResponse], error) {
	cl, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, composeTimeout)
	defer cancel()
	entry := audit.Entry{Action: "draft.update"}
	d, err := c.updateDraft(ctx, req.Msg, &entry)
	if err := c.record(ctx, cl, entry, err); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.UpdateDraftResponse{Draft: d}), nil
}

// updateDraft appends the new version to Drafts, then expunges the old one.
// The new version keeps the old one's In-Reply-To and References, so a
// reply draft stays a reply.
func (c *composeService) updateDraft(ctx context.Context, req *chainmailv1.UpdateDraftRequest, entry *audit.Entry) (*chainmailv1.Draft, error) {
	old, acct, err := c.draftTarget(req.GetRef(), entry)
	if err != nil {
		return nil, err
	}
	p := &chainmailv1.ComposeSpec{}
	if s := req.GetSpec(); s != nil {
		p = proto.Clone(s).(*chainmailv1.ComposeSpec)
	}
	switch p.GetAccount() {
	case "":
		p.Account = acct.Name
	case acct.Name:
	default:
		return nil, &argError{msg: "the spec's account is not the draft's account", hint: "leave the spec's account empty, or set it to the draft's"}
	}
	spec, err := c.specFromProto(ctx, p)
	if err == nil {
		err = checkSize(acct, inputSize(spec))
	}
	if err != nil {
		return nil, err
	}
	st := c.s.stores.get(acct)
	prev, err := fetchDraft(ctx, st, acct, old)
	if err != nil {
		return nil, err
	}
	spec.InReplyTo = validMsgID(mailstore.MessageIDs(prev.InReplyTo))
	if refs := validMsgIDs(mailstore.MessageIDs(prev.References)); len(refs) > 0 {
		spec.References = refs[max(len(refs)-referencesKept, 0):]
	}
	build := withBcc(func(context.Context) (compose.Message, error) { return compose.Build(spec) }, spec.Bcc)
	msg, err := build(ctx)
	if err == nil {
		err = checkSize(acct, int64(len(msg.Raw)))
	}
	if err != nil {
		return nil, err
	}
	d, err := c.saveDraft(ctx, acct, msg)
	if err != nil {
		return nil, err
	}
	entry.Targets = appendTargets(entry.Targets, d.GetRef(), msg.MessageID)
	if err := expungeOne(ctx, st, old); err != nil {
		c.s.opts.Logger.Warn("removing the old version of a draft", "account", acct.Name, "ref", old.String(), "err", err)
		cerr := composeErr(err)
		return nil, api.New(cerr.Code(), api.Detail(cerr).GetReason(),
			newVersionHint(d)+"; delete the old one with DeleteDraft",
			&fixedTextError{msg: "the new version was saved, but removing the old one failed", err: err})
	}
	return d, nil
}

// newVersionHint names the new version of a draft: its ref, or when the
// server stored it without saying where, its Message-ID to search Drafts for.
func newVersionHint(d *chainmailv1.Draft) string {
	if d.GetRef() != "" {
		return "the new version is " + d.GetRef()
	}
	return "search the Drafts folder for the new version, Message-ID <" + d.GetMessageId() + ">"
}

// appendTargets adds the non-empty values to audit targets.
func appendTargets(targets []string, values ...string) []string {
	for _, v := range values {
		if v != "" {
			targets = append(targets, v)
		}
	}
	return targets
}

// SendDraft implements chainmailv1connect.ComposeServiceHandler.
func (c *composeService) SendDraft(ctx context.Context, req *connect.Request[chainmailv1.SendDraftRequest]) (*connect.Response[chainmailv1.SendDraftResponse], error) {
	cl, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, composeTimeout)
	defer cancel()
	res, err := c.sendDraft(ctx, cl, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.SendDraftResponse{Result: res}), nil
}

// sendDraft sends a draft as SendRaw sends bytes, dated now and keeping
// its Message-ID, then expunges the draft. A key that is done, failed or
// unknown is replayed before the draft is read or the request hashed, so
// a repeat returns the first outcome even with another ref. The draft is
// read inside build, after the key is claimed.
func (c *composeService) sendDraft(ctx context.Context, cl caller, req *chainmailv1.SendDraftRequest) (*chainmailv1.ComposeResult, error) {
	const action = "draft.send"
	entry := audit.Entry{Action: action}
	r, acct, err := c.draftTarget(req.GetRef(), &entry)
	if err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	if res, ok, err := c.replaySettled(ctx, cl, acct, req.GetIdempotencyKey(), entry); ok {
		return res, err
	}
	hash, err := requestHash(req, nil)
	if err != nil {
		return nil, c.record(ctx, cl, entry, err)
	}
	st := c.s.stores.get(acct)
	// draft is r with the X-GM-MSGID build reads, so on Google the draft is
	// known in All Mail too. build sets it before the Sent copy is looked up.
	draft := r
	build := func(ctx context.Context) (compose.Message, error) {
		prev, err := fetchDraft(ctx, st, acct, r)
		if err != nil {
			return compose.Message{}, err
		}
		if id := prev.Summary.Ref.GmailMsgID; id != 0 {
			draft.GmailMsgID = id
		}
		_, raw, err := readPart(ctx, st, r, "", acct.MaxMessageSize)
		if errors.Is(err, errPartTooLarge) {
			return compose.Message{}, checkSize(acct, acct.MaxMessageSize+1)
		}
		if err != nil {
			return compose.Message{}, err
		}
		raw, err = editHeader(raw, func(h *gomail.Header) { h.SetDate(c.s.opts.Now().Local()) })
		if err != nil {
			return compose.Message{}, err
		}
		return prepareRaw(acct, raw, nil)
	}
	after := func(ctx context.Context) {
		if err := expungeOne(ctx, st, r); err != nil {
			c.s.opts.Logger.Warn("removing a sent draft", "account", acct.Name, "ref", r.String(), "err", err)
		}
	}
	return c.run(ctx, sendJob{
		acct: acct, key: req.GetIdempotencyKey(), hash: hash, build: build, after: after,
		action: action, targets: []string{r.String()}, caller: cl,
		notSentCopy: func() ref.Ref { return draft },
	})
}

// DeleteDraft implements chainmailv1connect.ComposeServiceHandler.
func (c *composeService) DeleteDraft(ctx context.Context, req *connect.Request[chainmailv1.DeleteDraftRequest]) (*connect.Response[chainmailv1.DeleteDraftResponse], error) {
	cl, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, composeTimeout)
	defer cancel()
	entry := audit.Entry{Action: "draft.delete"}
	err = c.deleteDraft(ctx, req.Msg, &entry)
	if err := c.record(ctx, cl, entry, err); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.DeleteDraftResponse{}), nil
}

func (c *composeService) deleteDraft(ctx context.Context, req *chainmailv1.DeleteDraftRequest, entry *audit.Entry) error {
	r, acct, err := c.draftTarget(req.GetRef(), entry)
	if err != nil {
		return err
	}
	st := c.s.stores.get(acct)
	if _, err := fetchDraft(ctx, st, acct, r); err != nil {
		return err
	}
	return expungeOne(ctx, st, r)
}

// draftTarget parses a draft ref and resolves its account, recording both
// in entry.
func (c *composeService) draftTarget(s string, entry *audit.Entry) (ref.Ref, config.ResolvedAccount, error) {
	r, err := ref.Parse(s)
	if err != nil {
		entry.Targets = []string{invalidTarget}
		return ref.Ref{}, config.ResolvedAccount{}, err
	}
	entry.Account, entry.Targets = r.Account, []string{r.String()}
	acct, err := c.account(r.Account)
	return r, acct, err
}

// fetchDraft reads r and checks that it is a draft: in the Drafts folder,
// or on Google in All Mail with the Drafts label, as Gmail keeps drafts.
// Anything else is errNotDraft.
func fetchDraft(ctx context.Context, st mailstore.Store, acct config.ResolvedAccount, r ref.Ref) (*mailstore.Message, error) {
	folders, err := st.ListFolders(ctx)
	if err != nil {
		return nil, err
	}
	role := func(name string) mailstore.Role {
		for _, f := range folders {
			if f.Name == name {
				return f.Role
			}
		}
		return mailstore.RoleNone
	}
	inDrafts := role(r.Folder) == mailstore.RoleDrafts
	if !inDrafts && (!acct.Provider.Gmail || role(r.Folder) != mailstore.RoleAll) {
		return nil, errNotDraft
	}
	res := st.FetchMessages(ctx, []ref.Ref{r})
	if len(res) != 1 {
		return nil, fmt.Errorf("%w: the draft could not be read", mailstore.ErrUnavailable)
	}
	if res[0].Err != nil {
		return nil, res[0].Err
	}
	msg := res[0].Message
	if !inDrafts && !slices.ContainsFunc(msg.Summary.Labels, func(l string) bool { return strings.EqualFold(l, gmailDraftLabel) }) {
		return nil, errNotDraft
	}
	return msg, nil
}

// expungeOne permanently deletes r.
func expungeOne(ctx context.Context, st mailstore.Store, r ref.Ref) error {
	res := st.Expunge(ctx, []ref.Ref{r})
	if len(res) != 1 {
		return fmt.Errorf("%w: the expunge gave no result", mailstore.ErrUnavailable)
	}
	return res[0].Err
}
