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
		Transport:     redirectGuard{base: &authTransport{m: m, account: account, base: base}, allow: sameOrigin, refused: errRedirectHost},
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

// withRedirectPolicy returns a copy of c that follows only the redirects
// allow accepts, nil meaning none, and that checkRedirect then lets
// through. Its transport is c's, or http.DefaultTransport, under a
// redirectGuard; its timeout and cookie jar are c's.
func withRedirectPolicy(c *http.Client, allow func(from, to *url.URL) bool, refused error,
	checkRedirect func(*http.Request, []*http.Request) error,
) *http.Client {
	out := *c
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	out.Transport = redirectGuard{base: base, allow: allow, refused: refused}
	out.CheckRedirect = checkRedirect
	return &out
}

// redirectGuard refuses a redirect response before net/http reads its
// Location. net/http quotes the Location of a redirect that CheckRedirect
// refuses, or that does not parse, in its error, and the Location is the
// server's text. A refusal here carries only the request's own URL and
// refused.
type redirectGuard struct {
	base    http.RoundTripper
	allow   func(from, to *url.URL) bool
	refused error
}

// RoundTrip sends req and passes on its response, unless it is a redirect
// that allow does not accept.
func (g redirectGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := g.base.RoundTrip(req)
	if err != nil || !isRedirect(resp.StatusCode) {
		return resp, err
	}
	if g.allow != nil {
		if to, err := req.URL.Parse(resp.Header.Get("Location")); err == nil && g.allow(req.URL, to) {
			return resp, nil
		}
	}
	_ = resp.Body.Close() // the redirect is refused unread
	return nil, g.refused
}

// sameOrigin accepts a redirect to the scheme and host it came from.
func sameOrigin(from, to *url.URL) bool { return to.Scheme == from.Scheme && to.Host == from.Host }

// refuseRedirect follows no redirect. redirectGuard refuses every redirect
// of a token request first; this is the backstop.
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

// isRedirect reports whether code is a status net/http follows. Other 3xx
// codes, such as 304 Not Modified, are answers, not redirects.
func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}
