// Package auth turns account config and vault secrets into login credentials,
// tracks which accounts need a human, and runs the OAuth2 login flow.
package auth

import (
	"errors"
	"fmt"

	"github.com/emersion/go-sasl"

	"github.com/jamesainslie/chainmail/internal/provider"
)

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

// Credentials are what a dialer needs to log in.
type Credentials struct {
	Method   provider.AuthMethod
	Username string
	// Secret is the password, or the OAuth2 access token.
	Secret string
}

// SASL returns a SASL client for c: PLAIN for passwords, XOAUTH2 for OAuth2.
func (c Credentials) SASL() sasl.Client {
	if c.Method == provider.AuthOAuth2 {
		return NewXOAuth2Client(c.Username, c.Secret)
	}
	return sasl.NewPlainClient("", c.Username, c.Secret)
}

// String keeps the secret out of logs and error messages.
func (c Credentials) String() string {
	return fmt.Sprintf("Credentials{Method: %s, Username: %s, Secret: [redacted]}", c.Method, c.Username)
}

// GoString keeps the secret out of %#v output.
func (c Credentials) GoString() string { return c.String() }
