package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/compose"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/idempotency"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/policy"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/sender"
	"github.com/jamesainslie/chainmail/internal/upload"
	"github.com/jamesainslie/chainmail/internal/version"
)

const (
	// uploadTTL is how long an upload lives.
	uploadTTL = time.Hour
	// recoverTimeout bounds the startup pass that settles sends an earlier
	// daemon left in progress.
	recoverTimeout = 10 * time.Second
	// composeTimeout is the deadline of a ComposeService call whose client
	// set none. Once a message is handed to SMTP, the send and what follows
	// it run to the end whatever the deadline.
	composeTimeout = 2 * time.Minute
	// outcomeTries and defaultOutcomeInterval make the search for a send
	// whose outcome is unknown: 3 tries over 10 seconds.
	outcomeTries           = 3
	defaultOutcomeInterval = 5 * time.Second
	// afterSendTimeout bounds the Sent copy, the search and the after hook,
	// which outlive the client.
	afterSendTimeout = time.Minute
	// finishTimeout bounds the write that settles an idempotency key.
	finishTimeout = 5 * time.Second
	// flagSeen and flagDraft are the IMAP flags of a Sent copy and a draft.
	flagSeen  = `\Seen`
	flagDraft = `\Draft`
	// The decision column of the audit log. decisionAllowed is a transmit
	// the send policy passed, decisionHeld a send the policy held as a
	// draft, and decisionDenied a refusal of a destructive action.
	decisionDuplicate = "duplicate"
	decisionDryRun    = "dry_run"
	decisionAllowed   = "allowed"
	decisionHeld      = "held"
	decisionDenied    = "denied"
)

// sentCopyFailed is Sent.sent_copy_error when the copy could not be stored.
const sentCopyFailed = "the message was sent, but storing a copy in the Sent folder failed"

// openSendState opens the idempotency and upload stores.
func (s *Server) openSendState() error {
	s.idem = idempotency.New(s.opts.StateDB, s.opts.Now)
	s.idem.Logger = s.opts.Logger
	if s.opts.StateDir == "" {
		return nil
	}
	var err error
	s.uploads, err = upload.New(filepath.Join(s.opts.StateDir, "uploads"), uploadTTL, s.opts.Now)
	return err
}

// startBackground runs the idempotency pruner and the upload sweeper until
// ctx ends or the returned stop is called. stop waits for both to return.
func (s *Server) startBackground(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { s.idem.Run(ctx) })
	if s.uploads != nil {
		wg.Go(func() { s.uploads.Run(ctx) })
	}
	return func() {
		cancel()
		wg.Wait()
	}
}

// dialFunc opens an authenticated SMTP connection. Tests replace sender.Dial.
type dialFunc func(ctx context.Context, ep provider.Endpoint, creds auth.Credentials, opts sender.DialOptions) (*sender.Conn, error)

// uploadOwner is the account an upload belongs to, and when it was stored.
type uploadOwner struct {
	account string
	at      time.Time
}

// sendJob is one send-type call after validation.
type sendJob struct {
	acct config.ResolvedAccount
	key  string
	hash [32]byte
	// build makes the message. It runs concurrently with dialing.
	build func(ctx context.Context) (compose.Message, error)
	// after runs once the server accepted the message, concurrently with
	// the Sent copy, such as setting \Answered. It logs its own failures:
	// they do not fail the send. Nil means nothing.
	after func(ctx context.Context)
	// action is the audit action.
	action string
	// targets are audit targets. The Message-ID is added to them.
	targets []string
	caller  caller
	// notSentCopy, when set, returns a message never taken for the Sent
	// copy. It is called after build. SendDraft returns the draft, which
	// has the sent message's Message-ID, with the X-GM-MSGID build read.
	notSentCopy func() ref.Ref
	// existingDraft, when set, is the draft a SendDraft sends. A hold names
	// it instead of saving a new draft.
	existingDraft *chainmailv1.Draft
}

// run executes spec 6.1: idempotency, dial || build, one Send, outcome
// search, Sent append (non-Google) || after, Finish, audit. It returns the
// result to send to the client.
func (c *composeService) run(ctx context.Context, job sendJob) (*chainmailv1.ComposeResult, error) {
	entry := audit.Entry{Account: job.acct.Name, Action: job.action, Targets: slices.Clone(job.targets)}
	res, err := c.claimAndSend(ctx, job, &entry)
	if id := res.GetSent().GetMessageId(); id != "" && !slices.Contains(entry.Targets, id) {
		entry.Targets = append(entry.Targets, id)
	}
	return res, c.record(ctx, job.caller, entry, err)
}

// claimAndSend claims the key, then sends or replays the stored outcome.
func (c *composeService) claimAndSend(ctx context.Context, job sendJob, entry *audit.Entry) (*chainmailv1.ComposeResult, error) {
	switch {
	case job.key == "":
		return nil, errKeyRequired
	case !idempotency.ValidKey(job.key):
		return nil, errKeyMalformed
	}
	claim, err := c.s.idem.Begin(ctx, job.acct.Name, job.key, job.hash)
	if errors.Is(err, idempotency.ErrKeyMismatch) {
		return nil, errKeyReused
	}
	if err != nil {
		return nil, err
	}
	if !claim.Proceed {
		entry.Decision = decisionDuplicate
		return replay(claim)
	}
	res, state, err := c.attempt(ctx, job, entry)
	c.finish(ctx, job, state, res, err)
	return res, err
}

// attempt performs one send on a claimed key. It returns the state to
// settle the key with.
func (c *composeService) attempt(ctx context.Context, job sendJob, entry *audit.Entry) (*chainmailv1.ComposeResult, idempotency.State, error) {
	msg, conn, err := c.prepare(ctx, job)
	var held *heldError
	if errors.As(err, &held) {
		return c.hold(ctx, job, held, entry)
	}
	if err != nil {
		return nil, idempotency.Retryable, err
	}
	entry.Decision = decisionAllowed
	entry.Targets = append(entry.Targets, msg.MessageID)

	// From here the message goes out in full or not at all, whether or not
	// the client is still there. The connection's own timeout bounds it.
	detached := context.WithoutCancel(ctx)
	sendErr := conn.Send(detached, msg.EnvelopeFrom, msg.EnvelopeTo, msg.Raw)
	sent := &chainmailv1.Sent{MessageId: msg.MessageID}
	if sendErr != nil {
		var se *sender.SendError
		if !errors.As(sendErr, &se) {
			se = &sender.SendError{Outcome: sender.NotSentTemporary, Err: sendErr}
		}
		// The error's text can quote the server, so only its outcome and
		// reply code are logged.
		c.s.opts.Logger.Debug("SMTP send failed", "account", job.acct.Name, "message_id", msg.MessageID,
			"outcome", se.Outcome.String(), "code", se.Code)
		switch se.Outcome {
		case sender.NotSentPermanent:
			return nil, idempotency.Failed, refusedErr(se)
		case sender.Unknown:
			found, ok := c.searchOutcome(detached, job.acct, msg.MessageID)
			if !ok {
				c.s.opts.Logger.Warn("send outcome unknown", "account", job.acct.Name, "message_id", msg.MessageID)
				return nil, idempotency.Unknown, outcomeUnknownErr(msg.MessageID)
			}
			sent.SentRef = found
		default:
			return nil, idempotency.Retryable, notSentErr(se)
		}
	}
	c.afterSend(detached, job, msg, sent)
	return &chainmailv1.ComposeResult{Result: &chainmailv1.ComposeResult_Sent{Sent: sent}}, idempotency.Done, nil
}

// heldHint is Held.hint, fixed text for the agent to relay.
const heldHint = "held by policy: saved to Drafts for James to review and send"

// heldError ends prepare when policy holds the built message. It is not
// a client error: attempt turns it into a held result.
type heldError struct {
	msg      compose.Message
	decision policy.SendDecision
}

func (e *heldError) Error() string { return "held by policy" }

// hold settles a send the policy held: it saves the built message to
// Drafts, or for SendDraft names the draft already there, and returns the
// held result. Nothing is sent. The audit row gets the draft ref, the
// Message-ID and the count of blocked recipients, never an address.
func (c *composeService) hold(ctx context.Context, job sendJob, held *heldError, entry *audit.Entry) (*chainmailv1.ComposeResult, idempotency.State, error) {
	var d *chainmailv1.Draft
	if job.existingDraft != nil {
		d = proto.Clone(job.existingDraft).(*chainmailv1.Draft)
		if d.GetMessageId() == "" {
			d.MessageId = held.msg.MessageID
		}
	} else {
		msg, err := heldDraft(held.msg)
		if err != nil {
			return nil, idempotency.Retryable, api.New(connect.CodeInternal, api.ReasonInternal, "",
				&fixedTextError{msg: "held by policy, but making the draft failed; nothing was sent", err: err})
		}
		if d, err = c.saveDraft(ctx, job.acct, msg); err != nil {
			return nil, idempotency.Retryable, api.New(connect.CodeUnavailable, api.ReasonServerUnavailable, "",
				&fixedTextError{msg: "held by policy, but saving the draft failed; nothing was sent", err: err})
		}
	}
	entry.Decision = decisionHeld
	entry.Reason = api.ReasonHeld
	if r := d.GetRef(); r != "" && !slices.Contains(entry.Targets, r) {
		entry.Targets = append(entry.Targets, r)
	}
	entry.Targets = append(entry.Targets, d.GetMessageId())
	if n := len(held.decision.Blocked); n > 0 {
		entry.Targets = append(entry.Targets, fmt.Sprintf("blocked:%d", n))
	}
	if held.decision.TooMany {
		entry.Targets = append(entry.Targets, "too_many")
	}
	return &chainmailv1.ComposeResult{Result: &chainmailv1.ComposeResult_Held{Held: &chainmailv1.Held{
		Draft: d, Blocked: held.decision.Blocked, TooManyRecipients: held.decision.TooMany, Hint: heldHint,
	}}}, idempotency.Done, nil
}

// heldDraft returns msg as a held send's draft: with a Bcc field listing
// every envelope recipient not in its To or Cc fields, so sending the
// draft from a mail client reaches them too. When there is none the
// message is returned as it is. Repeated To and Cc fields are all read, as
// prepareRaw reads them, and a field that does not parse counts as empty.
func heldDraft(msg compose.Message) (compose.Message, error) {
	th, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(msg.Raw)))
	if err != nil {
		return compose.Message{}, errRawUnparsable
	}
	h := gomail.Header{Header: message.Header{Header: th}}
	inHeader := map[string]bool{}
	for _, key := range []string{"To", "Cc"} {
		list, _ := addressFields(h, key) // an unparsable field leaves its recipients to Bcc
		for _, a := range list {
			inHeader[strings.ToLower(a.Address)] = true
		}
	}
	var extra []*gomail.Address
	for _, a := range msg.EnvelopeTo {
		if !inHeader[strings.ToLower(a)] {
			extra = append(extra, &gomail.Address{Address: a})
		}
	}
	if len(extra) == 0 {
		return msg, nil
	}
	msg.Raw, err = editHeader(msg.Raw, func(h *gomail.Header) { h.SetAddressList("Bcc", extra) })
	return msg, err
}

// prepare builds the message while it dials, and returns both. A build that
// fails, or that the send policy holds, cancels the dial. A dial failure
// does not cancel the build: the build decides first, so a hold still
// reaches Drafts while SMTP is down. The caller's ctx bounds both. Once
// both are done, the result is, in order: a hold, as a heldError carrying
// the built message; the build's error; the dial's error; the end of the
// caller's ctx. Any of these closes a connection that was made, so a
// cancelled call never reaches Send.
func (c *composeService) prepare(parent context.Context, job sendJob) (compose.Message, *sender.Conn, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var (
		msg      compose.Message
		buildErr error
		conn     *sender.Conn
		dialErr  error
		wg       sync.WaitGroup
	)
	wg.Go(func() {
		msg, buildErr = c.buildChecked(ctx, job)
		if buildErr != nil {
			cancel() // the connection will not be used
		}
	})
	wg.Go(func() {
		conn, dialErr = c.connect(ctx, job.acct)
	})
	wg.Wait()
	err := buildErr // a heldError is a build error, so it comes first
	if err == nil {
		err = dialErr
	}
	if err == nil {
		// The build does not watch ctx, so the caller may have gone while
		// it ran after a successful dial.
		err = parent.Err()
	}
	if err != nil {
		if conn != nil {
			if cerr := conn.Close(); cerr != nil {
				c.s.opts.Logger.Debug("closing an unused SMTP connection failed", "account", job.acct.Name)
			}
		}
		return compose.Message{}, nil, err
	}
	return msg, conn, nil
}

// buildChecked builds the message, checks its size and checks its
// recipients against the send policy. A held message fails with a
// heldError.
func (c *composeService) buildChecked(ctx context.Context, job sendJob) (compose.Message, error) {
	m, err := job.build(ctx)
	if err != nil {
		return compose.Message{}, err
	}
	if err := checkSize(job.acct, int64(len(m.Raw))); err != nil {
		return compose.Message{}, err
	}
	if d := c.s.policy.CheckSend(job.acct.Name, m.EnvelopeTo); !d.Allowed {
		return compose.Message{}, &heldError{msg: m, decision: d}
	}
	return m, nil
}

// connect fetches the account's credentials and dials its SMTP server. A
// refusal of the credentials parks the account as SP1 does.
func (c *composeService) connect(ctx context.Context, acct config.ResolvedAccount) (*sender.Conn, error) {
	creds, err := c.s.auth.Credentials(ctx, acct.Name)
	if err != nil {
		return nil, err
	}
	conn, err := c.dial(ctx, acct.SMTP, creds, sender.DialOptions{RootCAs: c.s.opts.RootCAs})
	if err == nil {
		return conn, nil
	}
	switch {
	case errors.Is(err, auth.ErrRejected):
		c.s.opts.Logger.Debug("SMTP dial failed", "account", acct.Name, "step", "auth", "outcome", "rejected")
		c.s.auth.Rejected(acct.Name, creds)
		hint := ""
		if st, serr := c.s.auth.State(acct.Name); serr == nil {
			hint = st.Detail
		}
		return nil, api.New(connect.CodeUnauthenticated, api.ReasonAuthRejected, hint,
			&fixedTextError{msg: "the server rejected the credentials; nothing was sent", err: err})
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		c.s.opts.Logger.Debug("SMTP dial failed", "account", acct.Name, "step", "dial", "outcome", "cancelled")
		return nil, err
	default:
		// The error can quote the server's reply, so it is not logged.
		c.s.opts.Logger.Debug("SMTP dial failed", "account", acct.Name, "step", "dial", "outcome", "unavailable")
		return nil, api.New(connect.CodeUnavailable, api.ReasonServerUnavailable, "",
			&fixedTextError{msg: "could not connect to the mail server; nothing was sent", err: err})
	}
}

// afterSend stores the Sent copy, or on Google finds the one Gmail filed,
// while job.after runs. sent gets the copy's ref or sent_copy_error.
func (c *composeService) afterSend(ctx context.Context, job sendJob, msg compose.Message, sent *chainmailv1.Sent) {
	ctx, cancel := context.WithTimeout(ctx, afterSendTimeout)
	defer cancel()
	var wg sync.WaitGroup
	if job.after != nil {
		wg.Go(func() { job.after(ctx) })
	}
	if sent.GetSentRef() == "" {
		var skip ref.Ref
		if job.notSentCopy != nil {
			skip = job.notSentCopy()
		}
		sent.SentRef, sent.SentCopyError = c.sentCopy(ctx, job.acct, msg, skip)
	}
	wg.Wait()
}

// sentCopy returns the ref of the Sent copy, or fixed text saying why there
// is none. Google files sent mail itself, so its copy is looked for once in
// All Mail, and not finding it yet is no error. Elsewhere the message is
// appended to the Sent folder, marked read. skip, when set, is a message
// never taken for the copy.
func (c *composeService) sentCopy(ctx context.Context, acct config.ResolvedAccount, msg compose.Message, skip ref.Ref) (sentRef, copyErr string) {
	st := c.s.stores.get(acct)
	if acct.Provider.Gmail {
		refs, err := findIn(ctx, st, mailstore.RoleAll, msg.MessageID)
		if err != nil {
			c.s.opts.Logger.Debug("looking up Gmail's Sent copy", "account", acct.Name, "reason", errReason(err))
		}
		if skip != (ref.Ref{}) {
			refs = c.without(ctx, st, acct, refs, skip)
		}
		if len(refs) == 0 {
			return "", ""
		}
		return refs[0].String(), ""
	}
	folder, err := roleFolder(ctx, st, mailstore.RoleSent)
	if err == nil {
		r, aerr := st.Append(ctx, folder, []string{flagSeen}, time.Time{}, msg.Raw)
		if aerr == nil {
			// Without APPENDUID the copy is stored but has no ref.
			return refString(r), ""
		}
		err = aerr
	}
	c.s.opts.Logger.Warn("storing the Sent copy failed", "account", acct.Name, "message_id", msg.MessageID, "reason", errReason(err))
	return "", sentCopyFailed
}

// without drops from refs the message skip names. On Google a message has
// one ref per folder it shows in, so a draft in Drafts and in All Mail has
// two, and the message is told by its X-GM-MSGID: refs without one get it
// from the server, and a ref whose X-GM-MSGID is still unknown is dropped
// too, as it may be skip.
func (c *composeService) without(ctx context.Context, st mailstore.Store, acct config.ResolvedAccount, refs []ref.Ref, skip ref.Ref) []ref.Ref {
	byID := acct.Provider.Gmail && skip.GmailMsgID != 0
	if byID {
		refs = c.withGmailIDs(ctx, st, acct, refs)
	}
	return slices.DeleteFunc(refs, func(r ref.Ref) bool {
		return sameMessage(r, skip) || (byID && r.GmailMsgID == 0)
	})
}

// withGmailIDs returns refs with the X-GM-MSGID of each that lacks one,
// read from the summaries of its folder. A ref whose summary cannot be read
// keeps 0.
func (c *composeService) withGmailIDs(ctx context.Context, st mailstore.Store, acct config.ResolvedAccount, refs []ref.Ref) []ref.Ref {
	out := slices.Clone(refs)
	groups := map[mailstore.Source][]uint32{}
	for _, r := range out {
		if r.GmailMsgID == 0 {
			src := mailstore.Source{Folder: r.Folder, UIDValidity: r.UIDValidity}
			groups[src] = append(groups[src], r.UID)
		}
	}
	for src, uids := range groups {
		sums, err := st.FetchSummaries(ctx, src, uids, mailstore.SummaryOptions{})
		if err != nil {
			c.s.opts.Logger.Debug("reading X-GM-MSGIDs", "account", acct.Name, "reason", errReason(err))
			continue
		}
		ids := make(map[uint32]uint64, len(sums))
		for _, s := range sums {
			ids[s.Ref.UID] = s.Ref.GmailMsgID
		}
		for i, r := range out {
			if r.GmailMsgID == 0 && r.Folder == src.Folder && r.UIDValidity == src.UIDValidity {
				out[i].GmailMsgID = ids[r.UID]
			}
		}
	}
	return out
}

// refString is r as a client sees it, or "" for the zero ref of a write
// the server confirmed without naming where the message went.
func refString(r ref.Ref) string {
	if r == (ref.Ref{}) {
		return ""
	}
	return r.String()
}

// searchOutcome looks for messageID in the Sent folder, up to outcomeTries
// times over outcomeInterval gaps, and returns the ref when found.
func (c *composeService) searchOutcome(ctx context.Context, acct config.ResolvedAccount, messageID string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, afterSendTimeout)
	defer cancel()
	st := c.s.stores.get(acct)
	for i := range c.outcomeTries {
		if i > 0 {
			t := time.NewTimer(c.outcomeInterval)
			select {
			case <-ctx.Done():
				t.Stop()
				return "", false
			case <-t.C:
			}
		}
		refs, err := findIn(ctx, st, mailstore.RoleSent, messageID)
		if err != nil {
			c.s.opts.Logger.Debug("searching Sent for a send of unknown outcome", "account", acct.Name, "reason", errReason(err))
			continue
		}
		if len(refs) > 0 {
			return refs[0].String(), true
		}
	}
	return "", false
}

// finish settles the key with state and the result to replay: the
// ComposeResult on success, else the ErrorDetail. A client hang-up must not
// leave the key in progress, so it ignores ctx's cancellation.
func (c *composeService) finish(ctx context.Context, job sendJob, state idempotency.State, res *chainmailv1.ComposeResult, sendErr error) {
	var stored proto.Message = res
	if sendErr != nil {
		stored = api.Detail(composeErr(sendErr))
	}
	b, err := proto.Marshal(stored)
	if err != nil {
		c.s.opts.Logger.Error("encoding a send result", "account", job.acct.Name, "err", err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if err := c.s.idem.Finish(ctx, job.acct.Name, job.key, state, b); err != nil {
		c.s.opts.Logger.Error("settling an idempotency key", "account", job.acct.Name, "state", string(state), "err", err)
	}
}

// replaySettled answers a keyed send-type call from the outcome stored for
// its key, before anything the request names is read: uploads, the
// original, the draft, or the account's size limit. A key that is done,
// failed or unknown is replayed as a duplicate without connecting, so a
// different request reusing such a key gets the first outcome too. ok is
// false when the key is unused, retryable or still in progress, or cannot
// be read: the call goes on, and Begin checks the request against the key
// and waits for an attempt still running in this process.
func (c *composeService) replaySettled(ctx context.Context, cl caller, acct config.ResolvedAccount, key string, entry audit.Entry) (*chainmailv1.ComposeResult, bool, error) {
	if !idempotency.ValidKey(key) {
		return nil, false, nil
	}
	row, found, err := c.s.idem.Lookup(ctx, acct.Name, key)
	if err != nil {
		c.s.opts.Logger.Warn("reading an idempotency key failed", "account", acct.Name, "err", err)
		return nil, false, nil
	}
	switch {
	case !found:
		return nil, false, nil
	case row.State == idempotency.Done, row.State == idempotency.Failed, row.State == idempotency.Unknown:
	default:
		return nil, false, nil
	}
	entry.Decision = decisionDuplicate
	res, err := replay(idempotency.Claim{State: row.State, Result: row.Result})
	if id := res.GetSent().GetMessageId(); id != "" {
		entry.Targets = append(entry.Targets, id)
	}
	return res, true, c.record(ctx, cl, entry, err)
}

// replay returns the outcome stored for a key that was not claimed.
func replay(claim idempotency.Claim) (*chainmailv1.ComposeResult, error) {
	switch claim.State {
	case idempotency.InProgress:
		return nil, errStillInProgress
	case idempotency.Done:
		res := &chainmailv1.ComposeResult{}
		if err := proto.Unmarshal(claim.Result, res); err != nil {
			return nil, fmt.Errorf("decoding the stored result: %w", err)
		}
		if s := res.GetSent(); s != nil {
			s.Duplicate = true
		}
		return res, nil
	}
	if len(claim.Result) == 0 {
		// An earlier daemon stopped mid-send and Recover settled the key.
		return nil, api.New(connect.CodeUnknown, api.ReasonSendOutcomeUnknown, outcomeHint(""), errors.New(replayText[api.ReasonSendOutcomeUnknown]))
	}
	d := &chainmailv1.ErrorDetail{}
	if err := proto.Unmarshal(claim.Result, d); err != nil {
		return nil, fmt.Errorf("decoding the stored error: %w", err)
	}
	msg, ok := replayText[d.GetReason()]
	if !ok {
		msg = replayText[""]
	}
	return nil, api.New(reasonCode(d.GetReason()), d.GetReason(), d.GetHint(), errors.New(msg))
}

// replayText is the message of a repeated call whose first attempt failed, by reason.
var replayText = map[string]string{
	api.ReasonInvalidArgument:    "an earlier call with this idempotency key was refused; nothing was sent",
	api.ReasonSendOutcomeUnknown: "an earlier call with this idempotency key may have sent the message",
	"":                           "an earlier call with this idempotency key failed; nothing was sent",
}

// reasonCode is the Connect code that goes with a stored reason.
func reasonCode(reason string) connect.Code {
	switch reason {
	case api.ReasonInvalidArgument:
		return connect.CodeInvalidArgument
	case api.ReasonSendOutcomeUnknown:
		return connect.CodeUnknown
	case api.ReasonServerUnavailable:
		return connect.CodeUnavailable
	case api.ReasonAuthRequired, api.ReasonAuthRejected:
		return connect.CodeUnauthenticated
	case api.ReasonTimeout:
		return connect.CodeDeadlineExceeded
	case api.ReasonNotFound, api.ReasonStaleRef:
		return connect.CodeNotFound
	default:
		return connect.CodeInternal
	}
}

// Fixed errors of the send pipeline.
var (
	errKeyRequired = &argError{
		msg:  "an idempotency key is required",
		hint: "pass idempotency_key: 1 to 128 of A-Z, a-z, 0-9 and . _ : -",
	}
	errKeyMalformed = &argError{
		msg:  "the idempotency key is malformed",
		hint: "pass idempotency_key: 1 to 128 of A-Z, a-z, 0-9 and . _ : -",
	}
	errKeyReused = &argError{
		msg:  "key already used for a different message",
		hint: "use a new idempotency key for a different message",
	}
	errStillInProgress = api.New(connect.CodeUnavailable, api.ReasonServerUnavailable, "try again later with the same key",
		errors.New("a send with this key is still in progress"))
	errDryRunAndDraft = &argError{msg: "dry_run and draft cannot both be set", hint: "pass one of them"}
)

// refusedErr is the error for a message the server refused for good.
func refusedErr(se *sender.SendError) error {
	const hint = "nothing was sent; fix the message and send it with a new idempotency key"
	switch {
	case len(se.Refused) > 0:
		// The refused addresses are the caller's own recipients.
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, hint,
			&fixedTextError{msg: "the mail server refused the recipients: " + strings.Join(se.Refused, ", "), err: se})
	case se.Code == 0:
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, hint,
			&fixedTextError{msg: "the message could not be submitted to the mail server", err: se})
	default:
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, hint,
			&fixedTextError{msg: "the mail server refused the message", err: se})
	}
}

// notSentErr is the error for a send that failed in a way that is safe to
// repeat: the server does not have the message.
func notSentErr(se *sender.SendError) error {
	return api.New(connect.CodeUnavailable, api.ReasonServerUnavailable, "try again later with the same idempotency key",
		&fixedTextError{msg: "the mail server did not take the message; nothing was sent", err: se})
}

// outcomeUnknownErr is the error for a send that may have gone out.
func outcomeUnknownErr(messageID string) error {
	return api.New(connect.CodeUnknown, api.ReasonSendOutcomeUnknown, outcomeHint(messageID),
		errors.New("the connection was lost after the message was handed over; it may or may not have been sent"))
}

func outcomeHint(messageID string) string {
	if messageID == "" {
		return "search the Sent folder before sending again; repeating this call will not send"
	}
	return "search the Sent folder for Message-ID <" + messageID + "> before sending again; repeating this call will not send"
}

// sendText is the whole client message for a send-type failure, by
// reason. As for reads, mail server and sender text never reaches a client.
var sendText = map[string]string{
	api.ReasonServerUnavailable: "the mail server failed; nothing was sent",
	api.ReasonTimeout:           "timed out; nothing was sent",
	api.ReasonAuthRequired:      "the account needs authentication",
	api.ReasonAuthRejected:      "the server rejected the credentials",
	api.ReasonNotFound:          "not found",
	api.ReasonStaleRef:          "the message is gone; search again",
	api.ReasonInternal:          "the message could not be sent",
}

// composeErr maps a ComposeService failure to the Connect error the client
// sees. Errors the pipeline already built pass through. INVALID_ARGUMENT
// keeps its text, which the daemon writes from the request. Anything else
// gets fixed text by reason, with the cause kept for the daemon's logs.
func composeErr(err error) *connect.Error {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr
	}
	switch {
	case errors.Is(err, compose.ErrInvalid):
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, "", err)
	case errors.Is(err, upload.ErrUnknownHandle):
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument,
			"upload the file again: an upload lasts an hour and belongs to the account it was uploaded for", err)
	case errors.Is(err, upload.ErrTooLarge):
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument,
			"the account's message size limit caps an upload; send a link instead", err)
	}
	cerr = toConnectError(err)
	d := api.Detail(cerr)
	if d.GetReason() == api.ReasonInvalidArgument {
		return cerr
	}
	msg, ok := sendText[d.GetReason()]
	if !ok {
		msg = sendText[api.ReasonInternal]
	}
	return api.New(cerr.Code(), d.GetReason(), d.GetHint(), &fixedTextError{msg: msg, err: err})
}

// errReason is err's reason code, for logs. A mail server's error can quote
// the server, so logs carry only how it was classified.
func errReason(err error) string { return api.Detail(toConnectError(err)).GetReason() }

// record writes the audit row for a ComposeService call and returns err as
// the client sees it.
func (c *composeService) record(ctx context.Context, cl caller, e audit.Entry, err error) error {
	var cerr *connect.Error
	if err != nil {
		cerr = composeErr(err)
		e.Result = audit.ResultError
		e.Reason = api.Detail(cerr).GetReason()
	}
	if aerr := c.s.record(ctx, cl, e, nil); aerr != nil {
		return aerr
	}
	if cerr != nil {
		return cerr
	}
	return nil
}

// checkSize refuses a message larger than the account's limit.
func checkSize(acct config.ResolvedAccount, n int64) error {
	if n <= acct.MaxMessageSize {
		return nil
	}
	return &argError{
		msg:  fmt.Sprintf("the message comes to about %d bytes, over the account's limit of %d", n, acct.MaxMessageSize),
		hint: "send large files as links, or raise max_message_size in the config",
	}
}

// inputSize is a lower bound, from spec alone, on the size of the message
// it builds, so a message certainly too large is refused before dialing.
// Attachments and inline images count at their raw size: base64 only grows
// a part, and a 7-bit email or multipart attachment goes as it is, with LF
// line endings only growing to CRLF. Text counts at its length with
// trailing white space dropped from each line, which is all quoted-printable
// and format=flowed can save. Header fields count at the length of the
// values the builder writes, each address once. Bcc counts for nothing: it
// is only in the envelope. The size of the built message is checked again
// before it is sent.
func inputSize(spec compose.Spec) int64 {
	n := textLen(spec.Body.Source)
	if spec.Body.Kind == compose.HTML && len(spec.Inline) > 0 {
		// Rewriting the image references can shorten the HTML.
		n = 0
	}
	n += textLen(spec.Body.HTMLText) + textLen(spec.Signature)
	n += headerLen(spec.Subject) + headerLen(spec.InReplyTo) + headerLen(spec.UserAgent) + int64(len(spec.From.Address))
	seen := map[string]bool{}
	for _, list := range [][]mail.Address{spec.To, spec.Cc} {
		for _, a := range list {
			if k := strings.ToLower(a.Address); !seen[k] {
				seen[k] = true
				n += int64(len(a.Address))
			}
		}
	}
	for k, v := range spec.Headers {
		n += int64(len(k)) + headerLen(v)
	}
	for _, r := range spec.References {
		n += int64(len(r))
	}
	for _, f := range spec.Attachments {
		n += int64(len(f.Data))
	}
	for _, f := range spec.Inline {
		n += int64(len(f.Data))
	}
	return n
}

// textLen is the length of s without the white space at the end of each
// line.
func textLen(s string) int64 {
	var n int64
	for l := range strings.SplitSeq(s, "\n") {
		n += int64(len(strings.TrimRight(l, " \t\r"))) + 1
	}
	return n - 1
}

// headerLen is the length of a header value without its line breaks.
func headerLen(s string) int64 {
	return int64(len(s) - strings.Count(s, "\r") - strings.Count(s, "\n"))
}

// specFromProto validates a ComposeSpec and resolves uploads into a
// compose.Spec without Quote. From is the account address with its display
// name, and the signature is the account's unless no_signature.
func (c *composeService) specFromProto(ctx context.Context, p *chainmailv1.ComposeSpec) (compose.Spec, error) {
	if err := ctx.Err(); err != nil {
		return compose.Spec{}, err
	}
	acct, err := c.account(p.GetAccount())
	if err != nil {
		return compose.Spec{}, err
	}
	spec := compose.Spec{
		From:      mail.Address{Name: acct.DisplayName, Address: acct.Address},
		Subject:   p.GetSubject(),
		Date:      c.s.opts.Now().Local(),
		UserAgent: "chainmail/" + version.Version,
	}
	if spec.To, err = parseAddresses("to", p.GetTo()); err != nil {
		return compose.Spec{}, err
	}
	if spec.Cc, err = parseAddresses("cc", p.GetCc()); err != nil {
		return compose.Spec{}, err
	}
	if spec.Bcc, err = parseAddresses("bcc", p.GetBcc()); err != nil {
		return compose.Spec{}, err
	}
	if err := c.applyContent(&spec, acct, p.GetContent()); err != nil {
		return compose.Spec{}, err
	}
	return spec, nil
}

// applyContent sets spec's body, attachments, inline images, headers and
// signature from p, reading the uploads it names once their recorded sizes
// together fit the account's limit.
func (c *composeService) applyContent(spec *compose.Spec, acct config.ResolvedAccount, p *chainmailv1.Content) error {
	body := p.GetBody()
	switch k := body.GetKind().(type) {
	case nil:
		spec.Body = compose.Body{Kind: compose.Markdown}
	case *chainmailv1.ComposeBody_Markdown:
		spec.Body = compose.Body{Kind: compose.Markdown, Source: k.Markdown}
	case *chainmailv1.ComposeBody_Text:
		spec.Body = compose.Body{Kind: compose.Text, Source: k.Text}
	case *chainmailv1.ComposeBody_Html:
		spec.Body = compose.Body{Kind: compose.HTML, Source: k.Html, HTMLText: body.GetHtmlText()}
	}
	if body.GetHtmlText() != "" && spec.Body.Kind != compose.HTML {
		return &argError{msg: "html_text is only for an html body", hint: "drop html_text, or send the body as html"}
	}
	if err := c.checkUploadSizes(acct, p); err != nil {
		return err
	}
	for i, h := range p.GetAttachments() {
		f, err := c.openUpload(acct, h)
		if err != nil {
			return fmt.Errorf("attachment %d: %w", i+1, err)
		}
		spec.Attachments = append(spec.Attachments, f)
	}
	for i, in := range p.GetInline() {
		f, err := c.openUpload(acct, in.GetUpload())
		if err != nil {
			return fmt.Errorf("inline image %d: %w", i+1, err)
		}
		if spec.Inline == nil {
			spec.Inline = map[string]compose.File{}
		}
		spec.Inline[in.GetName()] = f
	}
	spec.Headers = p.GetHeaders()
	if !p.GetNoSignature() {
		spec.Signature = acct.Signature
	}
	return nil
}

// checkUploadSizes checks the inline image names and every upload p names,
// and refuses the uploads when together, at their recorded sizes, they are
// over acct's limit. It reads no upload's bytes, so a request naming large
// uploads, or one upload many times, is refused before any is loaded.
func (c *composeService) checkUploadSizes(acct config.ResolvedAccount, p *chainmailv1.Content) error {
	var total int64
	for i, h := range p.GetAttachments() {
		info, err := c.statUpload(acct, h)
		if err != nil {
			return fmt.Errorf("attachment %d: %w", i+1, err)
		}
		total += info.Size
	}
	names := map[string]bool{}
	for i, in := range p.GetInline() {
		name := in.GetName()
		if name == "" || strings.ContainsFunc(name, isControl) {
			return &argError{msg: fmt.Sprintf("inline image %d has no usable name", i+1), hint: "name each inline image as the body refers to it"}
		}
		if names[name] {
			return &argError{msg: fmt.Sprintf("inline image %d repeats a name", i+1), hint: "give each inline image its own name"}
		}
		names[name] = true
		info, err := c.statUpload(acct, in.GetUpload())
		if err != nil {
			return fmt.Errorf("inline image %d: %w", i+1, err)
		}
		total += info.Size
	}
	return checkSize(acct, total)
}

// uploadSums maps each upload handle p names to the hex SHA-256 of the
// bytes spec read for it, for the request hash.
func uploadSums(p *chainmailv1.Content, spec compose.Spec) map[string]string {
	sums := map[string]string{}
	for i, h := range p.GetAttachments() {
		if i < len(spec.Attachments) {
			sums[h] = hexSum(spec.Attachments[i].Data)
		}
	}
	for _, in := range p.GetInline() {
		if f, ok := spec.Inline[in.GetName()]; ok {
			sums[in.GetUpload()] = hexSum(f.Data)
		}
	}
	return sums
}

func hexSum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// requestHash is the SHA-256 of req's deterministic encoding with the
// idempotency key cleared and every upload handle replaced by the hex
// SHA-256 of its bytes, so uploading the same file again under a new handle
// is the same request.
func requestHash(req proto.Message, sums map[string]string) ([32]byte, error) {
	m := proto.Clone(req)
	r := m.ProtoReflect()
	if fd := r.Descriptor().Fields().ByName("idempotency_key"); fd != nil {
		r.Clear(fd)
	}
	replaceHandles(r, sums)
	sortRecipients(r)
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		return [32]byte{}, fmt.Errorf("encoding the request: %w", err)
	}
	return sha256.Sum256(b), nil
}

// replaceHandles replaces the upload handles of every Content in m.
// recipientFields are the address lists whose order does not change what a
// send is. The CLI derives one key for any order, so the hash must agree.
var recipientFields = map[protoreflect.Name]bool{"to": true, "cc": true, "bcc": true, "envelope_to": true}

// sortRecipients sorts every recipient list in m and its messages, in place.
// requestHash calls it on a clone.
func sortRecipients(m protoreflect.Message) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Kind() == protoreflect.StringKind && recipientFields[fd.Name()]:
			list := v.List()
			addrs := make([]string, list.Len())
			for i := range addrs {
				addrs[i] = list.Get(i).String()
			}
			slices.Sort(addrs)
			for i, a := range addrs {
				list.Set(i, protoreflect.ValueOfString(a))
			}
		case fd.Kind() == protoreflect.MessageKind && !fd.IsList() && !fd.IsMap():
			sortRecipients(v.Message())
		}
		return true
	})
}

func replaceHandles(m protoreflect.Message, sums map[string]string) {
	if content, ok := m.Interface().(*chainmailv1.Content); ok {
		for i, h := range content.GetAttachments() {
			content.Attachments[i] = sums[h]
		}
		for _, in := range content.GetInline() {
			in.Upload = sums[in.GetUpload()]
		}
		return
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() == protoreflect.MessageKind && fd.Cardinality() != protoreflect.Repeated && !fd.IsMap() {
			replaceHandles(v.Message(), sums)
		}
		return true
	})
}

// account resolves a client-supplied account name.
func (c *composeService) account(name string) (config.ResolvedAccount, error) {
	if !config.ValidName(name) {
		return config.ResolvedAccount{}, errInvalidAccountName
	}
	return c.s.opts.Config.Account(name)
}

// parseAddresses parses the recipients of one field. The error names the
// field and position, never the bytes.
func parseAddresses(field string, list []string) ([]mail.Address, error) {
	out := make([]mail.Address, 0, len(list))
	for i, s := range list {
		a, err := mail.ParseAddress(s)
		if err != nil {
			return nil, &argError{
				msg:  fmt.Sprintf("%s recipient %d is not an email address", field, i+1),
				hint: `write each recipient as "Name <a@example.com>" or "a@example.com"`,
			}
		}
		out = append(out, *a)
	}
	return out, nil
}

func isControl(r rune) bool { return r < ' ' || r == 0x7f }

// own records that handle belongs to account, and forgets owners of
// uploads that have expired.
func (c *composeService) own(handle, account string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.s.opts.Now()
	for h, o := range c.owners {
		if now.Sub(o.at) >= uploadTTL {
			delete(c.owners, h)
		}
	}
	c.owners[handle] = uploadOwner{account: account, at: now}
}

// owned reports whether handle is a live upload of acct.
func (c *composeService) owned(acct config.ResolvedAccount, handle string) bool {
	c.mu.Lock()
	o, ok := c.owners[handle]
	c.mu.Unlock()
	return ok && o.account == acct.Name && c.s.uploads != nil
}

// statUpload is the info of an upload of acct, without its bytes. A handle
// of another account is unknown.
func (c *composeService) statUpload(acct config.ResolvedAccount, handle string) (upload.Info, error) {
	if !c.owned(acct, handle) {
		return upload.Info{}, upload.ErrUnknownHandle
	}
	return c.s.uploads.Stat(handle)
}

// openUpload reads an upload of acct. A handle of another account reads as unknown.
func (c *composeService) openUpload(acct config.ResolvedAccount, handle string) (compose.File, error) {
	if !c.owned(acct, handle) {
		return compose.File{}, upload.ErrUnknownHandle
	}
	info, data, err := c.s.uploads.Open(handle)
	if err != nil {
		return compose.File{}, err
	}
	return compose.File{Filename: info.Filename, ContentType: info.ContentType, Data: data}, nil
}

// roleFolder is the name of st's selectable folder with role.
func roleFolder(ctx context.Context, st mailstore.Store, role mailstore.Role) (string, error) {
	folders, err := st.ListFolders(ctx)
	if err != nil {
		return "", err
	}
	for _, f := range folders {
		if f.Role == role && f.Selectable {
			return f.Name, nil
		}
	}
	return "", fmt.Errorf("%w: the account has no %s folder", mailstore.ErrNotFound, role)
}

// findIn looks messageID up in st's folder with role.
func findIn(ctx context.Context, st mailstore.Store, role mailstore.Role, messageID string) ([]ref.Ref, error) {
	folder, err := roleFolder(ctx, st, role)
	if err != nil {
		return nil, err
	}
	return st.FindByMessageID(ctx, folder, messageID)
}

// saveDraft appends msg to the account's Drafts folder, flagged \Draft and \Seen.
// When the server stores it without naming it, the draft has no ref and a
// hint to search the Drafts folder for its Message-ID.
func (c *composeService) saveDraft(ctx context.Context, acct config.ResolvedAccount, msg compose.Message) (*chainmailv1.Draft, error) {
	st := c.s.stores.get(acct)
	folder, err := roleFolder(ctx, st, mailstore.RoleDrafts)
	if err != nil {
		return nil, err
	}
	r, err := st.Append(ctx, folder, []string{flagDraft, flagSeen}, time.Time{}, msg.Raw)
	if err != nil {
		return nil, err
	}
	d := &chainmailv1.Draft{Ref: refString(r), MessageId: msg.MessageID}
	if d.Ref == "" {
		d.Hint = "search the Drafts folder for Message-ID <" + msg.MessageID + ">"
	}
	return d, nil
}

// recoverSends settles the keys an earlier daemon left in progress as of
// unknown outcome, so a repeat never sends them again. Serve runs it before
// taking requests.
func (s *Server) recoverSends(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, recoverTimeout)
	defer cancel()
	n, err := s.idem.Recover(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		s.opts.Logger.Warn("sends left in progress by an earlier daemon now have an unknown outcome", "count", n)
	}
	return nil
}
