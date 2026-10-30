package daemon

import (
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
	// decisionDuplicate and decisionDryRun go in the audit decision column.
	decisionDuplicate = "duplicate"
	decisionDryRun    = "dry_run"
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
	// notSentCopy is never taken for the Sent copy. SendDraft sets it to
	// the draft, which has the sent message's Message-ID.
	notSentCopy ref.Ref
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
	if err != nil {
		return nil, idempotency.Retryable, err
	}
	entry.Targets = append(entry.Targets, msg.MessageID)

	// From here the message goes out in full or not at all, whether or not
	// the client is still there. The connection's own timeout bounds it.
	detached := context.WithoutCancel(ctx)
	sendErr := conn.Send(detached, msg.EnvelopeFrom, msg.EnvelopeTo, msg.Raw)
	sent := &chainmailv1.Sent{MessageId: msg.MessageID}
	if sendErr != nil {
		// The error's text can quote the server, so it stays at debug.
		c.s.opts.Logger.Debug("SMTP send failed", "account", job.acct.Name, "message_id", msg.MessageID, "err", sendErr)
		var se *sender.SendError
		if !errors.As(sendErr, &se) {
			se = &sender.SendError{Outcome: sender.NotSentTemporary, Err: sendErr}
		}
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

// prepare builds the message while it dials, and returns both. A failure of
// either cancels the other and closes the connection if one was made.
func (c *composeService) prepare(ctx context.Context, job sendJob) (compose.Message, *sender.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		once     sync.Once
		firstErr error
		msg      compose.Message
		conn     *sender.Conn
		wg       sync.WaitGroup
	)
	fail := func(err error) {
		once.Do(func() {
			firstErr = err
			cancel()
		})
	}
	wg.Go(func() {
		m, err := job.build(ctx)
		if err == nil {
			err = checkSize(job.acct, int64(len(m.Raw)))
		}
		if err != nil {
			fail(err)
			return
		}
		msg = m
	})
	wg.Go(func() {
		cn, err := c.connect(ctx, job.acct)
		if err != nil {
			fail(err)
			return
		}
		conn = cn
	})
	wg.Wait()
	if firstErr != nil {
		if conn != nil {
			if err := conn.Close(); err != nil {
				c.s.opts.Logger.Debug("closing an unused SMTP connection", "account", job.acct.Name, "err", err)
			}
		}
		return compose.Message{}, nil, firstErr
	}
	return msg, conn, nil
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
	c.s.opts.Logger.Debug("SMTP dial failed", "account", acct.Name, "err", err)
	switch {
	case errors.Is(err, auth.ErrRejected):
		c.s.auth.Rejected(acct.Name, creds)
		hint := ""
		if st, serr := c.s.auth.State(acct.Name); serr == nil {
			hint = st.Detail
		}
		return nil, api.New(connect.CodeUnauthenticated, api.ReasonAuthRejected, hint,
			&fixedTextError{msg: "the server rejected the credentials; nothing was sent", err: err})
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return nil, err
	default:
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
		sent.SentRef, sent.SentCopyError = c.sentCopy(ctx, job.acct, msg, job.notSentCopy)
	}
	wg.Wait()
}

// sentCopy returns the ref of the Sent copy, or fixed text saying why there
// is none. Google files sent mail itself, so its copy is looked for once in
// All Mail, and not finding it yet is no error. Elsewhere the message is
// appended to the Sent folder, marked read. skip is never taken for the
// copy.
func (c *composeService) sentCopy(ctx context.Context, acct config.ResolvedAccount, msg compose.Message, skip ref.Ref) (sentRef, copyErr string) {
	st := c.s.stores.get(acct)
	if acct.Provider.Gmail {
		refs, err := findIn(ctx, st, mailstore.RoleAll, msg.MessageID)
		if err != nil {
			c.s.opts.Logger.Debug("looking up Gmail's Sent copy", "account", acct.Name, "err", err)
		}
		refs = slices.DeleteFunc(refs, func(r ref.Ref) bool {
			return r.Folder == skip.Folder && r.UIDValidity == skip.UIDValidity && r.UID == skip.UID
		})
		if len(refs) == 0 {
			return "", ""
		}
		return refs[0].String(), ""
	}
	folder, err := roleFolder(ctx, st, mailstore.RoleSent)
	if err == nil {
		r, aerr := st.Append(ctx, folder, []string{flagSeen}, time.Time{}, msg.Raw)
		if aerr == nil {
			return r.String(), ""
		}
		err = aerr
	}
	c.s.opts.Logger.Warn("storing the Sent copy failed", "account", acct.Name, "message_id", msg.MessageID, "err", err)
	return "", sentCopyFailed
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
			c.s.opts.Logger.Debug("searching Sent for a send of unknown outcome", "account", acct.Name, "err", err)
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
		msg:  fmt.Sprintf("the message is at least %d bytes, over the account's limit of %d", n, acct.MaxMessageSize),
		hint: "send large files as links, or raise max_message_size in the config",
	}
}

// minSize is a lower bound on the size of the message spec builds: the body
// source, which every body kind sends at least once, and each file
// base64-encoded. It lets an oversized message be refused before dialing.
func minSize(spec compose.Spec) int64 {
	n := int64(len(spec.Body.Source))
	for _, f := range spec.Attachments {
		n += base64Len(len(f.Data))
	}
	for _, f := range spec.Inline {
		n += base64Len(len(f.Data))
	}
	return n
}

func base64Len(n int) int64 { return int64((n + 2) / 3 * 4) }

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
// signature from p, reading the uploads it names.
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
	for i, h := range p.GetAttachments() {
		f, err := c.openUpload(acct, h)
		if err != nil {
			return fmt.Errorf("attachment %d: %w", i+1, err)
		}
		spec.Attachments = append(spec.Attachments, f)
	}
	for i, in := range p.GetInline() {
		name := in.GetName()
		if name == "" || strings.ContainsFunc(name, isControl) {
			return &argError{msg: fmt.Sprintf("inline image %d has no usable name", i+1), hint: "name each inline image as the body refers to it"}
		}
		if _, dup := spec.Inline[name]; dup {
			return &argError{msg: fmt.Sprintf("inline image %d repeats a name", i+1), hint: "give each inline image its own name"}
		}
		f, err := c.openUpload(acct, in.GetUpload())
		if err != nil {
			return fmt.Errorf("inline image %d: %w", i+1, err)
		}
		if spec.Inline == nil {
			spec.Inline = map[string]compose.File{}
		}
		spec.Inline[name] = f
	}
	spec.Headers = p.GetHeaders()
	if !p.GetNoSignature() {
		spec.Signature = acct.Signature
	}
	return nil
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
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		return [32]byte{}, fmt.Errorf("encoding the request: %w", err)
	}
	return sha256.Sum256(b), nil
}

// replaceHandles replaces the upload handles of every Content in m.
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

// openUpload reads an upload of acct. A handle of another account reads as unknown.
func (c *composeService) openUpload(acct config.ResolvedAccount, handle string) (compose.File, error) {
	c.mu.Lock()
	o, ok := c.owners[handle]
	c.mu.Unlock()
	if !ok || o.account != acct.Name || c.s.uploads == nil {
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
	return &chainmailv1.Draft{Ref: r.String(), MessageId: msg.MessageID}, nil
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
