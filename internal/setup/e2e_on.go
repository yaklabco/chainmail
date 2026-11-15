//go:build e2e

package setup

import (
	"fmt"
	"net"
	"strconv"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/provider"
)

// The environment variables the end-to-end test sets, each host:port.
const (
	e2eIMAPEnv = "CHAINMAIL_E2E_IMAP"
	e2eSMTPEnv = "CHAINMAIL_E2E_SMTP"
)

// E2EEnabled reports whether this binary was built with the e2e tag, the
// only build that accepts endpoint overrides. It is true here.
func E2EEnabled() bool { return true }

// E2EOverrides reads CHAINMAIL_E2E_IMAP and CHAINMAIL_E2E_SMTP through
// getenv and returns them as overrides with security "tls". An unset
// variable gives nil.
func E2EOverrides(getenv func(string) string) (imap, smtp *config.EndpointOverride, err error) {
	if imap, err = e2eEndpoint(getenv, e2eIMAPEnv); err != nil {
		return nil, nil, err
	}
	if smtp, err = e2eEndpoint(getenv, e2eSMTPEnv); err != nil {
		return nil, nil, err
	}
	return imap, smtp, nil
}

// e2eEndpoint parses the host:port in the variable key.
func e2eEndpoint(getenv func(string) string, key string) (*config.EndpointOverride, error) {
	v := getenv(key)
	if v == "" {
		return nil, nil
	}
	host, portText, err := net.SplitHostPort(v)
	if err != nil {
		return nil, fmt.Errorf("%s=%q: want host:port: %w", key, v, err)
	}
	port, err := strconv.Atoi(portText)
	if host == "" || err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("%s=%q: want host:port with a port from 1 to 65535", key, v)
	}
	return &config.EndpointOverride{Host: host, Port: port, Security: provider.SecurityTLS.String()}, nil
}
