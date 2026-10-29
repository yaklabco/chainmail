// Package imapstore talks to IMAP servers. It is the only production package
// that imports go-imap, so the library can be patched or replaced in one place.
package imapstore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/provider"
)

const defaultTimeout = 30 * time.Second

// DialOptions tune how Dial connects.
type DialOptions struct {
	// RootCAs replaces the system roots when set. Tests use it.
	RootCAs *x509.CertPool
	// Timeout bounds connecting and logging in. Zero means 30 seconds.
	Timeout time.Duration
}

// Session is a logged-in IMAP connection.
type Session struct {
	client *imapclient.Client
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
	client, err := handshake(ctx, conn, ep, opts.RootCAs)
	if err == nil {
		err = login(client, creds)
	}
	if !stop() {
		return nil, fmt.Errorf("connecting to %s: %w", ep.Addr(), ctx.Err())
	}
	if err != nil {
		_ = conn.Close() // the login error is the one to report
		return nil, err
	}
	return &Session{client: client}, nil
}

func handshake(ctx context.Context, conn net.Conn, ep provider.Endpoint, roots *x509.CertPool) (*imapclient.Client, error) {
	tlsConfig := &tls.Config{ServerName: ep.Host, RootCAs: roots, MinVersion: tls.VersionTLS12}
	switch ep.Security {
	case provider.SecurityTLS:
		tlsConn := tls.Client(conn, tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("TLS handshake with %s: %w", ep.Addr(), err)
		}
		return imapclient.New(tlsConn, nil), nil
	case provider.SecurityStartTLS:
		c, err := imapclient.NewStartTLS(conn, &imapclient.Options{TLSConfig: tlsConfig})
		if err != nil {
			return nil, fmt.Errorf("STARTTLS with %s: %w", ep.Addr(), err)
		}
		return c, nil
	default:
		return nil, fmt.Errorf("unsupported security %v for %s", ep.Security, ep.Addr())
	}
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
	var imapErr *imap.Error
	if errors.As(err, &imapErr) && imapErr.Type == imap.StatusResponseTypeNo {
		return fmt.Errorf("%w: %s", auth.ErrRejected, imapErr.Text)
	}
	if err != nil {
		return fmt.Errorf("logging in: %w", err)
	}
	return nil
}

// Capabilities returns the server's capabilities in sorted order.
func (s *Session) Capabilities() []string {
	caps := s.client.Caps()
	out := make([]string, 0, len(caps))
	for c := range caps {
		out = append(out, string(c))
	}
	slices.Sort(out)
	return out
}

// Close logs out and closes the connection.
func (s *Session) Close() error {
	logoutErr := s.client.Logout().Wait()
	closeErr := s.client.Close()
	if errors.Is(closeErr, net.ErrClosed) {
		closeErr = nil // the server hung up after BYE, as it should
	}
	if err := errors.Join(logoutErr, closeErr); err != nil {
		return fmt.Errorf("closing IMAP session: %w", err)
	}
	return nil
}
