package auth

import "github.com/emersion/go-sasl"

// XOAuth2 is the SASL mechanism Google uses for OAuth2 bearer tokens.
const XOAuth2 = "XOAUTH2"

type xoauth2Client struct {
	username string
	token    string
}

// NewXOAuth2Client returns a SASL client for Google's XOAUTH2 mechanism.
func NewXOAuth2Client(username, token string) sasl.Client {
	return &xoauth2Client{username: username, token: token}
}

func (c *xoauth2Client) Start() (string, []byte, error) {
	ir := "user=" + c.username + "\x01auth=Bearer " + c.token + "\x01\x01"
	return XOAuth2, []byte(ir), nil
}

// Next answers a server challenge. Google only sends one to report an error,
// as JSON, and expects an empty reply before it fails the exchange.
func (c *xoauth2Client) Next(_ []byte) ([]byte, error) {
	return []byte{}, nil
}
