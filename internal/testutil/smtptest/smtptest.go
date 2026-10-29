// Package smtptest runs an SMTP submission server for tests. For now it only
// authenticates; sub-project 3 teaches it to accept mail.
package smtptest

import (
	"crypto/tls"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/testutil/saslx"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// User can authenticate with Password over PLAIN, or with Token over XOAUTH2 when Token is set.
type User struct {
	Username string
	Password string
	Token    string
	// AuthError, when set, answers every AUTH for this user, whatever the
	// credentials. Tests use it to make the server report trouble, such as
	// 454 4.7.0. An *smtp.SMTPError is sent with its own code.
	AuthError error
}

// Start runs a server until the test ends and returns its endpoint.
// security picks implicit TLS or STARTTLS. ca supplies the certificate.
func Start(t testing.TB, ca *tlstest.CA, security provider.Security, users ...User) provider.Endpoint {
	t.Helper()
	be := &backend{users: map[string]User{}}
	for _, u := range users {
		be.users[u.Username] = u
	}
	srv := smtp.NewServer(be)
	srv.Domain = "localhost"
	srv.TLSConfig = ca.Server
	srv.ReadTimeout = 10 * time.Second
	srv.WriteTimeout = 10 * time.Second
	srv.ErrorLog = log.New(io.Discard, "", 0)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	serveLn := ln
	if security == provider.SecurityTLS {
		serveLn = tls.NewListener(ln, ca.Server)
	}
	go func() { _ = srv.Serve(serveLn) }() // Serve returns an error once Close runs; that is the normal shutdown
	t.Cleanup(func() { _ = srv.Close() })  // nothing to do about a close error at test end
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", ln.Addr())
	}
	return provider.Endpoint{Host: "127.0.0.1", Port: addr.Port, Security: security}
}

var (
	errAuthFailed  = &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "authentication failed"}
	errUnknownMech = &smtp.SMTPError{Code: 504, EnhancedCode: smtp.EnhancedCode{5, 5, 4}, Message: "unsupported mechanism"}
	errNoMail      = &smtp.SMTPError{Code: 502, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "smtptest does not accept mail yet"}
)

type backend struct {
	users map[string]User
}

func (b *backend) NewSession(*smtp.Conn) (smtp.Session, error) {
	return &session{users: b.users}, nil
}

type session struct {
	users map[string]User
}

func (s *session) AuthMechanisms() []string {
	return []string{sasl.Plain, auth.XOAuth2}
}

func (s *session) Auth(mech string) (sasl.Server, error) {
	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(func(_, username, password string) error {
			return s.check(username, func(u User) bool { return u.Password == password })
		}), nil
	case auth.XOAuth2:
		return saslx.XOAuth2Server(func(username, token string) error {
			return s.check(username, func(u User) bool { return u.Token != "" && u.Token == token })
		}), nil
	default:
		return nil, errUnknownMech
	}
}

// check answers an AUTH for username: the user's AuthError when one is set,
// otherwise nil when ok accepts the user's secret and 535 when it does not.
func (s *session) check(username string, ok func(User) bool) error {
	u, found := s.users[username]
	switch {
	case !found:
		return errAuthFailed
	case u.AuthError != nil:
		return u.AuthError
	case ok(u):
		return nil
	default:
		return errAuthFailed
	}
}

func (s *session) Mail(string, *smtp.MailOptions) error { return errNoMail }
func (s *session) Rcpt(string, *smtp.RcptOptions) error { return errNoMail }

func (s *session) Data(r io.Reader) error {
	_, _ = io.Copy(io.Discard, r) // go-smtp requires the body to be consumed; its content doesn't matter here
	return errNoMail
}

func (s *session) Reset()        {}
func (s *session) Logout() error { return nil }
