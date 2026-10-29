package imapstore

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"

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
				if caps := s.Capabilities(); !slices.Contains(caps, "IMAP4rev1") {
					t.Errorf("Capabilities() = %v, want IMAP4rev1", caps)
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
