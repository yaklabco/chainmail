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

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/testutil/saslx"
	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
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
	// ByeOnLogin hangs up with an untagged BYE in reply to every login
	// attempt, after counting it, as a server going away mid-login would.
	ByeOnLogin bool
	// Folders are created at start, besides INBOX.
	Folders []string
	// Mail holds raw messages to append at start, by folder.
	Mail map[string][]string
	// OnSelect, when set, hears every SELECT and EXAMINE this user sends.
	OnSelect func(mailbox string, readOnly bool)
	// MaxConns, when above zero, caps this user's logged-in sessions: a
	// login past it gets NO [LIMIT], as a server over its connection limit
	// answers. A session stops counting once its connection has closed.
	MaxConns int
	// Conns, when set, counts this user's logged-in sessions. Start makes
	// one for a MaxConns user that has none.
	Conns *atomic.Int32
}

// Start runs a server until the test ends and returns its endpoint.
// security picks implicit TLS or STARTTLS. ca supplies the certificate.
func Start(t testing.TB, ca *tlstest.CA, security provider.Security, users ...User) provider.Endpoint {
	t.Helper()
	mem := imapmemserver.New()
	byName := map[string]User{}
	for _, u := range users {
		if u.MaxConns > 0 && u.Conns == nil {
			u.Conns = new(atomic.Int32)
		}
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
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &session{Session: mem.NewSession(), users: byName, conn: conn}, nil, nil
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
	// conn is the connection the session serves, for ByeOnLogin.
	conn *imapserver.Conn
	// current is the logged-in user, for OnSelect.
	current *User
	// counted says the session counts toward its user's Conns.
	counted bool
}

// Login counts the attempt and answers with the user's LoginError when one
// is set, or NO [LIMIT] when the user is at MaxConns.
func (s *session) Login(username, password string) error {
	u, ok := s.users[username]
	if ok && u.Logins != nil {
		u.Logins.Add(1)
	}
	if ok && u.ByeOnLogin {
		_ = s.conn.Bye("going away") // the client sees the hang-up either way
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "going away"}
	}
	if ok && u.LoginError != nil {
		return u.LoginError
	}
	counting := ok && u.Conns != nil
	if counting {
		if n := int(u.Conns.Add(1)); u.MaxConns > 0 && n > u.MaxConns {
			u.Conns.Add(-1)
			return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "Too many simultaneous connections"}
		}
	}
	if err := s.Session.Login(username, password); err != nil {
		if counting {
			u.Conns.Add(-1)
		}
		return err
	}
	s.counted = counting
	if ok {
		s.current = &u
	}
	return nil
}

// Close ends the session's count toward its user's Conns.
func (s *session) Close() error {
	if s.counted {
		s.current.Conns.Add(-1)
		s.counted = false
	}
	return s.Session.Close()
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
