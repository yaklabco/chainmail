// Package imaptest runs an in-memory IMAP server for tests.
package imaptest

import (
	"crypto/tls"
	"io"
	"log"
	"net"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-sasl"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/testutil/saslx"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// User is a mailbox on the test server. Token, when set, is accepted over XOAUTH2.
type User struct {
	Username string
	Password string
	Token    string
}

// Start runs a server until the test ends and returns its endpoint.
// security picks implicit TLS or STARTTLS. ca supplies the certificate.
func Start(t testing.TB, ca *tlstest.CA, security provider.Security, users ...User) provider.Endpoint {
	t.Helper()
	mem := imapmemserver.New()
	byName := map[string]User{}
	for _, u := range users {
		mu := imapmemserver.NewUser(u.Username, u.Password)
		if err := mu.Create("INBOX", nil); err != nil {
			t.Fatalf("creating INBOX: %v", err)
		}
		mem.AddUser(mu)
		byName[u.Username] = u
	}
	opts := &imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &session{Session: mem.NewSession(), users: byName}, nil, nil
		},
		Caps:   imap.CapSet{imap.CapIMAP4rev1: {}},
		Logger: log.New(io.Discard, "", 0),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	serveLn := ln
	if security == provider.SecurityTLS {
		serveLn = tls.NewListener(ln, ca.Server)
	} else {
		opts.TLSConfig = ca.Server
	}
	srv := imapserver.New(opts)
	go func() { _ = srv.Serve(serveLn) }() // Serve returns an error once Close runs; that is the normal shutdown
	t.Cleanup(func() { _ = srv.Close() })  // nothing to do about a close error at test end
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", ln.Addr())
	}
	return provider.Endpoint{Host: "127.0.0.1", Port: addr.Port, Security: security}
}

type session struct {
	imapserver.Session
	users map[string]User
}

func (s *session) AuthenticateMechanisms() []string {
	return []string{sasl.Plain, auth.XOAuth2}
}

func (s *session) Authenticate(mech string) (sasl.Server, error) {
	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(func(_, username, password string) error {
			return s.Login(username, password)
		}), nil
	case auth.XOAuth2:
		return saslx.XOAuth2Server(func(username, token string) error {
			u, ok := s.users[username]
			if !ok || u.Token == "" || u.Token != token {
				return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAuthenticationFailed, Text: "invalid token"}
			}
			return s.Login(u.Username, u.Password)
		}), nil
	default:
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "unsupported mechanism " + mech}
	}
}
