// Package oauthtest runs a fake OAuth2 token endpoint, with an OIDC
// discovery document, for tests.
package oauthtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"golang.org/x/oauth2"
)

// Values the fake server issues and accepts.
const (
	Code         = "test-auth-code"
	RefreshToken = "test-refresh-token"
	AccessToken  = "test-access-token"
	ClientSecret = "test-client-secret"
)

// IssuerPath is the path of the issuer a Server serves discovery for.
const IssuerPath = "/realms/test"

// Server is a fake token endpoint. It accepts Code with any PKCE verifier and
// RefreshToken until Revoke is called. It serves an OIDC discovery document
// for Issuer.
type Server struct {
	URL string

	public bool

	mu          sync.Mutex
	revoked     bool
	exchanges   int
	refreshes   int
	discoveries int
}

// Options configure a Server.
type Options struct {
	// Public makes the server treat the client as public: token requests
	// must carry a client_id and no client_secret, and the code exchange a
	// PKCE verifier, as Keycloak requires of a public client.
	Public bool
}

// Start runs a Server for a confidential client until the test ends.
func Start(t testing.TB) *Server {
	t.Helper()
	return StartWith(t, Options{})
}

// StartWith runs a Server with opts until the test ends.
func StartWith(t testing.TB, opts Options) *Server {
	t.Helper()
	s := &Server{public: opts.Public}
	mux := http.NewServeMux()
	mux.HandleFunc(IssuerPath+"/.well-known/openid-configuration", s.discovery)
	mux.HandleFunc("/", s.token)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// Endpoint points an oauth2.Config at s.
func (s *Server) Endpoint() oauth2.Endpoint {
	return oauth2.Endpoint{AuthURL: s.URL + "/auth", TokenURL: s.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}
}

// Issuer is the OIDC issuer URL whose discovery document s serves. Its
// endpoints are the same fake ones Endpoint names, under the issuer path.
func (s *Server) Issuer() string { return s.URL + IssuerPath }

// Discoveries counts discovery document requests.
func (s *Server) Discoveries() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.discoveries
}

func (s *Server) discovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	s.discoveries++
	s.mu.Unlock()
	issuer := s.Issuer()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{ // a failed write shows up as a client-side parse error
		"issuer":                           issuer,
		"authorization_endpoint":           issuer + "/auth",
		"token_endpoint":                   issuer + "/token",
		"jwks_uri":                         issuer + "/certs",
		"code_challenge_methods_supported": []string{"S256"},
	})
}

// Revoke makes the refresh token invalid.
func (s *Server) Revoke() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked = true
}

// Exchanges counts successful authorization code exchanges.
func (s *Server) Exchanges() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exchanges
}

// Refreshes counts successful refresh token grants.
func (s *Server) Refreshes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshes
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.clientOK(r.PostForm) {
		writeError(w, "invalid_client")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		if r.PostForm.Get("code") != Code || r.PostForm.Get("code_verifier") == "" {
			writeError(w, "invalid_grant")
			return
		}
		s.exchanges++
		writeToken(w, true)
	case "refresh_token":
		if s.revoked || r.PostForm.Get("refresh_token") != RefreshToken {
			writeError(w, "invalid_grant")
			return
		}
		s.refreshes++
		writeToken(w, false)
	default:
		writeError(w, "unsupported_grant_type")
	}
}

// clientOK checks the client authentication of a token request: the
// secret for a confidential client, a client_id and no secret for a public
// one.
func (s *Server) clientOK(form url.Values) bool {
	if s.public {
		_, hasSecret := form["client_secret"]
		return form.Get("client_id") != "" && !hasSecret
	}
	return form.Get("client_secret") == ClientSecret
}

func writeToken(w http.ResponseWriter, withRefresh bool) {
	body := map[string]any{"access_token": AccessToken, "token_type": "Bearer", "expires_in": 3600}
	if withRefresh {
		body["refresh_token"] = RefreshToken
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body) // a failed write shows up as a client-side parse error
}

func writeError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code}) // as above
}

// RedirectFor builds the URL a browser would land on after consenting to consentURL.
func RedirectFor(t testing.TB, consentURL, code string) string {
	t.Helper()
	u, err := url.Parse(consentURL)
	if err != nil {
		t.Fatalf("parsing consent URL: %v", err)
	}
	q := u.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		t.Fatalf("parsing redirect_uri: %v", err)
	}
	redirect.RawQuery = url.Values{"state": {q.Get("state")}, "code": {code}}.Encode()
	return redirect.String()
}
