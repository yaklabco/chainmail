// Package sender submits mail over SMTP.
package sender

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/provider"
)

const defaultTimeout = 30 * time.Second

// DialOptions tune how the sender connects.
type DialOptions struct {
	// RootCAs replaces the system roots when set. Tests use it.
	RootCAs *x509.CertPool
	// Timeout bounds Dial, and separately each Send on the connection it
	// returns. Probe uses it for the whole probe. Zero means 30 seconds.
	Timeout time.Duration
}

func (o DialOptions) timeout() time.Duration {
	if o.Timeout == 0 {
		return defaultTimeout
	}
	return o.Timeout
}

// Probe connects to ep, authenticates with creds and quits without sending
// anything. Errors matching auth.ErrRejected mean the server refused the credentials.
func Probe(ctx context.Context, ep provider.Endpoint, creds auth.Credentials, opts DialOptions) error {
	ctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()
	c, err := Dial(ctx, ep, creds, opts)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()                             // QUIT normally closes it first
	stop := context.AfterFunc(ctx, func() { _ = c.raw.Close() }) // the caller sees ctx.Err() instead
	err = c.client.Quit()
	if !stop() {
		return fmt.Errorf("probing %s: %w", ep.Addr(), ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("QUIT: %w", err)
	}
	return nil
}

func handshake(ctx context.Context, conn net.Conn, ep provider.Endpoint, roots *x509.CertPool) (*smtp.Client, error) {
	tlsConfig := &tls.Config{ServerName: ep.Host, RootCAs: roots, MinVersion: tls.VersionTLS12}
	switch ep.Security {
	case provider.SecurityTLS:
		tlsConn := tls.Client(conn, tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("TLS handshake with %s: %w", ep.Addr(), err)
		}
		return smtp.NewClient(tlsConn), nil
	case provider.SecurityStartTLS:
		c, err := smtp.NewClientStartTLS(conn, tlsConfig)
		if err != nil {
			return nil, fmt.Errorf("STARTTLS with %s: %w", ep.Addr(), err)
		}
		// go-smtp only wraps the connection; the TLS handshake runs on the
		// next write. Greeting again now makes a certificate failure a
		// STARTTLS error rather than an AUTH one.
		if err := c.Hello("localhost"); err != nil {
			return nil, fmt.Errorf("STARTTLS with %s: %w", ep.Addr(), err)
		}
		return c, nil
	default:
		return nil, fmt.Errorf("unsupported security %v for %s", ep.Security, ep.Addr())
	}
}

func authenticate(c *smtp.Client, creds auth.Credentials) error {
	switch creds.Method {
	case provider.AuthPassword, provider.AuthOAuth2:
		return authError(c.Auth(creds.SASL()))
	default:
		return fmt.Errorf("unsupported auth method %q", creds.Method)
	}
}

// authError sorts a failed SMTP AUTH into a credential rejection or an
// ordinary error. Either way the *smtp.SMTPError stays in the chain. Its text
// comes from the server, never from the credentials.
func authError(err error) error {
	if err == nil {
		return nil
	}
	var smtpErr *smtp.SMTPError
	if errors.As(err, &smtpErr) && rejectsCredentials(smtpErr) {
		return fmt.Errorf("%w: %w", auth.ErrRejected, smtpErr)
	}
	return fmt.Errorf("SMTP AUTH: %w", err)
}

// rejectsCredentials reports whether an AUTH reply refuses the credentials
// themselves, so a human has to supply new ones. 535 is a bad login. 534 is
// Google's "use a different method" (app password required, or web login
// required). Everything else, in particular 454 and 421, is server trouble
// or a protocol problem that new credentials would not fix.
func rejectsCredentials(e *smtp.SMTPError) bool {
	return e.Code == 535 || e.Code == 534
}
