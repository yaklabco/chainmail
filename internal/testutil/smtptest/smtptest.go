// Package smtptest runs an SMTP submission server for tests. It
// authenticates, captures the mail it accepts and can inject faults at MAIL,
// RCPT, DATA and around the final dot.
package smtptest

import (
	"crypto/tls"
	"io"
	"log"
	"net"
	"slices"
	"sync"
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

// Envelope is one message the server accepted: the MAIL FROM address, the
// RCPT TO addresses in order, and the DATA bytes with dot-stuffing undone.
type Envelope struct {
	From string
	To   []string
	Data []byte
}

func (e Envelope) clone() Envelope {
	return Envelope{From: e.From, To: slices.Clone(e.To), Data: slices.Clone(e.Data)}
}

// Options configure a server started with StartWith. The zero value accepts
// all mail from an authenticated user.
type Options struct {
	// OnMessage, when set, runs for each captured message on the server's
	// goroutine before the client gets its reply. A test can block in it to
	// hold the DATA reply back.
	OnMessage func(Envelope)

	// FailMail answers every MAIL FROM.
	FailMail *smtp.SMTPError
	// FailRcpt answers RCPT TO for the addresses it holds. Other recipients
	// are accepted.
	FailRcpt map[string]*smtp.SMTPError
	// DropAtRcpt closes the connection in place of the reply to RCPT TO for
	// this address. Recipients before it get their replies. Nothing is
	// captured.
	DropAtRcpt string
	// FailData answers the end of DATA once the whole message has been read.
	// The message is not captured.
	FailData *smtp.SMTPError
	// DropBeforeDataEnd closes the connection in place of the 354 reply to
	// DATA, so the client never gets to write the final dot. Nothing is
	// captured.
	DropBeforeDataEnd bool
	// DropAfterDataEnd reads the whole message, through the final dot, then
	// closes the connection without replying. The message is captured, as a
	// server that queued it and then lost the connection would have it.
	DropAfterDataEnd bool

	// EnableDSN offers the DSN extension (RFC 3461). The server then reads
	// ESMTP parameters such as ORCPT after a RCPT TO path, as a real
	// submission server does.
	EnableDSN bool
}

// Server is a running test server.
type Server struct {
	ep    provider.Endpoint
	opts  Options
	users map[string]User

	mu        sync.Mutex
	messages  []Envelope
	mailFroms []string
}

// Endpoint is where the server listens.
func (s *Server) Endpoint() provider.Endpoint { return s.ep }

// Messages returns copies of the messages captured so far, oldest first.
func (s *Server) Messages() []Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Envelope, len(s.messages))
	for i, m := range s.messages {
		out[i] = m.clone()
	}
	return out
}

// MailFroms returns the address of every MAIL FROM the server accepted so
// far, oldest first, whether or not its transaction reached DATA.
func (s *Server) MailFroms() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.mailFroms)
}

func (s *Server) capture(e Envelope) {
	s.mu.Lock()
	s.messages = append(s.messages, e.clone())
	s.mu.Unlock()
	if s.opts.OnMessage != nil {
		s.opts.OnMessage(e.clone())
	}
}

// Start runs a server that accepts all mail until the test ends and returns
// its endpoint. security picks implicit TLS or STARTTLS. ca supplies the
// certificate.
func Start(t testing.TB, ca *tlstest.CA, security provider.Security, users ...User) provider.Endpoint {
	t.Helper()
	return StartWith(t, ca, security, Options{}, users...).Endpoint()
}

// StartWith runs a server with opts until the test ends.
func StartWith(t testing.TB, ca *tlstest.CA, security provider.Security, opts Options, users ...User) *Server {
	t.Helper()
	s := &Server{opts: opts, users: map[string]User{}}
	for _, u := range users {
		s.users[u.Username] = u
	}
	srv := smtp.NewServer(s)
	srv.Domain = "localhost"
	srv.EnableDSN = opts.EnableDSN
	srv.TLSConfig = ca.Server
	srv.ReadTimeout = 10 * time.Second
	srv.WriteTimeout = 10 * time.Second
	srv.ErrorLog = log.New(io.Discard, "", 0)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	var serveLn net.Listener = faultListener{ln}
	if security == provider.SecurityTLS {
		serveLn = tls.NewListener(serveLn, ca.Server)
	}
	go func() { _ = srv.Serve(serveLn) }() // Serve returns an error once Close runs; that is the normal shutdown
	t.Cleanup(func() { _ = srv.Close() })  // nothing to do about a close error at test end
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", ln.Addr())
	}
	s.ep = provider.Endpoint{Host: "127.0.0.1", Port: addr.Port, Security: security}
	return s
}

var (
	errAuthFailed   = &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8}, Message: "authentication failed"}
	errUnknownMech  = &smtp.SMTPError{Code: 504, EnhancedCode: smtp.EnhancedCode{5, 5, 4}, Message: "unsupported mechanism"}
	errAuthRequired = &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "authentication required"}
	errDropped      = &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 4, 2}, Message: "smtptest dropped the connection"}
)

// NewSession implements smtp.Backend.
func (s *Server) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &session{srv: s, conn: rawConn(c.Conn())}, nil
}

// rawConn finds the faultConn under any TLS layer, or nil if there is none.
func rawConn(c net.Conn) *faultConn {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	fc, _ := c.(*faultConn) // nil if it is not one; the drop faults then do nothing
	return fc
}

type session struct {
	srv    *Server
	conn   *faultConn
	authed bool
	from   string
	to     []string
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
	u, found := s.srv.users[username]
	switch {
	case !found:
		return errAuthFailed
	case u.AuthError != nil:
		return u.AuthError
	case ok(u):
		s.authed = true
		return nil
	default:
		return errAuthFailed
	}
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	if !s.authed {
		return errAuthRequired
	}
	if e := s.srv.opts.FailMail; e != nil {
		return e
	}
	s.from = from
	s.srv.mu.Lock()
	s.srv.mailFroms = append(s.srv.mailFroms, from)
	s.srv.mu.Unlock()
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if d := s.srv.opts.DropAtRcpt; d != "" && to == d && s.conn != nil {
		s.conn.drop()
		return errDropped // never reaches the client; the connection is gone
	}
	if s.srv.opts.DropBeforeDataEnd && s.conn != nil {
		// Let this RCPT's reply through, then drop whatever comes next,
		// which is the 354 once the client sends DATA.
		s.conn.dropAfter(1)
	}
	if e := s.srv.opts.FailRcpt[to]; e != nil {
		return e
	}
	s.to = append(s.to, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if e := s.srv.opts.FailData; e != nil {
		return e
	}
	s.srv.capture(Envelope{From: s.from, To: s.to, Data: data})
	if s.srv.opts.DropAfterDataEnd && s.conn != nil {
		s.conn.drop()
		return errDropped // never reaches the client; the connection is gone
	}
	return nil
}

func (s *session) Reset() {
	s.from = ""
	s.to = nil
}

func (s *session) Logout() error { return nil }

// faultListener wraps every accepted connection in a faultConn.
type faultListener struct{ net.Listener }

func (l faultListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &faultConn{Conn: c, passes: -1}, nil
}

// faultConn is the raw TCP connection, under any TLS. It can be told to
// close itself in place of a later write. go-smtp writes each reply line as
// one TLS record, so counting writes counts reply lines.
type faultConn struct {
	net.Conn

	mu     sync.Mutex
	passes int // writes to let through before dropping; negative means never drop
}

// dropAfter lets n more writes through and then closes the connection in
// place of the one after.
func (c *faultConn) dropAfter(n int) {
	c.mu.Lock()
	c.passes = n
	c.mu.Unlock()
}

// drop closes the connection now, without TLS close_notify, as a lost
// connection would.
func (c *faultConn) drop() {
	_ = c.Close() // the point is to lose the connection; a close error changes nothing
}

func (c *faultConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	drop := c.passes == 0
	if c.passes > 0 {
		c.passes--
	}
	c.mu.Unlock()
	if drop {
		c.drop()
		return 0, net.ErrClosed
	}
	return c.Conn.Write(b)
}
