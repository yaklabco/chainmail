package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/provider"
)

// httpTimeout bounds each request of a client HTTPClient returns.
const httpTimeout = 30 * time.Second

// maxRedirects is how many same-host redirects a request follows.
const maxRedirects = 10

// ErrPlainHTTP means a request would have sent credentials over plain http
// to a host other than 127.0.0.1 or localhost.
var ErrPlainHTTP = errors.New("credentials go only over https, or plain http to 127.0.0.1 or localhost")

// errRedirectHost refuses a redirect that would take the credentials to
// another host, or a discovery to another host or scheme.
var errRedirectHost = errors.New("redirect to another host or scheme refused")

// errTokenRedirect refuses a redirect of a token request. A 307 or 308
// would send the form again, with the refresh token, the authorization code
// and the PKCE verifier, and token endpoints never redirect a POST.
var errTokenRedirect = errors.New("the token endpoint redirected, which chainmail never follows")

// HTTPClient returns a client whose transport authenticates every request
// for account: Bearer with the OAuth2 access token, which Credentials
// refreshes and parks as for mail, or Basic with the username and password
// for password accounts. A parked account fails each request with a
// *RequiredError before anything is sent. The client refuses plain http to
// any host but 127.0.0.1 and localhost, and redirects to another host. It
// never calls Rejected: a refusal by a calendar server says nothing about
// mail, so the caller decides what it means. Each request is bounded by 30
// seconds. The transport under it is that of the client WithHTTPClient set,
// or http.DefaultTransport.
func (m *Manager) HTTPClient(account string) (*http.Client, error) {
	if _, err := m.cfg.Account(account); err != nil {
		return nil, err
	}
	base := m.httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	return &http.Client{
		Timeout:       httpTimeout,
		Transport:     &authTransport{m: m, account: account, base: base},
		CheckRedirect: sameHostRedirect,
	}, nil
}

// authTransport adds an account's credentials to each request. It holds no
// credentials itself: it asks the Manager for them per request.
type authTransport struct {
	m       *Manager
	account string
	base    http.RoundTripper
}

// RoundTrip authenticates a copy of req and sends it. The caller's request
// is never changed.
func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !secureURL(req.URL) {
		closeBody(req)
		return nil, ErrPlainHTTP
	}
	creds, err := t.m.Credentials(req.Context(), t.account)
	if err != nil {
		closeBody(req)
		return nil, err
	}
	out := req.Clone(req.Context())
	switch creds.Method {
	case provider.AuthOAuth2:
		out.Header.Set("Authorization", "Bearer "+creds.Secret)
	case provider.AuthPassword:
		out.SetBasicAuth(creds.Username, creds.Secret)
	default:
		closeBody(req)
		return nil, fmt.Errorf("unsupported auth method %q", creds.Method)
	}
	return t.base.RoundTrip(out)
}

// closeBody closes a request body that will not be sent, as a RoundTripper
// must.
func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close() // nothing was read from it
	}
}

// secureURL reports whether credentials may go to u: https, or plain http
// to a loopback host.
func secureURL(u *url.URL) bool {
	return u.Scheme == "https" || (u.Scheme == "http" && config.IsLoopbackHost(u.Hostname()))
}

// withRedirectPolicy returns a copy of c that follows redirects only as
// policy allows. The copy shares c's transport, timeout and cookie jar.
func withRedirectPolicy(c *http.Client, policy func(*http.Request, []*http.Request) error) *http.Client {
	out := *c
	out.CheckRedirect = policy
	return &out
}

// refuseRedirect follows no redirect.
func refuseRedirect(*http.Request, []*http.Request) error { return errTokenRedirect }

// sameHostRedirect follows a redirect only to the scheme and host of the
// first request.
func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
		return errRedirectHost
	}
	return nil
}
