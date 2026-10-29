// Package saslx has SASL server pieces that go-sasl lacks, for test servers.
package saslx

import (
	"errors"
	"strings"

	"github.com/emersion/go-sasl"
)

// XOAuth2Server returns a SASL server for XOAUTH2. It passes the username and
// bearer token from the client's response to check.
func XOAuth2Server(check func(username, token string) error) sasl.Server {
	return xoauth2Server(check)
}

type xoauth2Server func(username, token string) error

func (check xoauth2Server) Next(response []byte) ([]byte, bool, error) {
	if response == nil {
		return []byte{}, false, nil // ask for the initial response
	}
	user, token, ok := parse(string(response))
	if !ok {
		return nil, true, errors.New("malformed XOAUTH2 response")
	}
	return nil, true, check(user, token)
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
