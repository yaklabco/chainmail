// Package imaptest runs an in-memory IMAP server for tests.
package imaptest

import (
	"crypto/tls"
	"io"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	// LoginError, when set, answers every login for this user, whatever the
	// credentials. Tests use it to make the server report trouble, such as
	// NO [UNAVAILABLE].
	LoginError error
	// Logins, when set, counts this user's login attempts, whatever their outcome.
	Logins *atomic.Int32
	// Folders are created at start, besides INBOX.
	Folders []string
	// Mail holds raw messages to append at start, by folder.
	Mail map[string][]string
	// OnSelect, when set, hears every SELECT and EXAMINE this user sends.
	OnSelect func(mailbox string, readOnly bool)
}

// Start runs a server until the test ends and returns its endpoint.
// security picks implicit TLS or STARTTLS. ca supplies the certificate.
func Start(t testing.TB, ca *tlstest.CA, security provider.Security, users ...User) provider.Endpoint {
	t.Helper()
	mem := imapmemserver.New()
	byName := map[string]User{}
	for _, u := range users {
		mu := imapmemserver.NewUser(u.Username, u.Password)
		for _, name := range append([]string{"INBOX"}, u.Folders...) {
			if err := mu.Create(name, nil); err != nil {
				t.Fatalf("creating %s: %v", name, err)
			}
		}
		for folder, msgs := range u.Mail {
			for _, raw := range msgs {
				if _, err := mu.Append(folder, strings.NewReader(raw), &imap.AppendOptions{Time: time.Now()}); err != nil {
					t.Fatalf("appending to %s: %v", folder, err)
				}
			}
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
	// current is the logged-in user, for OnSelect.
	current *User
}

// Login counts the attempt and answers with the user's LoginError when one is set.
func (s *session) Login(username, password string) error {
	u, ok := s.users[username]
	if ok && u.Logins != nil {
		u.Logins.Add(1)
	}
	if ok && u.LoginError != nil {
		return u.LoginError
	}
	if err := s.Session.Login(username, password); err != nil {
		return err
	}
	if ok {
		s.current = &u
	}
	return nil
}

// Select reports SELECT and EXAMINE to the user's OnSelect.
func (s *session) Select(mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	if s.current != nil && s.current.OnSelect != nil {
		s.current.OnSelect(mailbox, options != nil && options.ReadOnly)
	}
	return s.Session.Select(mailbox, options)
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
