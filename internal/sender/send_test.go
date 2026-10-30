package sender

import (
	"bytes"
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/testutil/smtptest"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// msg has CRLF line endings and a line that starts with a dot, so a
// byte-for-byte capture proves dot-stuffing round-trips.
var msg = []byte("From: me@example.com\r\nTo: a@example.com\r\nSubject: hi\r\n\r\n.leading dot\r\nbody\r\n")

var rcpts = []string{"a@example.com", "b@example.com", "c@example.com"}

func smtpErr(code int, enh smtp.EnhancedCode) *smtp.SMTPError {
	return &smtp.SMTPError{Code: code, EnhancedCode: enh, Message: "server says no"}
}

// startSMTP runs smtptest with opts and returns the server and a CA that trusts it.
func startSMTP(t *testing.T, sec provider.Security, opts smtptest.Options) (*smtptest.Server, *tlstest.CA) {
	t.Helper()
	ca := tlstest.New(t)
	return smtptest.StartWith(t, ca, sec, opts, user), ca
}

func dialT(t *testing.T, srv *smtptest.Server, ca *tlstest.CA, opts DialOptions) *Conn {
	t.Helper()
	opts.RootCAs = ca.Pool
	c, err := Dial(t.Context(), srv.Endpoint(), creds(provider.AuthPassword, "pw"), opts)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() }) // Send may already have closed it
	return c
}

func asSendError(t *testing.T, err error) *SendError {
	t.Helper()
	var se *SendError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want a *SendError", err, err)
	}
	return se
}

func TestSendDelivers(t *testing.T) {
	t.Parallel()
	for _, sec := range []provider.Security{provider.SecurityTLS, provider.SecurityStartTLS} {
		for name, c := range map[string]auth.Credentials{
			"password": creds(provider.AuthPassword, "pw"),
			"xoauth2":  creds(provider.AuthOAuth2, "tok"),
		} {
			t.Run(sec.String()+"/"+name, func(t *testing.T) {
				t.Parallel()
				srv, ca := startSMTP(t, sec, smtptest.Options{})
				conn, err := Dial(t.Context(), srv.Endpoint(), c, DialOptions{RootCAs: ca.Pool})
				if err != nil {
					t.Fatalf("Dial: %v", err)
				}
				if err := conn.Send(t.Context(), user.Username, rcpts, msg); err != nil {
					t.Fatalf("Send: %v", err)
				}
				got := srv.Messages()
				if len(got) != 1 {
					t.Fatalf("captured %d messages, want 1", len(got))
				}
				if got[0].From != user.Username || !slices.Equal(got[0].To, rcpts) {
					t.Errorf("envelope = %s -> %v, want %s -> %v", got[0].From, got[0].To, user.Username, rcpts)
				}
				if !bytes.Equal(got[0].Data, msg) {
					t.Errorf("data = %q, want %q", got[0].Data, msg)
				}
				if err := conn.Close(); err != nil {
					t.Errorf("Close after Send: %v", err)
				}
			})
		}
	}
}

func TestSendOutcomes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		opts        smtptest.Options
		wantOutcome Outcome
		wantCode    int
		wantRefused []string
		wantCapture bool
	}{
		{
			name:        "5xx at MAIL",
			opts:        smtptest.Options{FailMail: smtpErr(553, smtp.EnhancedCode{5, 7, 1})},
			wantOutcome: NotSentPermanent, wantCode: 553,
		},
		{
			name:        "4xx at MAIL",
			opts:        smtptest.Options{FailMail: smtpErr(451, smtp.EnhancedCode{4, 3, 0})},
			wantOutcome: NotSentTemporary, wantCode: 451,
		},
		{
			name:        "550 on one of three RCPTs",
			opts:        smtptest.Options{FailRcpt: map[string]*smtp.SMTPError{"b@example.com": smtpErr(550, smtp.EnhancedCode{5, 1, 1})}},
			wantOutcome: NotSentPermanent, wantCode: 550, wantRefused: []string{"b@example.com"},
		},
		{
			name: "every 5xx RCPT is collected",
			opts: smtptest.Options{FailRcpt: map[string]*smtp.SMTPError{
				"a@example.com": smtpErr(550, smtp.EnhancedCode{5, 1, 1}),
				"c@example.com": smtpErr(553, smtp.EnhancedCode{5, 1, 3}),
			}},
			wantOutcome: NotSentPermanent, wantCode: 550, wantRefused: []string{"a@example.com", "c@example.com"},
		},
		{
			name:        "450 RCPT",
			opts:        smtptest.Options{FailRcpt: map[string]*smtp.SMTPError{"b@example.com": smtpErr(450, smtp.EnhancedCode{4, 2, 1})}},
			wantOutcome: NotSentTemporary, wantCode: 450,
		},
		{
			name: "5xx RCPT outranks 4xx RCPT",
			opts: smtptest.Options{FailRcpt: map[string]*smtp.SMTPError{
				"a@example.com": smtpErr(450, smtp.EnhancedCode{4, 2, 1}),
				"b@example.com": smtpErr(550, smtp.EnhancedCode{5, 1, 1}),
			}},
			wantOutcome: NotSentPermanent, wantCode: 550, wantRefused: []string{"b@example.com"},
		},
		{
			name: "5xx RCPT then a lost connection keeps the refusal",
			opts: smtptest.Options{
				FailRcpt:   map[string]*smtp.SMTPError{"a@example.com": smtpErr(550, smtp.EnhancedCode{5, 1, 1})},
				DropAtRcpt: "b@example.com",
			},
			wantOutcome: NotSentPermanent, wantCode: 550, wantRefused: []string{"a@example.com"},
		},
		{
			name:        "lost connection at RCPT",
			opts:        smtptest.Options{DropAtRcpt: "b@example.com"},
			wantOutcome: NotSentTemporary,
		},
		{
			name: "4xx RCPT then a lost connection",
			opts: smtptest.Options{
				FailRcpt:   map[string]*smtp.SMTPError{"a@example.com": smtpErr(450, smtp.EnhancedCode{4, 2, 1})},
				DropAtRcpt: "b@example.com",
			},
			wantOutcome: NotSentTemporary,
		},
		{
			name:        "554 after DATA",
			opts:        smtptest.Options{FailData: smtpErr(554, smtp.EnhancedCode{5, 6, 0})},
			wantOutcome: NotSentPermanent, wantCode: 554,
		},
		{
			name:        "451 after DATA",
			opts:        smtptest.Options{FailData: smtpErr(451, smtp.EnhancedCode{4, 3, 0})},
			wantOutcome: NotSentTemporary, wantCode: 451,
		},
		{
			name:        "drop before DATA end",
			opts:        smtptest.Options{DropBeforeDataEnd: true},
			wantOutcome: NotSentTemporary,
		},
		{
			name:        "drop after DATA end",
			opts:        smtptest.Options{DropAfterDataEnd: true},
			wantOutcome: Unknown, wantCapture: true,
		},
	}
	for _, sec := range []provider.Security{provider.SecurityTLS, provider.SecurityStartTLS} {
		for _, tt := range tests {
			t.Run(sec.String()+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				srv, ca := startSMTP(t, sec, tt.opts)
				conn := dialT(t, srv, ca, DialOptions{})
				se := asSendError(t, conn.Send(t.Context(), user.Username, rcpts, msg))
				if se.Outcome != tt.wantOutcome {
					t.Errorf("Outcome = %v, want %v (err %v)", se.Outcome, tt.wantOutcome, se)
				}
				if se.Code != tt.wantCode {
					t.Errorf("Code = %d, want %d (err %v)", se.Code, tt.wantCode, se)
				}
				if !slices.Equal(se.Refused, tt.wantRefused) {
					t.Errorf("Refused = %v, want %v", se.Refused, tt.wantRefused)
				}
				if tt.wantCode != 0 {
					var reply *smtp.SMTPError
					if !errors.As(se, &reply) || reply.Code != tt.wantCode {
						t.Errorf("err = %v, want the server's %d in the chain", se, tt.wantCode)
					}
				}
				if n := len(srv.Messages()); (n == 1) != tt.wantCapture || n > 1 {
					t.Errorf("captured %d messages, want capture %v", n, tt.wantCapture)
				}
			})
		}
	}
}

// A context that ends while the client waits for the reply to the final dot
// leaves the outcome unknown: the server has the whole message.
func TestSendDeadlineDuringDataReplyIsUnknown(t *testing.T) {
	t.Parallel()
	for name, bound := range map[string]func(*testing.T) (context.Context, DialOptions){
		"ctx deadline": func(t *testing.T) (context.Context, DialOptions) {
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			t.Cleanup(cancel)
			return ctx, DialOptions{}
		},
		"dial timeout": func(t *testing.T) (context.Context, DialOptions) {
			return t.Context(), DialOptions{Timeout: 300 * time.Millisecond}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			release := make(chan struct{})
			srv, ca := startSMTP(t, provider.SecurityStartTLS, smtptest.Options{
				OnMessage: func(smtptest.Envelope) { <-release },
			})
			defer close(release) // lets the server goroutine finish before its Close
			ctx, opts := bound(t)
			conn := dialT(t, srv, ca, opts)
			start := time.Now()
			err := conn.Send(ctx, user.Username, rcpts, msg)
			se := asSendError(t, err)
			if se.Outcome != Unknown {
				t.Errorf("Outcome = %v, want Unknown (err %v)", se.Outcome, err)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("err = %v, want context.DeadlineExceeded in the chain", err)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Errorf("Send took %v, want it bounded by 300ms", elapsed)
			}
		})
	}
}

func TestSendCanceledBeforeStartIsNotSent(t *testing.T) {
	t.Parallel()
	srv, ca := startSMTP(t, provider.SecurityTLS, smtptest.Options{})
	conn := dialT(t, srv, ca, DialOptions{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := conn.Send(ctx, user.Username, rcpts, msg)
	if se := asSendError(t, err); se.Outcome != NotSentTemporary {
		t.Errorf("Outcome = %v, want NotSentTemporary (err %v)", se.Outcome, err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled in the chain", err)
	}
	if n := len(srv.Messages()); n != 0 {
		t.Errorf("captured %d messages, want none", n)
	}
}

func TestSendNoRecipients(t *testing.T) {
	t.Parallel()
	srv, ca := startSMTP(t, provider.SecurityTLS, smtptest.Options{})
	conn := dialT(t, srv, ca, DialOptions{})
	if se := asSendError(t, conn.Send(t.Context(), user.Username, nil, msg)); se.Outcome != NotSentPermanent {
		t.Errorf("Outcome = %v, want NotSentPermanent", se.Outcome)
	}
	if n := len(srv.Messages()); n != 0 {
		t.Errorf("captured %d messages, want none", n)
	}
}

// Send submits once. A second call on the same Conn sends nothing.
func TestSendOnlyOnce(t *testing.T) {
	t.Parallel()
	srv, ca := startSMTP(t, provider.SecurityTLS, smtptest.Options{})
	conn := dialT(t, srv, ca, DialOptions{})
	if err := conn.Send(t.Context(), user.Username, rcpts, msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if se := asSendError(t, conn.Send(t.Context(), user.Username, rcpts, msg)); se.Outcome != NotSentPermanent {
		t.Errorf("second Send Outcome = %v, want NotSentPermanent", se.Outcome)
	}
	if n := len(srv.Messages()); n != 1 {
		t.Errorf("captured %d messages, want 1", n)
	}
}

func TestCloseWithoutSending(t *testing.T) {
	t.Parallel()
	srv, ca := startSMTP(t, provider.SecurityStartTLS, smtptest.Options{})
	conn := dialT(t, srv, ca, DialOptions{})
	if err := conn.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if se := asSendError(t, conn.Send(t.Context(), user.Username, rcpts, msg)); se.Outcome != NotSentPermanent {
		t.Errorf("Send after Close Outcome = %v, want NotSentPermanent", se.Outcome)
	}
	if n := len(srv.Messages()); n != 0 {
		t.Errorf("captured %d messages, want none", n)
	}
}

func TestDialRejected(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]auth.Credentials{
		"password": creds(provider.AuthPassword, "wrong-pw-7f3a"),
		"xoauth2":  creds(provider.AuthOAuth2, "wrong-tok-c81e"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, ca := startSMTP(t, provider.SecurityStartTLS, smtptest.Options{})
			conn, err := Dial(t.Context(), srv.Endpoint(), c, DialOptions{RootCAs: ca.Pool})
			if conn != nil {
				t.Error("Dial returned a Conn with its error")
			}
			if !errors.Is(err, auth.ErrRejected) {
				t.Errorf("err = %v, want auth.ErrRejected", err)
			}
			assertNoSecret(t, err, c)
		})
	}
}

func TestSendErrorText(t *testing.T) {
	t.Parallel()
	se := &SendError{
		Outcome: NotSentPermanent,
		Code:    550,
		Refused: []string{"secret-rcpt@example.com"},
		Err:     errors.New("cause"),
	}
	msg := se.Error()
	if strings.Contains(msg, "secret-rcpt") {
		t.Errorf("Error() = %q, names a recipient", msg)
	}
	for _, want := range []string{"permanent", "550", "cause"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, want it to mention %q", msg, want)
		}
	}
	if !errors.Is(se, se.Err) {
		t.Error("SendError does not unwrap to Err")
	}
}

// Servers echo addresses in their replies. Error shows a reply's code and
// the step, never its text, which stays reachable through Unwrap.
func TestSendErrorTextHidesServerReplyText(t *testing.T) {
	t.Parallel()
	reply := &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "<secret-rcpt@example.com>: no such user"}
	se := failure(reply, false, "RCPT TO")
	se.Refused = []string{"secret-rcpt@example.com"}
	msg := se.Error()
	if strings.Contains(msg, "secret-rcpt") || strings.Contains(msg, "no such user") {
		t.Errorf("Error() = %q, shows the server's reply text", msg)
	}
	for _, want := range []string{"permanent", "550", "RCPT TO", "1 recipient(s) refused"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, want it to mention %q", msg, want)
		}
	}
	if got, ok := errors.AsType[*smtp.SMTPError](se); !ok || got.Message != reply.Message {
		t.Errorf("errors.AsType = %v, want the server's reply in the chain", got)
	}
}

func TestOutcomeString(t *testing.T) {
	t.Parallel()
	for o, want := range map[Outcome]string{
		NotSentPermanent: "not sent (permanent)",
		NotSentTemporary: "not sent (temporary)",
		Unknown:          "unknown",
		Outcome(42):      "Outcome(42)",
	} {
		if got := o.String(); got != want {
			t.Errorf("Outcome(%d).String() = %q, want %q", int(o), got, want)
		}
	}
}

func TestFailure(t *testing.T) {
	t.Parallel()
	lost := errors.New("connection reset")
	tests := []struct {
		name     string
		err      error
		afterDot bool
		want     Outcome
		wantCode int
	}{
		{name: "5xx before the dot", err: smtpErr(550, smtp.EnhancedCode{5, 0, 0}), want: NotSentPermanent, wantCode: 550},
		{name: "5xx after the dot", err: smtpErr(554, smtp.EnhancedCode{5, 0, 0}), afterDot: true, want: NotSentPermanent, wantCode: 554},
		{name: "4xx before the dot", err: smtpErr(421, smtp.EnhancedCode{4, 0, 0}), want: NotSentTemporary, wantCode: 421},
		{name: "4xx after the dot", err: smtpErr(452, smtp.EnhancedCode{4, 3, 1}), afterDot: true, want: NotSentTemporary, wantCode: 452},
		{name: "odd reply before the dot", err: smtpErr(250, smtp.EnhancedCode{2, 0, 0}), want: NotSentTemporary, wantCode: 250},
		{name: "odd reply after the dot", err: smtpErr(251, smtp.EnhancedCode{2, 0, 0}), afterDot: true, want: Unknown, wantCode: 251},
		{name: "lost before the dot", err: lost, want: NotSentTemporary},
		{name: "lost after the dot", err: lost, afterDot: true, want: Unknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			se := failure(tt.err, tt.afterDot, "step")
			if se.Outcome != tt.want || se.Code != tt.wantCode {
				t.Errorf("failure(%v, %v) = %v code %d, want %v code %d", tt.err, tt.afterDot, se.Outcome, se.Code, tt.want, tt.wantCode)
			}
			if !errors.Is(se, tt.err) {
				t.Errorf("failure(%v) = %v, does not wrap the cause", tt.err, se)
			}
		})
	}
}

// Only a write that fails between startMessage and the next read counts as
// a failed message write.
func TestTrackedConnRecordsMessageWriteFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// before runs on a working pipe, then the far end closes and a write fails.
		before func(*trackedConn)
		want   bool
	}{
		{name: "before startMessage", before: func(*trackedConn) {}, want: false},
		{name: "after startMessage", before: func(tc *trackedConn) { tc.startMessage() }, want: true},
		{name: "after a good message write", before: func(tc *trackedConn) {
			tc.startMessage()
			if _, err := tc.Write([]byte("body")); err != nil {
				t.Errorf("Write: %v", err)
			}
		}, want: true},
		{name: "after the read that follows the message", before: func(tc *trackedConn) {
			tc.startMessage()
			if _, err := tc.Write([]byte("body\r\n.\r\n")); err != nil {
				t.Errorf("Write: %v", err)
			}
			if _, err := tc.Read(make([]byte, 8)); err != nil {
				t.Errorf("Read: %v", err)
			}
		}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, b := net.Pipe()
			tc := &trackedConn{Conn: a}
			go func() { // the far end: drain writes, answer once
				buf := make([]byte, 64)
				for {
					if _, err := b.Read(buf); err != nil {
						return
					}
					_, _ = b.Write([]byte("250 ok\r\n")) // only read in one case; the others leave it unread
				}
			}()
			_ = a.SetDeadline(time.Now().Add(5 * time.Second)) // a pipe always accepts a deadline
			tt.before(tc)
			if tc.messageWriteFailed() {
				t.Fatal("messageWriteFailed before any write failed")
			}
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := tc.Write([]byte("lost")); err == nil {
				t.Fatal("Write to a closed pipe succeeded")
			}
			if got := tc.messageWriteFailed(); got != tt.want {
				t.Errorf("messageWriteFailed() = %v, want %v", got, tt.want)
			}
			_ = a.Close() // nothing to do about a close error at test end
		})
	}
}

// Envelopes the client can tell are unsendable fail permanently, before
// anything reaches the server.
func TestSendRefusesBadEnvelopeLocally(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		from string
		to   []string
	}{
		{name: "CRLF in a recipient", from: user.Username, to: []string{"a@example.com", "b@example.com\r\nRCPT TO:<x@example.com>"}},
		{name: "CRLF in the sender", from: "me@example.com\r\nRSET", to: rcpts},
		{name: "non-ASCII without SMTPUTF8", from: user.Username, to: []string{"ü@example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, ca := startSMTP(t, provider.SecurityTLS, smtptest.Options{})
			conn := dialT(t, srv, ca, DialOptions{})
			se := asSendError(t, conn.Send(t.Context(), tt.from, tt.to, msg))
			if se.Outcome != NotSentPermanent || se.Code != 0 {
				t.Errorf("Outcome = %v code %d, want NotSentPermanent code 0 (err %v)", se.Outcome, se.Code, se)
			}
			if n := len(srv.Messages()); n != 0 {
				t.Errorf("captured %d messages, want none", n)
			}
		})
	}
}

// bigMsg is large enough that only the message body takes the client's
// writes past len(bigMsg) bytes: the handshake and commands before DATA come
// to a few kilobytes.
var bigMsg = []byte("From: me@example.com\r\nTo: a@example.com\r\nSubject: big\r\n\r\n" +
	strings.Repeat("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd\r\n", 1024))

// badRecord is a TLS application-data record that cannot decrypt. A TLS
// client that reads it answers with an alert, which is a write.
var badRecord = append([]byte{0x17, 0x03, 0x03, 0x00, 0x20}, make([]byte, 0x20)...)

var errInjected = errors.New("injected write failure")

// faultyTCP is the client's TCP connection, under TLS. Once the client has
// written at least failWritesFrom bytes, every later write fails. Once it has
// written at least garbleReadFrom bytes, its next read returns badRecord and
// every later write fails. Zero turns a fault off.
type faultyTCP struct {
	net.Conn
	failWritesFrom int
	garbleReadFrom int

	mu      sync.Mutex
	written int
	garbled bool
	failed  int    // writes refused
	pending []byte // the rest of badRecord, for reads with small buffers
}

func (c *faultyTCP) Write(b []byte) (int, error) {
	c.mu.Lock()
	fail := c.garbled || (c.failWritesFrom > 0 && c.written >= c.failWritesFrom)
	c.mu.Unlock()
	if fail {
		c.mu.Lock()
		c.failed++
		c.mu.Unlock()
		return 0, errInjected
	}
	n, err := c.Conn.Write(b)
	c.mu.Lock()
	c.written += n
	c.mu.Unlock()
	return n, err
}

func (c *faultyTCP) Read(b []byte) (int, error) {
	c.mu.Lock()
	if !c.garbled && c.garbleReadFrom > 0 && c.written >= c.garbleReadFrom {
		c.garbled = true
		c.pending = slices.Clone(badRecord)
	}
	if len(c.pending) > 0 {
		n := copy(b, c.pending)
		c.pending = c.pending[n:]
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	return c.Conn.Read(b)
}

// dialFaulty dials srv with the client's TCP connection wrapped by wrap.
func dialFaulty(t *testing.T, srv *smtptest.Server, ca *tlstest.CA, wrap func(net.Conn) net.Conn) *Conn {
	t.Helper()
	tcp := func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		nc, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return wrap(nc), nil
	}
	c, err := dial(t.Context(), srv.Endpoint(), creds(provider.AuthPassword, "pw"), DialOptions{RootCAs: ca.Pool}, tcp)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() }) // Send has already closed it
	return c
}

// The whole message and the final dot go out, then the TLS layer fails a
// write of its own while the client waits for the reply: here an alert
// answering a record it cannot decrypt, as it would a KeyUpdate. The server
// has the message, so the outcome is unknown, not "not sent".
func TestSendTLSWriteFailureWhileAwaitingDataReplyIsUnknown(t *testing.T) {
	t.Parallel()
	for _, sec := range []provider.Security{provider.SecurityTLS, provider.SecurityStartTLS} {
		t.Run(sec.String(), func(t *testing.T) {
			t.Parallel()
			got := make(chan smtptest.Envelope, 1)
			srv, ca := startSMTP(t, sec, smtptest.Options{OnMessage: func(e smtptest.Envelope) { got <- e }})
			var tcp *faultyTCP
			conn := dialFaulty(t, srv, ca, func(nc net.Conn) net.Conn {
				tcp = &faultyTCP{Conn: nc, garbleReadFrom: len(bigMsg)}
				return tcp
			})
			err := conn.Send(t.Context(), user.Username, rcpts, bigMsg)
			se := asSendError(t, err)
			if se.Outcome != Unknown {
				t.Errorf("Outcome = %v, want Unknown (err %v)", se.Outcome, err)
			}
			tcp.mu.Lock()
			failed := tcp.failed
			tcp.mu.Unlock()
			if failed == 0 {
				t.Error("no write failed after the garbled read, so the fault did not happen")
			}
			select {
			case e := <-got:
				if !bytes.Equal(e.Data, bigMsg) {
					t.Errorf("server captured %d bytes, want the %d-byte message", len(e.Data), len(bigMsg))
				}
			case <-time.After(5 * time.Second):
				t.Error("the server never got the message")
			}
		})
	}
}

// A write that fails partway through the message means the final dot never
// went out: not sent.
func TestSendWriteFailureMidMessageIsNotSent(t *testing.T) {
	t.Parallel()
	for _, sec := range []provider.Security{provider.SecurityTLS, provider.SecurityStartTLS} {
		t.Run(sec.String(), func(t *testing.T) {
			t.Parallel()
			srv, ca := startSMTP(t, sec, smtptest.Options{})
			conn := dialFaulty(t, srv, ca, func(nc net.Conn) net.Conn {
				return &faultyTCP{Conn: nc, failWritesFrom: len(bigMsg) / 2}
			})
			err := conn.Send(t.Context(), user.Username, rcpts, bigMsg)
			if se := asSendError(t, err); se.Outcome != NotSentTemporary {
				t.Errorf("Outcome = %v, want NotSentTemporary (err %v)", se.Outcome, err)
			}
			if !errors.Is(err, errInjected) {
				t.Errorf("err = %v, want the failed write in the chain", err)
			}
			if n := len(srv.Messages()); n != 0 {
				t.Errorf("captured %d messages, want none", n)
			}
		})
	}
}
