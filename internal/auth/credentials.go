// Package auth turns account config and vault secrets into login credentials,
// tracks which accounts need a human, and runs the OAuth2 login flow.
package auth

import (
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/emersion/go-sasl"

	"github.com/jamesainslie/chainmail/internal/provider"
)

// redacted replaces a secret value wherever one would be printed.
const redacted = "[redacted]"

var (
	// ErrAuthRequired means a human must provide or renew credentials.
	// Errors that match it are *RequiredError values carrying a hint.
	ErrAuthRequired = errors.New("authentication required")
	// ErrRejected means a server refused the credentials.
	ErrRejected = errors.New("credentials rejected by server")
)

// RequiredError says which account needs attention and what to run.
type RequiredError struct {
	Account string
	Hint    string
}

func (e *RequiredError) Error() string {
	return fmt.Sprintf("account %s needs authentication: %s", e.Account, e.Hint)
}

// Unwrap lets errors.Is(err, ErrAuthRequired) match.
func (e *RequiredError) Unwrap() error { return ErrAuthRequired }

// Credentials are what a dialer needs to log in. Every way of printing,
// logging or marshalling them leaves Secret out.
type Credentials struct {
	Method   provider.AuthMethod
	Username string
	// Secret is the password, or the OAuth2 access token.
	Secret string `json:"-"`
}

// SASL returns a SASL client for c: PLAIN for passwords, XOAUTH2 for OAuth2.
// The client redacts the secret when printed or logged, like c does.
func (c Credentials) SASL() sasl.Client {
	if c.Method == provider.AuthOAuth2 {
		return NewXOAuth2Client(c.Username, c.Secret)
	}
	return &plainClient{
		inner:    sasl.NewPlainClient("", c.Username, c.Secret),
		username: c.Username,
	}
}

// String keeps the secret out of logs and error messages.
func (c Credentials) String() string {
	return fmt.Sprintf("Credentials{Method: %s, Username: %s, Secret: %s}", c.Method, c.Username, redacted)
}

// Format prints the redacted String for every verb, including %#v, %d and %x,
// which would otherwise reflect over the fields.
func (c Credentials) Format(f fmt.State, _ rune) { writeFormatted(f, c.String()) }

// LogValue keeps the secret out of slog output.
func (c Credentials) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("method", string(c.Method)),
		slog.String("username", c.Username),
		slog.String("secret", redacted),
	)
}

// plainClient wraps go-sasl's PLAIN client, whose exported Password field
// would otherwise show up in fmt, encoding/json and slog output. The
// wrapped client sits in an unexported field so encoding/json skips it.
type plainClient struct {
	inner    sasl.Client
	username string
}

func (c *plainClient) Start() (string, []byte, error) { return c.inner.Start() }

func (c *plainClient) Next(challenge []byte) ([]byte, error) { return c.inner.Next(challenge) }

func (c *plainClient) String() string {
	return fmt.Sprintf("PLAIN client for %s, password %s", c.username, redacted)
}

func (c *plainClient) Format(f fmt.State, _ rune) { writeFormatted(f, c.String()) }

func (c *plainClient) LogValue() slog.Value { return slog.StringValue(c.String()) }

// writeFormatted is the body of every redacting Format method. fmt.State
// writes cannot usefully fail, so the error is dropped.
func writeFormatted(f fmt.State, s string) { _, _ = io.WriteString(f, s) }
