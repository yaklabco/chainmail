// Package saslx has SASL server pieces that go-sasl lacks, for test servers.
package saslx

import (
	"errors"
	"strings"

	"github.com/emersion/go-sasl"
)

// gmailErrorChallenge is the JSON error Gmail sends, as a SASL challenge,
// before it fails an XOAUTH2 exchange with a bad token.
const gmailErrorChallenge = `{"status":"401","schemes":"Bearer","scope":"https://mail.google.com/"}`

// XOAuth2Server returns a SASL server for XOAUTH2. It passes the username and
// bearer token from the client's response to check. When check fails, the
// server copies Gmail: it sends a JSON error challenge, waits for the
// client's reply, then fails the exchange with check's error.
func XOAuth2Server(check func(username, token string) error) sasl.Server {
	return &xoauth2Server{check: check}
}

type xoauth2Server struct {
	check func(username, token string) error
	// failure is check's error, held until the client answers the error challenge.
	failure error
}

func (s *xoauth2Server) Next(response []byte) ([]byte, bool, error) {
	if s.failure != nil {
		return nil, true, s.failure // whatever the client replied, the exchange fails
	}
	if response == nil {
		return nil, false, nil // an empty continuation asks for the initial response
	}
	user, token, ok := parse(string(response))
	if !ok {
		return nil, true, errors.New("malformed XOAUTH2 response")
	}
	// A failed check is not this step's error. It is held and reported once
	// the client has answered the error challenge.
	s.failure = s.check(user, token)
	return s.challenge(), s.failure == nil, nil
}

// challenge is what follows the initial response: nothing after a good
// token, Gmail's JSON error after a bad one.
func (s *xoauth2Server) challenge() []byte {
	if s.failure == nil {
		return nil
	}
	return []byte(gmailErrorChallenge)
}

func parse(s string) (user, token string, ok bool) {
	s, found := strings.CutSuffix(s, "\x01\x01")
	if !found {
		return "", "", false
	}
	userField, authField, found := strings.Cut(s, "\x01")
	if !found {
		return "", "", false
	}
	user, okUser := strings.CutPrefix(userField, "user=")
	token, okToken := strings.CutPrefix(authField, "auth=Bearer ")
	return user, token, okUser && okToken
}
