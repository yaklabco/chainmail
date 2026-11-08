package auth_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/oauth2"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/testutil/oauthtest"
)

// sinkServer counts the requests it gets and keeps their bodies, which
// must stay empty when nothing is to reach it.
func sinkServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		_, _ = io.Copy(io.Discard, r.Body) // the count is what the test checks
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// locationLeak marks the Location header of a refused redirect, which is the
// server's text and must not reach an error.
const locationLeak = "location-leak-6a2d"

// A token endpoint never legitimately redirects a POST. A 307 or 308 would
// make net/http send the form again, with the refresh token, the code and
// the PKCE verifier, so no redirect is followed, to another host or to the
// same one. The refusal does not quote the Location, even a malformed one.
func TestTokenRequestsFollowNoRedirect(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, target := range []string{"another host", "same host", "malformed"} {
			t.Run(fmt.Sprintf("%d to %s", status, target), func(t *testing.T) {
				t.Parallel()
				sink, sunk := sinkServer(t)
				var sameHost atomic.Int32
				tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/elsewhere" {
						sameHost.Add(1)
						http.NotFound(w, r)
						return
					}
					loc := map[string]string{
						"another host": sink.URL + "/token?" + locationLeak,
						"same host":    "/elsewhere?" + locationLeak,
						"malformed":    "http://bad host/?" + locationLeak,
					}[target]
					w.Header().Set("Location", loc)
					w.WriteHeader(status)
				}))
				t.Cleanup(tokens.Close)
				ep := oauth2.Endpoint{AuthURL: tokens.URL + "/auth", TokenURL: tokens.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}
				f := newFixtureAt(t, ep)
				f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
				f.set(t, auth.RefreshTokenSecret("gm"), oauthtest.RefreshToken)

				_, err := f.m.Credentials(t.Context(), "gm")
				if !errors.Is(err, auth.ErrTokenEndpoint) {
					t.Errorf("refresh err = %v, want ErrTokenEndpoint", err)
				}
				if err != nil {
					requireNoSecrets(t, "refresh error", err.Error(), locationLeak)
				}
				ch, err := f.m.BeginLogin(t.Context(), "gm")
				if err != nil {
					t.Fatal(err)
				}
				err = f.m.FinishLogin(t.Context(), "gm", oauthtest.RedirectFor(t, ch.URL, oauthtest.Code))
				if !errors.Is(err, auth.ErrTokenEndpoint) {
					t.Errorf("exchange err = %v, want ErrTokenEndpoint", err)
				}
				if err != nil {
					requireNoSecrets(t, "exchange error", err.Error(), locationLeak)
				}
				if sunk.Load() != 0 || sameHost.Load() != 0 {
					t.Errorf("redirect targets got %d (other host) and %d (same host) requests, want none", sunk.Load(), sameHost.Load())
				}
			})
		}
	}
}

// tlsDiscoveryIssuer serves, over https, a discovery document at
// /realms/r/.well-known/openid-configuration that redirects to location(srv).
// It returns the issuer and a client that trusts the server.
func tlsDiscoveryIssuer(t *testing.T, location func(srv *httptest.Server) string) (string, *http.Client) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issuer := srv.URL + "/realms/r"
		switch r.URL.Path {
		case "/realms/r/.well-known/openid-configuration":
			w.Header().Set("Location", location(srv))
			w.WriteHeader(http.StatusFound)
		case "/moved":
			writeJSON(w, map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/auth", "token_endpoint": issuer + "/token"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/realms/r", srv.Client()
}

// Discovery follows a redirect only on the same host and scheme, so the
// endpoints it trusts come from the issuer. A redirect to plain http or to
// another host is refused, even when the document there names the issuer.
// No refusal quotes the Location, which is the server's text.
func TestIssuerDiscoveryRedirects(t *testing.T) {
	t.Parallel()
	// elsewhere serves a document naming whichever issuer asked, at
	// /moved, over plain http or https.
	elsewhere := func(t *testing.T, newServer func(http.Handler) *httptest.Server) (*httptest.Server, *atomic.Int32, *atomic.Pointer[string]) {
		t.Helper()
		var n atomic.Int32
		var issuer atomic.Pointer[string]
		srv := newServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n.Add(1)
			iss := *issuer.Load()
			writeJSON(w, map[string]string{"issuer": iss, "authorization_endpoint": iss + "/auth", "token_endpoint": iss + "/token"})
		}))
		t.Cleanup(srv.Close)
		return srv, &n, &issuer
	}
	for name, newServer := range map[string]func(http.Handler) *httptest.Server{
		"https to http":               httptest.NewServer,
		"https to another https host": httptest.NewTLSServer,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			other, n, otherIssuer := elsewhere(t, newServer)
			issuer, client := tlsDiscoveryIssuer(t, func(*httptest.Server) string { return other.URL + "/moved?" + locationLeak })
			otherIssuer.Store(&issuer)
			m, _ := issuerManager(t, issuer, auth.WithHTTPClient(client))
			_, err := m.BeginLogin(t.Context(), "mw")
			if !errors.Is(err, auth.ErrTokenEndpoint) {
				t.Errorf("BeginLogin err = %v, want ErrTokenEndpoint", err)
			}
			if err != nil {
				requireNoSecrets(t, "discovery error", err.Error(), locationLeak)
			}
			if n.Load() != 0 {
				t.Errorf("the redirect target got %d requests, want none", n.Load())
			}
		})
	}
	for name, location := range map[string]func(srv *httptest.Server) string{
		"malformed location": func(*httptest.Server) string { return "https://bad host/?" + locationLeak },
		"redirect loop": func(srv *httptest.Server) string {
			return srv.URL + "/realms/r/.well-known/openid-configuration?" + locationLeak
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			issuer, client := tlsDiscoveryIssuer(t, location)
			m, _ := issuerManager(t, issuer, auth.WithHTTPClient(client))
			_, err := m.BeginLogin(t.Context(), "mw")
			if !errors.Is(err, auth.ErrTokenEndpoint) {
				t.Fatalf("BeginLogin err = %v, want ErrTokenEndpoint", err)
			}
			requireNoSecrets(t, "discovery error", err.Error(), locationLeak)
		})
	}
	t.Run("same host and scheme", func(t *testing.T) {
		t.Parallel()
		issuer, client := tlsDiscoveryIssuer(t, func(srv *httptest.Server) string { return srv.URL + "/moved" })
		m, _ := issuerManager(t, issuer, auth.WithHTTPClient(client))
		ch, err := m.BeginLogin(t.Context(), "mw")
		if err != nil {
			t.Fatalf("BeginLogin: %v", err)
		}
		if !strings.HasPrefix(ch.URL, issuer+"/auth?") {
			t.Errorf("consent URL = %q, want the issuer's endpoint", ch.URL)
		}
	})
}
