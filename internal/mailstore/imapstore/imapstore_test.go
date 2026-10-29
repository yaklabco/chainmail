package imapstore

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/testutil/imaptest"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

var user = imaptest.User{Username: "me@example.com", Password: "pw", Token: "tok"}

func password(secret string) auth.Credentials {
	return auth.Credentials{Method: provider.AuthPassword, Username: user.Username, Secret: secret}
}

func oauth(token string) auth.Credentials {
	return auth.Credentials{Method: provider.AuthOAuth2, Username: user.Username, Secret: token}
}

func TestDial(t *testing.T) {
	t.Parallel()
	for _, sec := range []provider.Security{provider.SecurityTLS, provider.SecurityStartTLS} {
		for name, creds := range map[string]auth.Credentials{"password": password("pw"), "xoauth2": oauth("tok")} {
			t.Run(sec.String()+"/"+name, func(t *testing.T) {
				t.Parallel()
				ca := tlstest.New(t)
				ep := imaptest.Start(t, ca, sec, user)
				s, err := Dial(t.Context(), ep, creds, DialOptions{RootCAs: ca.Pool})
				if err != nil {
					t.Fatalf("Dial: %v", err)
				}
				caps := s.Capabilities()
				if !slices.Contains(caps, "IMAP4rev1") {
					t.Fatalf("Capabilities() = %v, want IMAP4rev1", caps)
				}
				caps[0] = "MUTATED"
				if slices.Contains(s.Capabilities(), "MUTATED") {
					t.Error("Capabilities() returned the stored slice, not a copy")
				}
				if err := s.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			})
		}
	}
}

func TestDialRejected(t *testing.T) {
	t.Parallel()
	for name, creds := range map[string]auth.Credentials{"password": password("wrong-pw-5e1c"), "xoauth2": oauth("wrong-tok-9b2d")} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ca := tlstest.New(t)
			ep := imaptest.Start(t, ca, provider.SecurityTLS, user)
			_, err := Dial(t.Context(), ep, creds, DialOptions{RootCAs: ca.Pool})
			if !errors.Is(err, auth.ErrRejected) {
				t.Errorf("err = %v, want auth.ErrRejected", err)
			}
			if err != nil && strings.Contains(err.Error(), creds.Secret) {
				t.Errorf("err = %q, leaks the secret", err)
			}
		})
	}
}

func TestDialFailuresAreNotRejections(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	ep := imaptest.Start(t, ca, provider.SecurityTLS, user)
	startTLSEp := imaptest.Start(t, ca, provider.SecurityStartTLS, user)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed, _ := ln.Addr().(*net.TCPAddr)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name string
		ctx  context.Context
		ep   provider.Endpoint
		opts DialOptions
	}{
		{name: "untrusted certificate", ctx: t.Context(), ep: ep, opts: DialOptions{RootCAs: x509.NewCertPool()}},
		{name: "untrusted certificate over STARTTLS", ctx: t.Context(), ep: startTLSEp, opts: DialOptions{RootCAs: x509.NewCertPool()}},
		{name: "connection refused", ctx: t.Context(), ep: provider.Endpoint{Host: "127.0.0.1", Port: closed.Port, Security: provider.SecurityTLS}, opts: DialOptions{RootCAs: ca.Pool}},
		{name: "canceled context", ctx: canceled, ep: ep, opts: DialOptions{RootCAs: ca.Pool}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Dial(tt.ctx, tt.ep, password("pw"), tt.opts)
			if err == nil {
				t.Fatal("Dial succeeded")
			}
			if errors.Is(err, auth.ErrRejected) {
				t.Errorf("err = %v, should not be a credential rejection", err)
			}
		})
	}
}

func TestLoginError(t *testing.T) {
	t.Parallel()
	no := func(code imap.ResponseCode) *imap.Error {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: code, Text: "server says no"}
	}
	tests := []struct {
		name         string
		err          *imap.Error
		wantRejected bool
	}{
		{name: "NO without a code", err: no(""), wantRejected: true},
		{name: "AUTHENTICATIONFAILED", err: no(imap.ResponseCodeAuthenticationFailed), wantRejected: true},
		{name: "AUTHORIZATIONFAILED", err: no(imap.ResponseCodeAuthorizationFailed), wantRejected: true},
		{name: "EXPIRED", err: no(imap.ResponseCodeExpired), wantRejected: true},
		{name: "WEBALERT", err: no("WEBALERT"), wantRejected: true},
		{name: "UNAVAILABLE", err: no(imap.ResponseCodeUnavailable)},
		{name: "INUSE", err: no(imap.ResponseCodeInUse)},
		{name: "LIMIT", err: no(imap.ResponseCodeLimit)},
		{name: "SERVERBUG", err: no(imap.ResponseCodeServerBug)},
		{name: "ALERT", err: no(imap.ResponseCodeAlert)},
		{name: "CONTACTADMIN", err: no(imap.ResponseCodeContactAdmin)},
		{name: "PRIVACYREQUIRED", err: no(imap.ResponseCodePrivacyRequired)},
		{name: "unknown code", err: no("SOMETHINGNEW")},
		{name: "BAD without a code", err: &imap.Error{Type: imap.StatusResponseTypeBad, Text: "syntax"}},
		{name: "BAD with AUTHENTICATIONFAILED", err: &imap.Error{Type: imap.StatusResponseTypeBad, Code: imap.ResponseCodeAuthenticationFailed}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := loginError(tt.err)
			if got := errors.Is(err, auth.ErrRejected); got != tt.wantRejected {
				t.Errorf("errors.Is(%v, auth.ErrRejected) = %v, want %v", err, got, tt.wantRejected)
			}
			var imapErr *imap.Error
			if !errors.As(err, &imapErr) || imapErr.Code != tt.err.Code {
				t.Errorf("err = %v, want the *imap.Error with code %q in the chain", err, tt.err.Code)
			}
		})
	}
}

func TestLoginErrorPlainError(t *testing.T) {
	t.Parallel()
	if err := loginError(nil); err != nil {
		t.Errorf("loginError(nil) = %v, want nil", err)
	}
	cause := errors.New("connection reset")
	err := loginError(cause)
	if !errors.Is(err, cause) || errors.Is(err, auth.ErrRejected) {
		t.Errorf("loginError(%v) = %v, want it wrapped and not a rejection", cause, err)
	}
}

func TestDialServerTroubleIsNotRejection(t *testing.T) {
	t.Parallel()
	troubled := user
	troubled.LoginError = &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeUnavailable, Text: "try again later"}
	for name, creds := range map[string]auth.Credentials{"password": password("pw"), "xoauth2": oauth("tok")} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ca := tlstest.New(t)
			ep := imaptest.Start(t, ca, provider.SecurityTLS, troubled)
			_, err := Dial(t.Context(), ep, creds, DialOptions{RootCAs: ca.Pool})
			if err == nil {
				t.Fatal("Dial succeeded")
			}
			if errors.Is(err, auth.ErrRejected) {
				t.Errorf("err = %v, should not be a credential rejection", err)
			}
			var imapErr *imap.Error
			if !errors.As(err, &imapErr) || imapErr.Code != imap.ResponseCodeUnavailable {
				t.Errorf("err = %v, want NO [UNAVAILABLE] in the chain", err)
			}
		})
	}
}

func TestDialFetchesCapabilitiesUnderTheDeadline(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	ep := startStallingServer(t, ca, "CAPABILITY")
	start := time.Now()
	s, err := Dial(t.Context(), ep, password("pw"), DialOptions{RootCAs: ca.Pool, Timeout: 300 * time.Millisecond})
	if err == nil {
		_ = s.Close() // the test has already failed; the close result adds nothing
		t.Fatal("Dial succeeded although the server never answered CAPABILITY")
	}
	if errors.Is(err, auth.ErrRejected) {
		t.Errorf("err = %v, should not be a credential rejection", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Dial took %v, want it bounded by the 300ms timeout", elapsed)
	}
}

func TestCloseIsBoundedWhenLogoutGoesUnanswered(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	ep := startStallingServer(t, ca, "LOGOUT")
	s, err := Dial(t.Context(), ep, password("pw"), DialOptions{RootCAs: ca.Pool})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	s.closeTimeout = 100 * time.Millisecond
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "LOGOUT unanswered") {
			t.Errorf("Close = %v, want an error saying LOGOUT went unanswered", err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("Close took %v, want it bounded by the 100ms close timeout", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close blocked although the close timeout is 100ms")
	}
}

// startStallingServer runs a one-connection IMAP server over implicit TLS
// that accepts any LOGIN without sending capabilities and never answers the
// command called stall. It answers everything else, so with stall set to
// CAPABILITY a Close still works.
func startStallingServer(t *testing.T, ca *tlstest.CA, stall string) provider.Endpoint {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", ca.Server)
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
		stop := context.AfterFunc(t.Context(), func() { _ = conn.Close() })
		defer stop()
		serveStalling(conn, stall)
	}()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", ln.Addr())
	}
	return provider.Endpoint{Host: "127.0.0.1", Port: addr.Port, Security: provider.SecurityTLS}
}

func serveStalling(conn net.Conn, stall string) {
	w := bufio.NewWriter(conn)
	reply := func(lines ...string) bool {
		for _, l := range lines {
			if _, err := w.WriteString(l + "\r\n"); err != nil {
				return false
			}
		}
		return w.Flush() == nil
	}
	if !reply("* OK [CAPABILITY IMAP4rev1] ready") {
		return
	}
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tag, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		cmd, _, _ := strings.Cut(rest, " ")
		cmd = strings.ToUpper(cmd)
		if cmd == stall {
			continue // left unanswered: the client has to give up on its own
		}
		switch cmd {
		case "LOGIN":
			if !reply(tag + " OK logged in") {
				return
			}
		case "CAPABILITY":
			if !reply("* CAPABILITY IMAP4rev1", tag+" OK done") {
				return
			}
		case "LOGOUT":
			reply("* BYE", tag+" OK bye")
			return
		default:
			if !reply(tag + " BAD unknown command") {
				return
			}
		}
	}
}

func TestClientOptionsDecodeEveryCharset(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, encoded, want string
	}{
		{"windows-1252", "=?windows-1252?q?caf=E9?=", "café"},
		{"koi8-r", "=?koi8-r?b?8NLJ18XU?=", "Привет"},
		{"utf-8", "=?utf-8?q?na=C3=AFve?=", "naïve"},
	}
	opts := clientOptions(nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := opts.WordDecoder.DecodeHeader(tt.encoded)
			if err != nil || got != tt.want {
				t.Errorf("DecodeHeader(%q) = %q, %v; want %q", tt.encoded, got, err, tt.want)
			}
		})
	}
}
