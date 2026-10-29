package saslx

import (
	"encoding/json"
	"errors"
	"testing"
)

const goodResponse = "user=me@example.com\x01auth=Bearer tok\x01\x01"

func checkToken(username, token string) error {
	if username != "me@example.com" || token != "tok" {
		return errBadToken
	}
	return nil
}

var errBadToken = errors.New("bad token")

func TestXOAuth2ServerAsksForInitialResponse(t *testing.T) {
	t.Parallel()
	s := XOAuth2Server(checkToken)
	challenge, done, err := s.Next(nil)
	if err != nil || done {
		t.Fatalf("Next(nil) = done %v, err %v, want a continuation", done, err)
	}
	// A nil challenge is an empty continuation, which asks for the initial response.
	if challenge != nil {
		t.Errorf("Next(nil) challenge = %q, want nil", challenge)
	}
}

func TestXOAuth2ServerAcceptsGoodToken(t *testing.T) {
	t.Parallel()
	s := XOAuth2Server(checkToken)
	challenge, done, err := s.Next([]byte(goodResponse))
	if challenge != nil || !done || err != nil {
		t.Errorf("Next(good) = %q, %v, %v, want nil, true, nil", challenge, done, err)
	}
}

// Gmail answers a bad token with a JSON error challenge, waits for the
// client's empty reply, then fails the exchange.
func TestXOAuth2ServerFailsLikeGmail(t *testing.T) {
	t.Parallel()
	s := XOAuth2Server(checkToken)
	challenge, done, err := s.Next([]byte("user=me@example.com\x01auth=Bearer nope\x01\x01"))
	if done || err != nil {
		t.Fatalf("Next(bad) = done %v, err %v, want an error challenge", done, err)
	}
	var body struct {
		Status  string `json:"status"`
		Schemes string `json:"schemes"`
		Scope   string `json:"scope"`
	}
	if err := json.Unmarshal(challenge, &body); err != nil {
		t.Fatalf("challenge %q is not JSON: %v", challenge, err)
	}
	if body.Status != "401" || body.Schemes != "Bearer" || body.Scope != "https://mail.google.com/" {
		t.Errorf("challenge = %+v", body)
	}

	challenge, done, err = s.Next([]byte{})
	if challenge != nil || !done || !errors.Is(err, errBadToken) {
		t.Errorf("Next(empty reply) = %q, %v, %v, want nil, true, %v", challenge, done, err, errBadToken)
	}
}

func TestXOAuth2ServerRejectsMalformedResponse(t *testing.T) {
	t.Parallel()
	s := XOAuth2Server(checkToken)
	if _, done, err := s.Next([]byte("garbage")); !done || err == nil {
		t.Errorf("Next(garbage) = done %v, err %v, want done with an error", done, err)
	}
}
