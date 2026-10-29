package auth

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/emersion/go-sasl"
)

// XOAuth2 is the SASL mechanism Google uses for OAuth2 bearer tokens.
const XOAuth2 = "XOAUTH2"

// xoauth2Separator separates the fields of an XOAUTH2 initial response.
const xoauth2Separator = "\x01"

// errXOAuth2Separator never includes the offending value, which may be a token.
var errXOAuth2Separator = errors.New("XOAUTH2 username or token contains a 0x01 byte")

type xoauth2Client struct {
	username string
	token    string
}

// NewXOAuth2Client returns a SASL client for Google's XOAUTH2 mechanism.
// The client redacts the token when printed or logged.
func NewXOAuth2Client(username, token string) sasl.Client {
	return &xoauth2Client{username: username, token: token}
}

// Start builds the initial response. A 0x01 byte in either value would let it
// inject extra fields, so Start refuses one.
func (c *xoauth2Client) Start() (string, []byte, error) {
	if strings.Contains(c.username, xoauth2Separator) || strings.Contains(c.token, xoauth2Separator) {
		return "", nil, errXOAuth2Separator
	}
	ir := "user=" + c.username + xoauth2Separator + "auth=Bearer " + c.token + xoauth2Separator + xoauth2Separator
	return XOAuth2, []byte(ir), nil
}

// Next answers a server challenge. Google only sends one to report an error,
// as JSON, and expects an empty reply before it fails the exchange.
func (c *xoauth2Client) Next(_ []byte) ([]byte, error) {
	return []byte{}, nil
}

func (c *xoauth2Client) String() string {
	return fmt.Sprintf("XOAUTH2 client for %s, token %s", c.username, redacted)
}

func (c *xoauth2Client) Format(f fmt.State, _ rune) { writeFormatted(f, c.String()) }

func (c *xoauth2Client) LogValue() slog.Value { return slog.StringValue(c.String()) }
