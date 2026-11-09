// Package imapstore talks to IMAP servers. It is the only production package
// that imports go-imap, so the library can be patched or replaced in one place.
package imapstore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"mime"
	"net"
	"slices"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/charset"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/provider"
)

const defaultTimeout = 30 * time.Second

// errLogin marks a failure once LOGIN or AUTHENTICATE went out, other than
// a rejection of the credentials: the server hung up, said BYE, or the time
// ran out. Dialing again would send the credentials again, so the Store
// never does.
var errLogin = errors.New("logging in")

// defaultCloseTimeout bounds the LOGOUT exchange in Close.
const defaultCloseTimeout = 5 * time.Second

// DialOptions tune how Dial connects.
type DialOptions struct {
	// RootCAs replaces the system roots when set. Tests use it.
	RootCAs *x509.CertPool
	// Timeout bounds connecting, logging in and fetching capabilities. Zero
	// means 30 seconds.
	Timeout time.Duration
	// unilateral, when set, hears what the server reports unprompted, such
	// as EXISTS and EXPUNGE during IDLE. Watch connections set it.
	unilateral *imapclient.UnilateralDataHandler
}

// Session is a logged-in IMAP connection.
type Session struct {
	client *imapclient.Client
	// caps is fetched in Dial, under its deadline, and sorted.
	caps []string
	// conn is the TCP connection under client. Close shuts it if LOGOUT stalls.
	conn net.Conn
	// closeTimeout bounds LOGOUT in Close. Tests shorten it.
	closeTimeout time.Duration
}

// Dial connects to ep, secures the connection and logs in with creds.
// Errors matching auth.ErrRejected mean the server refused the credentials.
func Dial(ctx context.Context, ep provider.Endpoint, creds auth.Credentials, opts DialOptions) (*Session, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", ep.Addr())
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", ep.Addr(), err)
	}
	// If ctx ends mid-handshake, closing the connection unblocks the read or write in progress.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() }) // the caller sees ctx.Err() instead
	client, err := handshake(ctx, conn, ep, opts.RootCAs, opts.unilateral)
	loginFailed := false
	if err == nil {
		err = login(client, creds)
		loginFailed = err != nil
	}
	var caps []string
	if err == nil {
		caps, err = capabilities(client)
	}
	if !stop() {
		if loginFailed && !errors.Is(err, auth.ErrRejected) {
			return nil, fmt.Errorf("%w at %s: %w", errLogin, ep.Addr(), ctx.Err())
		}
		return nil, fmt.Errorf("connecting to %s: %w", ep.Addr(), ctx.Err())
	}
	if err != nil {
		_ = conn.Close() // the handshake, login or capability error is the one to report
		return nil, err
	}
	return &Session{client: client, caps: caps, conn: conn, closeTimeout: defaultCloseTimeout}, nil
}

// handshake secures conn and returns a client on it, with unilateral, when
// set, hearing what the server reports unprompted.
func handshake(ctx context.Context, conn net.Conn, ep provider.Endpoint, roots *x509.CertPool, unilateral *imapclient.UnilateralDataHandler) (*imapclient.Client, error) {
	tlsConfig := &tls.Config{ServerName: ep.TLSServerName(), RootCAs: roots, MinVersion: tls.VersionTLS12}
	switch ep.Security {
	case provider.SecurityTLS:
		tlsConn := tls.Client(conn, tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("TLS handshake with %s: %w", ep.Addr(), err)
		}
		options := clientOptions(nil)
		options.UnilateralDataHandler = unilateral
		return imapclient.New(tlsConn, options), nil
	case provider.SecurityStartTLS:
		options := clientOptions(tlsConfig)
		options.UnilateralDataHandler = unilateral
		c, err := imapclient.NewStartTLS(conn, options)
		if err != nil {
			return nil, fmt.Errorf("STARTTLS with %s: %w", ep.Addr(), err)
		}
		return c, nil
	default:
		return nil, fmt.Errorf("unsupported security %v for %s", ep.Security, ep.Addr())
	}
}

// clientOptions decodes RFC 2047 encoded words in every charset go-message
// knows, not only UTF-8 and ISO-8859-1, so subjects and display names arrive
// as UTF-8.
func clientOptions(tlsConfig *tls.Config) *imapclient.Options {
	return &imapclient.Options{TLSConfig: tlsConfig, WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader}}
}

func login(c *imapclient.Client, creds auth.Credentials) error {
	var err error
	switch creds.Method {
	case provider.AuthPassword:
		err = c.Login(creds.Username, creds.Secret).Wait()
	case provider.AuthOAuth2:
		err = c.Authenticate(creds.SASL())
	default:
		return fmt.Errorf("unsupported auth method %q", creds.Method)
	}
	return loginError(err)
}

// responseCodeWebAlert is Gmail's code for a login that needs the user to
// sign in on the web first. go-imap has no constant for it.
const responseCodeWebAlert imap.ResponseCode = "WEBALERT"

// loginError sorts a failed LOGIN or AUTHENTICATE into a credential
// rejection or an ordinary error. Either way the *imap.Error stays in the
// chain. Its text comes from the server, never from creds.
func loginError(err error) error {
	if err == nil {
		return nil
	}
	var imapErr *imap.Error
	if errors.As(err, &imapErr) && rejectsCredentials(imapErr) {
		return fmt.Errorf("%w: %w", auth.ErrRejected, imapErr)
	}
	return fmt.Errorf("%w: %w", errLogin, err)
}

// rejectsCredentials reports whether a reply to LOGIN or AUTHENTICATE refuses
// the credentials themselves, so a human has to supply new ones.
func rejectsCredentials(e *imap.Error) bool {
	if e.Type != imap.StatusResponseTypeNo {
		return false
	}
	switch e.Code {
	case "", imap.ResponseCodeAuthenticationFailed, imap.ResponseCodeAuthorizationFailed,
		imap.ResponseCodeExpired, responseCodeWebAlert:
		return true
	default:
		// ALERT is deliberately not a rejection: Gmail sends it for too many
		// simultaneous connections. UNAVAILABLE, INUSE, LIMIT and the rest
		// are server trouble, and an unknown code is not assumed to be about
		// the credentials.
		return false
	}
}

// capabilities asks for the capabilities again, since logging in invalidates
// the ones from the greeting, and returns them sorted.
func capabilities(c *imapclient.Client) ([]string, error) {
	set, err := c.Capability().Wait()
	if err != nil {
		return nil, fmt.Errorf("fetching capabilities: %w", err)
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, string(name))
	}
	slices.Sort(out)
	return out, nil
}

// Capabilities returns a copy of the capabilities fetched by Dial, sorted.
func (s *Session) Capabilities() []string {
	return slices.Clone(s.caps)
}

// Close logs out and closes the connection. A server that does not answer
// LOGOUT within the close timeout has its connection shut instead, and that
// counts as closed: the session is gone either way.
func (s *Session) Close() error {
	// A deadline set on the connection would not hold, because imapclient
	// resets the read deadline after every response, so a timer shuts the
	// connection instead.
	timer := time.AfterFunc(s.closeTimeout, func() { _ = s.conn.Close() }) // Logout and Close below report the outcome
	logoutErr := s.client.Logout().Wait()
	closeErr := s.client.Close()
	if !timer.Stop() {
		return fmt.Errorf("closing IMAP session: LOGOUT unanswered after %v; connection closed", s.closeTimeout)
	}
	if errors.Is(closeErr, net.ErrClosed) {
		closeErr = nil // the server hung up after BYE, as it should
	}
	if err := errors.Join(logoutErr, closeErr); err != nil {
		return fmt.Errorf("closing IMAP session: %w", err)
	}
	return nil
}
