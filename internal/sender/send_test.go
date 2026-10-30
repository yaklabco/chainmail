package sender

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
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

func TestTrackedConnRecordsWriteFailure(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	tc := &trackedConn{Conn: a}
	go func() { _, _ = io.Copy(io.Discard, b) }() // drains until b closes
	if _, err := tc.Write([]byte("ok")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if tc.writeFailed.Load() {
		t.Fatal("writeFailed after a good write")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := tc.Write([]byte("lost")); err == nil {
		t.Fatal("Write to a closed pipe succeeded")
	}
	if !tc.writeFailed.Load() {
		t.Error("writeFailed not set after a failed write")
	}
	_ = a.Close() // nothing to do about a close error at test end
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
