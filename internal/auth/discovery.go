package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/yaklabco/chainmail/internal/config"
)

const (
	// discoveryTimeout bounds one OIDC discovery request.
	discoveryTimeout = 30 * time.Second
	// maxDiscoveryBytes caps the discovery document read. Real ones are a
	// few KiB.
	maxDiscoveryBytes = 1 << 20
)

// issuerEndpoint returns the endpoints of acct's issuer client, from its
// OIDC discovery document. The first call for a client fetches it; later
// calls use the cached endpoints. A failure is not cached, so the next call
// tries again.
func (m *Manager) issuerEndpoint(ctx context.Context, acct config.ResolvedAccount) (oauth2.Endpoint, error) {
	m.discoverMu.Lock()
	defer m.discoverMu.Unlock()
	if ep, ok := m.discovered[acct.OAuthClient]; ok {
		return ep, nil
	}
	ep, err := m.discover(ctx, acct.OAuthIssuer)
	if err != nil {
		return oauth2.Endpoint{}, fmt.Errorf("%w: OIDC discovery for oauth client %s: %w", ErrTokenEndpoint, acct.OAuthClient, err)
	}
	m.discovered[acct.OAuthClient] = ep
	return ep, nil
}

// discoveryDoc is the part of an OIDC discovery document chainmail reads.
type discoveryDoc struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

// discover fetches <issuer>/.well-known/openid-configuration. Its errors
// name the status or the field at fault, never the response body.
func (m *Manager) discover(ctx context.Context, issuer string) (oauth2.Endpoint, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	base := strings.TrimSuffix(issuer, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/.well-known/openid-configuration", nil)
	if err != nil {
		return oauth2.Endpoint{}, fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return oauth2.Endpoint{}, err
	}
	defer func() { _ = resp.Body.Close() }() // the document is read or refused already
	if resp.StatusCode != http.StatusOK {
		return oauth2.Endpoint{}, fmt.Errorf("the issuer returned %s", resp.Status)
	}
	var doc discoveryDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDiscoveryBytes)).Decode(&doc); err != nil {
		// json errors can quote the input, so only the fact is reported.
		return oauth2.Endpoint{}, fmt.Errorf("the discovery document is not JSON of at most %d bytes", maxDiscoveryBytes)
	}
	switch {
	case strings.TrimSuffix(doc.Issuer, "/") != base:
		return oauth2.Endpoint{}, fmt.Errorf("the discovery document names another issuer")
	case !endpointOK(doc.AuthorizationEndpoint):
		return oauth2.Endpoint{}, fmt.Errorf("the discovery document has no usable authorization_endpoint")
	case !endpointOK(doc.TokenEndpoint):
		return oauth2.Endpoint{}, fmt.Errorf("the discovery document has no usable token_endpoint")
	}
	return oauth2.Endpoint{
		AuthURL:   doc.AuthorizationEndpoint,
		TokenURL:  doc.TokenEndpoint,
		AuthStyle: oauth2.AuthStyleInParams,
	}, nil
}

// endpointOK reports whether s is an absolute URL with a host and no user
// info that credentials may go to, as config requires of the issuer itself.
func endpointOK(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return false
	}
	return secureURL(u)
}
