// Package oauthtest runs a fake OAuth2 token endpoint for tests.
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

// Server is a fake token endpoint. It accepts Code with any PKCE verifier and
// RefreshToken until Revoke is called.
type Server struct {
	URL string

	mu        sync.Mutex
	revoked   bool
	exchanges int
	refreshes int
}

// Start runs a Server until the test ends.
func Start(t testing.TB) *Server {
	t.Helper()
	s := &Server{}
	srv := httptest.NewServer(http.HandlerFunc(s.token))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// Endpoint points an oauth2.Config at s.
func (s *Server) Endpoint() oauth2.Endpoint {
	return oauth2.Endpoint{AuthURL: s.URL + "/auth", TokenURL: s.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}
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
	if r.PostForm.Get("client_secret") != ClientSecret {
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
