package smtptest

import (
	"bytes"
	"crypto/tls"
	"errors"
	"slices"
	"testing"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

var tester = User{Username: "me@example.com", Password: "pw"}

// A message with CRLF line endings and a line that starts with a dot, so the
// capture proves dot-stuffing is undone and nothing else changes.
var raw = []byte("From: me@example.com\r\nTo: a@example.com\r\nSubject: hi\r\n\r\n.leading dot\r\nbody\r\n")

// dial opens an authenticated go-smtp client on ep.
func dial(t *testing.T, ca *tlstest.CA, ep provider.Endpoint, login bool) *smtp.Client {
	t.Helper()
	cfg := &tls.Config{ServerName: ep.Host, RootCAs: ca.Pool, MinVersion: tls.VersionTLS12}
	var (
		c   *smtp.Client
		err error
	)
	switch ep.Security {
	case provider.SecurityTLS:
		c, err = smtp.DialTLS(ep.Addr(), cfg)
	default:
		c, err = smtp.DialStartTLS(ep.Addr(), cfg)
	}
	if err != nil {
		t.Fatalf("dialing %s: %v", ep.Addr(), err)
	}
	t.Cleanup(func() { _ = c.Close() }) // the test may already have closed or quit it
	if login {
		if err := c.Auth(sasl.NewPlainClient("", tester.Username, tester.Password)); err != nil {
			t.Fatalf("AUTH: %v", err)
		}
	}
	return c
}

// submit runs MAIL, RCPT for each of to, and DATA with raw, returning the
// first error and the step that produced it.
func submit(c *smtp.Client, from string, to []string, data []byte) (string, error) {
	if err := c.Mail(from, nil); err != nil {
		return "MAIL", err
	}
	for _, r := range to {
		if err := c.Rcpt(r, nil); err != nil {
			return "RCPT", err
		}
	}
	w, err := c.Data()
	if err != nil {
		return "DATA", err
	}
	if _, err := w.Write(data); err != nil {
		return "write", err
	}
	if err := w.Close(); err != nil {
		return "end of data", err
	}
	return "", nil
}

func TestStartWithCaptures(t *testing.T) {
	t.Parallel()
	for _, sec := range []provider.Security{provider.SecurityTLS, provider.SecurityStartTLS} {
		t.Run(sec.String(), func(t *testing.T) {
			t.Parallel()
			ca := tlstest.New(t)
			seen := make(chan Envelope, 2) // OnMessage runs on the server goroutine
			srv := StartWith(t, ca, sec, Options{OnMessage: func(e Envelope) { seen <- e }}, tester)
			c := dial(t, ca, srv.Endpoint(), true)
			to := []string{"a@example.com", "b@example.com"}
			if step, err := submit(c, tester.Username, to, raw); err != nil {
				t.Fatalf("%s: %v", step, err)
			}
			if err := c.Quit(); err != nil {
				t.Fatalf("QUIT: %v", err)
			}
			want := Envelope{From: tester.Username, To: to, Data: raw}
			got := srv.Messages()
			if len(got) != 1 || !sameEnvelope(got[0], want) {
				t.Fatalf("Messages() = %+v, want [%+v]", got, want)
			}
			if n := len(seen); n != 1 {
				t.Fatalf("OnMessage ran %d times, want once", n)
			}
			if e := <-seen; !sameEnvelope(e, want) {
				t.Errorf("OnMessage saw %+v, want %+v", e, want)
			}
		})
	}
}

func sameEnvelope(a, b Envelope) bool {
	return a.From == b.From && slices.Equal(a.To, b.To) && bytes.Equal(a.Data, b.Data)
}

// Messages hands out copies, so a caller cannot change what the server captured.
func TestMessagesIsACopy(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	srv := StartWith(t, ca, provider.SecurityTLS, Options{}, tester)
	c := dial(t, ca, srv.Endpoint(), true)
	if step, err := submit(c, tester.Username, []string{"a@example.com"}, raw); err != nil {
		t.Fatalf("%s: %v", step, err)
	}
	got := srv.Messages()
	got[0].To[0] = "changed"
	got[0].Data[0] = 'X'
	again := srv.Messages()
	if again[0].To[0] != "a@example.com" || !bytes.Equal(again[0].Data, raw) {
		t.Errorf("Messages() = %+v after the caller changed its copy", again)
	}
}

func TestStartStillWorks(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	ep := Start(t, ca, provider.SecurityStartTLS, tester)
	c := dial(t, ca, ep, true)
	if step, err := submit(c, tester.Username, []string{"a@example.com"}, raw); err != nil {
		t.Fatalf("%s: %v", step, err)
	}
}

func TestMailNeedsAuth(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	srv := StartWith(t, ca, provider.SecurityTLS, Options{}, tester)
	c := dial(t, ca, srv.Endpoint(), false)
	step, err := submit(c, tester.Username, []string{"a@example.com"}, raw)
	if smtpErr, ok := errors.AsType[*smtp.SMTPError](err); step != "MAIL" || !ok || smtpErr.Code != 530 {
		t.Errorf("%s: %v, want 530 at MAIL", step, err)
	}
	if n := len(srv.Messages()); n != 0 {
		t.Errorf("captured %d messages, want none", n)
	}
}

func TestFaults(t *testing.T) {
	t.Parallel()
	mailErr := &smtp.SMTPError{Code: 553, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "sender refused"}
	rcptErr := &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such user"}
	dataErr := &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "message refused"}
	to := []string{"a@example.com", "b@example.com"}
	tests := []struct {
		name         string
		opts         Options
		wantStep     string
		wantCode     int // 0 means a lost connection rather than an SMTP reply
		wantCaptured bool
	}{
		{name: "FailMail", opts: Options{FailMail: mailErr}, wantStep: "MAIL", wantCode: 553},
		{name: "FailRcpt", opts: Options{FailRcpt: map[string]*smtp.SMTPError{"b@example.com": rcptErr}}, wantStep: "RCPT", wantCode: 550},
		{name: "FailData", opts: Options{FailData: dataErr}, wantStep: "end of data", wantCode: 554},
		{name: "DropAtRcpt", opts: Options{DropAtRcpt: "b@example.com"}, wantStep: "RCPT"},
		{name: "DropBeforeDataEnd", opts: Options{DropBeforeDataEnd: true}, wantStep: "DATA"},
		{name: "DropAfterDataEnd", opts: Options{DropAfterDataEnd: true}, wantStep: "end of data", wantCaptured: true},
	}
	for _, sec := range []provider.Security{provider.SecurityTLS, provider.SecurityStartTLS} {
		for _, tt := range tests {
			t.Run(sec.String()+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				ca := tlstest.New(t)
				srv := StartWith(t, ca, sec, tt.opts, tester)
				c := dial(t, ca, srv.Endpoint(), true)
				step, err := submit(c, tester.Username, to, raw)
				if step != tt.wantStep {
					t.Fatalf("failed at %q with %v, want a failure at %q", step, err, tt.wantStep)
				}
				smtpErr, isReply := errors.AsType[*smtp.SMTPError](err)
				switch {
				case tt.wantCode != 0 && (!isReply || smtpErr.Code != tt.wantCode):
					t.Errorf("err = %v, want SMTP %d", err, tt.wantCode)
				case tt.wantCode == 0 && isReply:
					t.Errorf("err = %v, want a lost connection, not a reply", err)
				}
				got := srv.Messages()
				if tt.wantCaptured {
					if len(got) != 1 || !bytes.Equal(got[0].Data, raw) {
						t.Errorf("Messages() = %+v, want the one message", got)
					}
				} else if len(got) != 0 {
					t.Errorf("Messages() = %+v, want none", got)
				}
			})
		}
	}
}

// A refused recipient does not stop the others being tried, so a client can
// collect every refusal before it gives up.
func TestFailRcptOnlyRefusesThatAddress(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	rcptErr := &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such user"}
	srv := StartWith(t, ca, provider.SecurityTLS, Options{FailRcpt: map[string]*smtp.SMTPError{"b@example.com": rcptErr}}, tester)
	c := dial(t, ca, srv.Endpoint(), true)
	if err := c.Mail(tester.Username, nil); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	for addr, wantErr := range map[string]bool{"a@example.com": false, "b@example.com": true, "c@example.com": false} {
		if err := c.Rcpt(addr, nil); (err != nil) != wantErr {
			t.Errorf("RCPT %s: %v, want error %v", addr, err, wantErr)
		}
	}
}

// DropBeforeDataEnd closes the connection in place of the 354 reply, so the
// client never gets to write the final dot.
func TestDropBeforeDataEndClosesInsteadOf354(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	srv := StartWith(t, ca, provider.SecurityStartTLS, Options{DropBeforeDataEnd: true}, tester)
	c := dial(t, ca, srv.Endpoint(), true)
	if err := c.Mail(tester.Username, nil); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	for _, addr := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		if err := c.Rcpt(addr, nil); err != nil {
			t.Fatalf("RCPT %s: %v", addr, err)
		}
	}
	_, err := c.Data()
	if err == nil {
		t.Fatal("DATA got its 354")
	}
	if _, ok := errors.AsType[*smtp.SMTPError](err); ok {
		t.Errorf("DATA: %v, want a lost connection", err)
	}
}

// DropAtRcpt answers the recipients before it, then loses the connection in
// place of the reply for its address.
func TestDropAtRcptAnswersEarlierRecipients(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	rcptErr := &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such user"}
	srv := StartWith(t, ca, provider.SecurityTLS, Options{
		FailRcpt:   map[string]*smtp.SMTPError{"a@example.com": rcptErr},
		DropAtRcpt: "b@example.com",
	}, tester)
	c := dial(t, ca, srv.Endpoint(), true)
	if err := c.Mail(tester.Username, nil); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if smtpErr, ok := errors.AsType[*smtp.SMTPError](c.Rcpt("a@example.com", nil)); !ok || smtpErr.Code != 550 {
		t.Fatalf("RCPT a: %v, want 550", smtpErr)
	}
	err := c.Rcpt("b@example.com", nil)
	if err == nil {
		t.Fatal("RCPT b got a reply")
	}
	if _, ok := errors.AsType[*smtp.SMTPError](err); ok {
		t.Errorf("RCPT b: %v, want a lost connection", err)
	}
}
