package sender

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/mailaddr"
	"github.com/yaklabco/chainmail/internal/provider"
)

// Outcome says what a failed Send means for the message.
type Outcome int

const (
	// NotSentPermanent is a 5xx before or at the end of DATA. Sending the
	// same message again would fail the same way.
	NotSentPermanent Outcome = iota
	// NotSentTemporary is a 4xx, or a connection lost before the final dot
	// was written. The server does not have the message.
	NotSentTemporary
	// Unknown is a connection lost, or a timeout, after the final dot was
	// written and before the server replied. The server may have the message.
	Unknown
)

func (o Outcome) String() string {
	switch o {
	case NotSentPermanent:
		return "not sent (permanent)"
	case NotSentTemporary:
		return "not sent (temporary)"
	case Unknown:
		return "unknown"
	default:
		return "Outcome(" + strconv.Itoa(int(o)) + ")"
	}
}

// SendError is every error Send returns.
type SendError struct {
	Outcome Outcome
	// Code is the SMTP reply code when the server answered, else 0.
	Code int
	// Refused lists the addresses refused at RCPT with a 5xx, in the order given.
	Refused []string
	Err     error

	step string // the SMTP step that failed, for Error
}

// Error describes the failure. It names no recipient: Refused is only
// counted, and an SMTP reply shows only its code and the step it answered,
// because servers echo addresses in reply text. A malformed reply is shown
// only as that. The full reply stays in the chain for errors.AsType.
func (e *SendError) Error() string {
	var b strings.Builder
	b.WriteString("SMTP send: ")
	b.WriteString(e.Outcome.String())
	if e.Code != 0 {
		b.WriteString(", ")
		b.WriteString(strconv.Itoa(e.Code))
	}
	_, isReply := errors.AsType[*smtp.SMTPError](e.Err)
	if isReply && e.step != "" {
		b.WriteString(" at ")
		b.WriteString(e.step)
	}
	if n := len(e.Refused); n > 0 {
		fmt.Fprintf(&b, ", %d recipient(s) refused", n)
	}
	_, isProtocol := errors.AsType[textproto.ProtocolError](e.Err)
	switch {
	case e.Err == nil || isReply:
	case isProtocol:
		// A malformed reply line is server text too, and may hold an address.
		b.WriteString(": malformed reply at ")
		b.WriteString(e.step)
	default:
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *SendError) Unwrap() error { return e.Err }

var (
	errNoRecipients = errors.New("no recipients")
	errConnDone     = errors.New("connection already used or closed")
	// The unsafe-address errors name no address: Error never does.
	errUnsafeFrom = errors.New("the envelope sender is not a plain address that is safe to send")
	errUnsafeRcpt = errors.New("an envelope recipient is not a plain address that is safe to send")
)

// Conn is an authenticated SMTP session ready to submit one message. It is
// for one goroutine at a time.
type Conn struct {
	client  *smtp.Client
	raw     net.Conn
	addr    string
	timeout time.Duration
	done    bool // Send ran or Close was called

	// withTimeout bounds a Send by timeout. Nil means context.WithTimeout;
	// a test sets it to pick the moment the bound passes.
	withTimeout func(context.Context, time.Duration) (context.Context, context.CancelFunc)
}

// Dial connects, negotiates TLS and authenticates, within opts.Timeout.
// Auth refusals match auth.ErrRejected as Probe's do.
func Dial(ctx context.Context, ep provider.Endpoint, creds auth.Credentials, opts DialOptions) (*Conn, error) {
	var dialer net.Dialer
	return dial(ctx, ep, creds, opts, dialer.DialContext)
}

// dialFunc opens the TCP connection. Tests swap it to put faults under TLS.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func dial(ctx context.Context, ep provider.Endpoint, creds auth.Credentials, opts DialOptions, dialTCP dialFunc) (*Conn, error) {
	timeout := opts.timeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	nc, err := dialTCP(ctx, "tcp", ep.Addr())
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", ep.Addr(), err)
	}
	raw := nc
	// If ctx ends mid-exchange, closing the connection unblocks the read or write in progress.
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() }) // the caller sees ctx.Err() instead
	c, err := handshake(ctx, raw, ep, opts.RootCAs)
	if err == nil {
		err = authenticate(c, creds)
	}
	if !stop() {
		return nil, fmt.Errorf("dialing %s: %w", ep.Addr(), ctx.Err())
	}
	if err != nil {
		_ = raw.Close() // the dial error is the one to report
		return nil, err
	}
	return &Conn{client: c, raw: raw, addr: ep.Addr(), timeout: timeout}, nil
}

// Close drops the connection without sending. It is safe to call after Send
// and more than once.
func (c *Conn) Close() error {
	if c.done {
		return nil
	}
	c.done = true
	if err := c.raw.Close(); err != nil {
		return fmt.Errorf("closing connection to %s: %w", c.addr, err)
	}
	return nil
}

// Send submits once and quits. It never retries. Errors are *SendError.
// The DialOptions timeout bounds it as well as ctx.
func (c *Conn) Send(ctx context.Context, from string, to []string, raw []byte) error {
	if c.done {
		return &SendError{Outcome: NotSentPermanent, Err: errConnDone}
	}
	c.done = true
	defer func() { _ = c.raw.Close() }() // QUIT may already have closed it

	if len(to) == 0 {
		return &SendError{Outcome: NotSentPermanent, Err: errNoRecipients}
	}
	bound := c.withTimeout
	if bound == nil {
		bound = context.WithTimeout
	}
	ctx, cancel := bound(ctx, c.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return &SendError{Outcome: NotSentTemporary, Err: fmt.Errorf("before MAIL FROM: %w", err)}
	}
	stop := context.AfterFunc(ctx, func() { _ = c.raw.Close() }) // the error then carries ctx.Err()
	defer stop()

	se := c.submit(from, to, raw)
	if se == nil {
		return nil
	}
	if _, isReply := errors.AsType[*smtp.SMTPError](se.Err); !isReply && ctx.Err() != nil {
		se.Err = fmt.Errorf("%w: %w", ctx.Err(), se.Err)
	}
	return se
}

// submit runs one mail transaction and QUIT. A nil result means the server
// accepted the message.
func (c *Conn) submit(from string, to []string, raw []byte) *SendError {
	var opts smtp.MailOptions
	opts.Size = int64(len(raw))
	opts.UTF8 = !isASCII(from) || !allASCII(to)
	if err := c.checkEnvelope(from, to, opts.UTF8); err != nil {
		c.quit()
		return &SendError{Outcome: NotSentPermanent, Err: err}
	}
	if err := c.client.Mail(from, &opts); err != nil {
		c.quit()
		return failure(err, false, "MAIL FROM")
	}

	var refused []string
	var permanent, temporary *smtp.SMTPError
	for _, addr := range to {
		err := c.client.Rcpt(addr, nil)
		if err == nil {
			continue
		}
		reply, ok := errors.AsType[*smtp.SMTPError](err)
		if !ok {
			// The connection is unusable. A refusal already received still
			// decides the outcome: the message cannot go to that address.
			if permanent != nil {
				se := failure(permanent, false, "RCPT TO")
				se.Refused = refused
				se.Err = errors.Join(se.Err, fmt.Errorf("RCPT TO after a refusal: %w", err))
				return se
			}
			return failure(err, false, "RCPT TO")
		}
		switch {
		case reply.Code >= 500:
			refused = append(refused, addr)
			if permanent == nil {
				permanent = reply
			}
		case temporary == nil:
			temporary = reply
		}
	}
	if permanent != nil {
		c.resetAndQuit()
		se := failure(permanent, false, "RCPT TO")
		se.Refused = refused
		return se
	}
	if temporary != nil {
		c.resetAndQuit()
		return failure(temporary, false, "RCPT TO")
	}

	w, err := c.client.Data()
	if err != nil {
		c.quit()
		return failure(err, false, "DATA")
	}
	// A failed Write means the final dot was never written: not sent.
	if _, err := w.Write(raw); err != nil {
		return failure(err, false, "writing the message")
	}
	// Close flushes what is still buffered, writes the final dot and reads
	// the reply. Nothing here can tell a write that failed before the dot
	// from one that failed after it (TLS also writes while reading, for an
	// alert or a KeyUpdate), so any failure counts as after the dot. The
	// message may have been sent, and a duplicate is worse than an unknown.
	if err := w.Close(); err != nil {
		c.quit()
		return failure(err, true, "end of DATA")
	}
	c.quit()
	return nil
}

// failure classifies err from step. afterDot says the final dot had been
// written when err happened.
func failure(err error, afterDot bool, step string) *SendError {
	wrapped := fmt.Errorf("%s: %w", step, err)
	reply, ok := errors.AsType[*smtp.SMTPError](err)
	if !ok {
		if afterDot {
			return &SendError{Outcome: Unknown, Err: wrapped, step: step}
		}
		return &SendError{Outcome: NotSentTemporary, Err: wrapped, step: step}
	}
	se := &SendError{Code: reply.Code, Err: wrapped, step: step}
	switch {
	case reply.Code >= 500:
		se.Outcome = NotSentPermanent
	case reply.Code >= 400:
		se.Outcome = NotSentTemporary
	case afterDot:
		// An unexpected reply class to the final dot says nothing certain.
		se.Outcome = Unknown
	default:
		// An unexpected reply class before the dot: nothing was submitted.
		se.Outcome = NotSentTemporary
	}
	return se
}

// checkEnvelope refuses an envelope it cannot write safely, before MAIL
// FROM. go-smtp writes each address into its path as it is, so an address
// that is not mailaddr.Safe could close the path early and pass the rest as
// ESMTP parameters, reaching a recipient nobody checked. An empty from is
// the null reverse-path. It also refuses what go-smtp would refuse locally.
// Either way the refusal counts as permanent, not as a lost connection.
func (c *Conn) checkEnvelope(from string, to []string, utf8 bool) error {
	if from != "" && !mailaddr.Safe(from) {
		return errUnsafeFrom
	}
	for _, addr := range to {
		if !mailaddr.Safe(addr) {
			return errUnsafeRcpt
		}
	}
	if ok, _ := c.client.Extension("SMTPUTF8"); utf8 && !ok {
		return errors.New("non-ASCII envelope address and the server does not offer SMTPUTF8")
	}
	return nil
}

// resetAndQuit ends a refused transaction politely. Failures are ignored: the
// outcome is already decided and the connection is closed either way.
func (c *Conn) resetAndQuit() {
	if c.client.Reset() == nil {
		c.quit()
	}
}

// quit sends QUIT, ignoring the reply: the outcome is already decided and the
// connection is closed either way.
func (c *Conn) quit() {
	_ = c.client.Quit() // see above
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func allASCII(ss []string) bool {
	for _, s := range ss {
		if !isASCII(s) {
			return false
		}
	}
	return true
}
