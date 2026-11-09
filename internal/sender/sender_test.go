package sender

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/testutil/smtptest"
	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
)

var user = smtptest.User{Username: "me@example.com", Password: "pw", Token: "tok"}

func creds(method provider.AuthMethod, secret string) auth.Credentials {
	return auth.Credentials{Method: method, Username: user.Username, Secret: secret}
}

func TestProbe(t *testing.T) {
	t.Parallel()
	for _, sec := range []provider.Security{provider.SecurityTLS, provider.SecurityStartTLS} {
		for name, c := range map[string]auth.Credentials{
			"password": creds(provider.AuthPassword, "pw"),
			"xoauth2":  creds(provider.AuthOAuth2, "tok"),
		} {
			t.Run(sec.String()+"/"+name, func(t *testing.T) {
				t.Parallel()
				ca := tlstest.New(t)
				ep := smtptest.Start(t, ca, sec, user)
				if err := Probe(t.Context(), ep, c, DialOptions{RootCAs: ca.Pool}); err != nil {
					t.Errorf("Probe: %v", err)
				}
			})
		}
	}
}

func TestProbeRejected(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]auth.Credentials{
		"password": creds(provider.AuthPassword, "wrong-pw-7f3a"),
		"xoauth2":  creds(provider.AuthOAuth2, "wrong-tok-c81e"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ca := tlstest.New(t)
			ep := smtptest.Start(t, ca, provider.SecurityStartTLS, user)
			err := Probe(t.Context(), ep, c, DialOptions{RootCAs: ca.Pool})
			if !errors.Is(err, auth.ErrRejected) {
				t.Errorf("err = %v, want auth.ErrRejected", err)
			}
			var smtpErr *smtp.SMTPError
			if !errors.As(err, &smtpErr) || smtpErr.Code != 535 {
				t.Errorf("err = %v, want the server's 535 in the chain", err)
			}
			assertNoSecret(t, err, c)
		})
	}
}

// assertNoSecret fails if err's text holds the secret, raw or inside the
// base64 SASL initial response that carried it.
func assertNoSecret(t *testing.T, err error, c auth.Credentials) {
	t.Helper()
	if err == nil {
		return
	}
	_, ir, startErr := c.SASL().Start()
	if startErr != nil {
		t.Fatalf("starting SASL: %v", startErr)
	}
	msg := err.Error()
	if strings.Contains(msg, c.Secret) || strings.Contains(msg, base64.StdEncoding.EncodeToString(ir)) {
		t.Errorf("err = %q, leaks the secret", msg)
	}
}

func TestProbeServerTroubleIsNotRejection(t *testing.T) {
	t.Parallel()
	for name, serverErr := range map[string]*smtp.SMTPError{
		"454": {Code: 454, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "temporary authentication failure"},
		"421": {Code: 421, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "try again later"},
	} {
		troubled := user
		troubled.AuthError = serverErr
		for method, c := range map[string]auth.Credentials{
			"password": creds(provider.AuthPassword, "pw"),
			"xoauth2":  creds(provider.AuthOAuth2, "tok"),
		} {
			t.Run(name+"/"+method, func(t *testing.T) {
				t.Parallel()
				ca := tlstest.New(t)
				ep := smtptest.Start(t, ca, provider.SecurityStartTLS, troubled)
				err := Probe(t.Context(), ep, c, DialOptions{RootCAs: ca.Pool})
				if err == nil {
					t.Fatal("Probe succeeded")
				}
				if errors.Is(err, auth.ErrRejected) {
					t.Errorf("err = %v, should not be a credential rejection", err)
				}
				var smtpErr *smtp.SMTPError
				if !errors.As(err, &smtpErr) || smtpErr.Code != serverErr.Code {
					t.Errorf("err = %v, want the server's %d in the chain", err, serverErr.Code)
				}
			})
		}
	}
}

func TestAuthError(t *testing.T) {
	t.Parallel()
	reply := func(code int, enh smtp.EnhancedCode) *smtp.SMTPError {
		return &smtp.SMTPError{Code: code, EnhancedCode: enh, Message: "server says no"}
	}
	tests := []struct {
		name         string
		err          *smtp.SMTPError
		wantRejected bool
	}{
		{name: "535 bad credentials", err: reply(535, smtp.EnhancedCode{5, 7, 8}), wantRejected: true},
		{name: "534 app password or web login required", err: reply(534, smtp.EnhancedCode{5, 7, 9}), wantRejected: true},
		{name: "454 temporary authentication failure", err: reply(454, smtp.EnhancedCode{4, 7, 0})},
		{name: "421 service not available", err: reply(421, smtp.EnhancedCode{4, 7, 0})},
		{name: "432 password transition needed", err: reply(432, smtp.EnhancedCode{4, 7, 12})},
		{name: "504 unsupported mechanism", err: reply(504, smtp.EnhancedCode{5, 5, 4})},
		{name: "530 authentication required", err: reply(530, smtp.EnhancedCode{5, 7, 0})},
		{name: "538 encryption required", err: reply(538, smtp.EnhancedCode{5, 7, 11})},
		{name: "501 cancelled", err: reply(501, smtp.EnhancedCode{5, 0, 0})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := authError(tt.err)
			if got := errors.Is(err, auth.ErrRejected); got != tt.wantRejected {
				t.Errorf("errors.Is(%v, auth.ErrRejected) = %v, want %v", err, got, tt.wantRejected)
			}
			var smtpErr *smtp.SMTPError
			if !errors.As(err, &smtpErr) || smtpErr.Code != tt.err.Code {
				t.Errorf("err = %v, want the *smtp.SMTPError with code %d in the chain", err, tt.err.Code)
			}
		})
	}
}

func TestAuthErrorPlainError(t *testing.T) {
	t.Parallel()
	if err := authError(nil); err != nil {
		t.Errorf("authError(nil) = %v, want nil", err)
	}
	cause := errors.New("connection reset")
	err := authError(cause)
	if !errors.Is(err, cause) || errors.Is(err, auth.ErrRejected) {
		t.Errorf("authError(%v) = %v, want it wrapped and not a rejection", cause, err)
	}
}

func TestProbeFailuresAreNotRejections(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	tlsEp := smtptest.Start(t, ca, provider.SecurityTLS, user)
	startTLSEp := smtptest.Start(t, ca, provider.SecurityStartTLS, user)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", ln.Addr())
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name  string
		ctx   context.Context
		ep    provider.Endpoint
		creds auth.Credentials
		opts  DialOptions
	}{
		{name: "untrusted certificate", ctx: t.Context(), ep: tlsEp, opts: DialOptions{RootCAs: x509.NewCertPool()}},
		{name: "untrusted certificate over STARTTLS", ctx: t.Context(), ep: startTLSEp, opts: DialOptions{RootCAs: x509.NewCertPool()}},
		{name: "connection refused", ctx: t.Context(), ep: provider.Endpoint{Host: "127.0.0.1", Port: closed.Port, Security: provider.SecurityTLS}, opts: DialOptions{RootCAs: ca.Pool}},
		{name: "canceled context", ctx: canceled, ep: tlsEp, opts: DialOptions{RootCAs: ca.Pool}},
		{name: "unsupported security", ctx: t.Context(), ep: provider.Endpoint{Host: "127.0.0.1", Port: tlsEp.Port}, opts: DialOptions{RootCAs: ca.Pool}},
		{
			name: "unsupported auth method", ctx: t.Context(), ep: tlsEp, opts: DialOptions{RootCAs: ca.Pool},
			creds: auth.Credentials{Method: "kerberos", Username: user.Username, Secret: "pw"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := tt.creds
			if c.Method == "" {
				c = creds(provider.AuthPassword, "pw")
			}
			err := Probe(tt.ctx, tt.ep, c, tt.opts)
			if err == nil {
				t.Fatal("Probe succeeded")
			}
			if errors.Is(err, auth.ErrRejected) {
				t.Errorf("err = %v, should not be a credential rejection", err)
			}
		})
	}
}

func TestProbeIsBoundedByTimeout(t *testing.T) {
	t.Parallel()
	ep := startSilentServer(t)
	start := time.Now()
	err := Probe(t.Context(), ep, creds(provider.AuthPassword, "pw"), DialOptions{Timeout: 300 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if errors.Is(err, auth.ErrRejected) {
		t.Errorf("err = %v, should not be a credential rejection", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Probe took %v, want it bounded by the 300ms timeout", elapsed)
	}
}

// startSilentServer accepts one connection and never sends the SMTP greeting.
func startSilentServer(t *testing.T) provider.Endpoint {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = ln.Close() // ends Accept; nothing to do about a close error at test end
		<-done
	})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }() // the client may already have hung up
		<-t.Context().Done()
	}()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", ln.Addr())
	}
	return provider.Endpoint{Host: "127.0.0.1", Port: addr.Port, Security: provider.SecurityStartTLS}
}

// An untrusted certificate offered after STARTTLS is a TLS failure, reported
// as such, not as an AUTH problem.
func TestProbeUntrustedSTARTTLSIsATLSError(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	ep := smtptest.Start(t, ca, provider.SecurityStartTLS, user)
	err := Probe(t.Context(), ep, creds(provider.AuthPassword, "pw"), DialOptions{RootCAs: x509.NewCertPool()})
	if err == nil {
		t.Fatal("Probe succeeded")
	}
	if errors.Is(err, auth.ErrRejected) {
		t.Errorf("err = %v, should not be a credential rejection", err)
	}
	if msg := err.Error(); strings.Contains(msg, "AUTH") || !strings.Contains(msg, "STARTTLS") {
		t.Errorf("err = %q, want a STARTTLS failure that does not mention AUTH", msg)
	}
	var unknownCA x509.UnknownAuthorityError
	if !errors.As(err, &unknownCA) {
		t.Errorf("err = %v, want x509.UnknownAuthorityError in the chain", err)
	}
}

// A server that offers AUTH but not STARTTLS on a STARTTLS endpoint must not
// get the credentials over plaintext.
func TestProbeFailsClosedWithoutSTARTTLS(t *testing.T) {
	t.Parallel()
	ep, commands := startNoSTARTTLSServer(t)
	err := Probe(t.Context(), ep, creds(provider.AuthPassword, "pw-plaintext-4d2b"), DialOptions{})
	if err == nil {
		t.Fatal("Probe succeeded")
	}
	if errors.Is(err, auth.ErrRejected) {
		t.Errorf("err = %v, should not be a credential rejection", err)
	}
	var got []string
	select {
	case got = <-commands:
	case <-time.After(5 * time.Second):
		t.Fatal("the fake server never saw the connection end")
	}
	if len(got) == 0 {
		t.Fatal("the fake server received no commands; the test is not exercising the probe")
	}
	for _, cmd := range got {
		if strings.HasPrefix(strings.ToUpper(cmd), "AUTH") {
			t.Errorf("server received %q over plaintext", cmd)
		}
	}
}

// startNoSTARTTLSServer runs a one-connection plaintext SMTP fake whose EHLO
// advertises AUTH but not STARTTLS. Once the client hangs up it sends every
// command line it received on the returned channel.
func startNoSTARTTLSServer(t *testing.T) (provider.Endpoint, <-chan []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	commands := make(chan []string, 1)
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = ln.Close() // ends Accept; nothing to do about a close error at test end
		<-done
	})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }() // the client may already have hung up
		stop := context.AfterFunc(t.Context(), func() { _ = conn.Close() })
		defer stop()
		commands <- serveNoSTARTTLS(conn)
	}()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", ln.Addr())
	}
	return provider.Endpoint{Host: "127.0.0.1", Port: addr.Port, Security: provider.SecurityStartTLS}, commands
}

func serveNoSTARTTLS(conn net.Conn) []string {
	var received []string
	w := bufio.NewWriter(conn)
	reply := func(lines ...string) bool {
		for _, l := range lines {
			if _, err := w.WriteString(l + "\r\n"); err != nil {
				return false
			}
		}
		return w.Flush() == nil
	}
	if !reply("220 localhost ESMTP ready") {
		return received
	}
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return received
		}
		line = strings.TrimRight(line, "\r\n")
		received = append(received, line)
		verb, _, _ := strings.Cut(line, " ")
		var ok bool
		switch strings.ToUpper(verb) {
		case "EHLO":
			ok = reply("250-localhost", "250 AUTH PLAIN XOAUTH2")
		case "AUTH":
			ok = reply("235 2.7.0 accepted")
		case "QUIT":
			reply("221 2.0.0 bye")
			return received
		default:
			ok = reply("502 5.5.1 not implemented")
		}
		if !ok {
			return received
		}
	}
}

// An endpoint with a TLS server name verifies the certificate against that
// name, not the host dialed.
func TestProbeVerifiesTLSServerName(t *testing.T) {
	t.Parallel()
	for _, sec := range []provider.Security{provider.SecurityTLS, provider.SecurityStartTLS} {
		t.Run(sec.String(), func(t *testing.T) {
			t.Parallel()
			ca := tlstest.NewFor(t, "mail.example.test")
			ep := smtptest.Start(t, ca, sec, user)
			if err := Probe(t.Context(), ep, creds(provider.AuthPassword, "pw"), DialOptions{RootCAs: ca.Pool}); err == nil {
				t.Fatal("Probe by address succeeded against a certificate for another name")
			}
			ep.ServerName = "mail.example.test"
			if err := Probe(t.Context(), ep, creds(provider.AuthPassword, "pw"), DialOptions{RootCAs: ca.Pool}); err != nil {
				t.Fatalf("Probe with server name: %v", err)
			}
			ep.ServerName = "other.example.test"
			err := Probe(t.Context(), ep, creds(provider.AuthPassword, "pw"), DialOptions{RootCAs: ca.Pool})
			var hostErr x509.HostnameError
			if !errors.As(err, &hostErr) {
				t.Errorf("Probe with the wrong server name: err = %v, want x509.HostnameError", err)
			}
		})
	}
}
