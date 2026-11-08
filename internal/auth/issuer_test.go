package auth_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/testutil/oauthtest"
)

// issuerConfig is a Google account on client g and a custom account on the
// issuer client kc, whose issuer is the given URL.
func issuerConfig(issuer string) string {
	return testConfig + `
[oauth_clients.kc]
client_id = "chainmail"
issuer = "` + issuer + `"
scopes = ["openid", "offline_access"]

[accounts.mw]
provider = "custom"
address = "james@example.org"
username = "james"
auth = "oauth2"
oauth_client = "kc"
imap = { host = "127.0.0.1", port = 993 }
smtp = { host = "127.0.0.1", port = 465 }
`
}

// issuerManager builds a manager for issuerConfig(issuer) with the given
// options and an empty store.
func issuerManager(t *testing.T, issuer string, opts ...auth.Option) (*auth.Manager, *memStore) {
	t.Helper()
	cfg, err := config.Parse([]byte(issuerConfig(issuer)))
	if err != nil {
		t.Fatal(err)
	}
	store := &memStore{m: map[string][]byte{}}
	return auth.NewManager(cfg, store, opts...), store
}

// login runs a whole login for account against the oauthtest server the
// manager's endpoints lead to, returning the consent URL's query.
func login(t *testing.T, m *auth.Manager, account string) url.Values {
	t.Helper()
	ch, err := m.BeginLogin(t.Context(), account)
	if err != nil {
		t.Fatalf("BeginLogin(%s): %v", account, err)
	}
	if err := m.FinishLogin(t.Context(), account, oauthtest.RedirectFor(t, ch.URL, oauthtest.Code)); err != nil {
		t.Fatalf("FinishLogin(%s): %v", account, err)
	}
	u, err := url.Parse(ch.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

// A public issuer client logs in with PKCE and refreshes with no client
// secret: the public oauthtest server refuses any request that carries
// one. Its endpoints come from one discovery.
func TestIssuerPublicClientLoginAndRefresh(t *testing.T) {
	t.Parallel()
	srv := oauthtest.StartWith(t, oauthtest.Options{Public: true})
	m, store := issuerManager(t, srv.Issuer())
	ch, err := m.BeginLogin(t.Context(), "mw")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	u, err := url.Parse(ch.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := u.Scheme+"://"+u.Host+u.Path, srv.Issuer()+"/auth"; got != want {
		t.Errorf("consent endpoint = %s, want %s from discovery", got, want)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"client_id":             "chainmail",
		"scope":                 "openid offline_access",
		"code_challenge_method": "S256",
		"login_hint":            "james@example.org",
		"redirect_uri":          auth.LoginRedirectURL,
	} {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if err := m.FinishLogin(t.Context(), "mw", oauthtest.RedirectFor(t, ch.URL, oauthtest.Code)); err != nil {
		t.Fatalf("FinishLogin: %v", err)
	}
	if srv.Exchanges() != 1 {
		t.Errorf("exchanges = %d, want 1", srv.Exchanges())
	}
	if rt, _ := store.Get(auth.RefreshTokenSecret("mw")); string(rt) != oauthtest.RefreshToken {
		t.Errorf("refresh token = %q, want it stored", rt)
	}
	creds, err := m.Credentials(t.Context(), "mw")
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if creds.Method != provider.AuthOAuth2 || creds.Username != "james" || creds.Secret != oauthtest.AccessToken {
		t.Errorf("creds = %v %s, want the access token for james", creds.Method, creds.Username)
	}
	if srv.Discoveries() != 1 {
		t.Errorf("discoveries = %d after login and credentials, want 1", srv.Discoveries())
	}

	// A new manager starts from the stored refresh token and refreshes.
	m2 := auth.NewManager(mustParse(t, issuerConfig(srv.Issuer())), store)
	if _, err := m2.Credentials(t.Context(), "mw"); err != nil {
		t.Fatalf("Credentials after restart: %v", err)
	}
	if srv.Refreshes() != 1 {
		t.Errorf("refreshes = %d, want 1", srv.Refreshes())
	}
}

func mustParse(t *testing.T, s string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// An issuer client with a secret in the vault is confidential and sends it.
func TestIssuerConfidentialClientSendsItsSecret(t *testing.T) {
	t.Parallel()
	srv := oauthtest.Start(t)
	m, store := issuerManager(t, srv.Issuer())
	if err := store.Set(auth.ClientSecret("kc"), []byte(oauthtest.ClientSecret)); err != nil {
		t.Fatal(err)
	}
	login(t, m, "mw")
	if srv.Exchanges() != 1 {
		t.Errorf("exchanges = %d, want 1", srv.Exchanges())
	}
}

// An issuer client needs no client secret: only the refresh token makes a
// login necessary.
func TestIssuerStateNeedsOnlyTheRefreshToken(t *testing.T) {
	t.Parallel()
	srv := oauthtest.StartWith(t, oauthtest.Options{Public: true})
	m, store := issuerManager(t, srv.Issuer())
	st, err := m.State("mw")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Required || st.Detail != "run: chainmail auth login mw" {
		t.Errorf("state = %+v, want a login required", st)
	}
	if err := store.Set(auth.RefreshTokenSecret("mw"), []byte(oauthtest.RefreshToken)); err != nil {
		t.Fatal(err)
	}
	if st, err := m.State("mw"); err != nil || st.Required {
		t.Errorf("state = %+v, %v; want ready without a client secret", st, err)
	}
	if srv.Discoveries() != 0 {
		t.Errorf("State fetched discovery %d times, want none", srv.Discoveries())
	}
}

// Setting the optional secret of an issuer client resets the accounts on
// it, as for any other client.
func TestIssuerClientSecretChangeResetsAccount(t *testing.T) {
	t.Parallel()
	srv := oauthtest.StartWith(t, oauthtest.Options{Public: true})
	m, store := issuerManager(t, srv.Issuer())
	if err := store.Set(auth.RefreshTokenSecret("mw"), []byte("bad")); err != nil {
		t.Fatal(err)
	}
	_, err := m.Credentials(t.Context(), "mw")
	requireHint(t, err, "chainmail auth login mw")
	m.SecretChanged(auth.ClientSecret("kc"))
	if st, _ := m.State("mw"); st.Required {
		t.Errorf("state = %+v after the client secret changed, want the park cleared", st)
	}
}

// Discovery runs once per client, however many logins and refreshes and
// however many at once.
func TestIssuerDiscoveryIsCached(t *testing.T) {
	t.Parallel()
	srv := oauthtest.StartWith(t, oauthtest.Options{Public: true})
	m, _ := issuerManager(t, srv.Issuer())
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := m.BeginLogin(t.Context(), "mw"); err != nil {
				t.Errorf("BeginLogin: %v", err)
			}
		})
	}
	wg.Wait()
	login(t, m, "mw")
	if srv.Discoveries() != 1 {
		t.Errorf("discoveries = %d, want 1", srv.Discoveries())
	}
}

// WithOAuthEndpoint replaces the endpoints of preset clients only. An
// issuer client still discovers its own.
func TestOAuthEndpointOverrideLeavesIssuerClients(t *testing.T) {
	t.Parallel()
	google := oauthtest.Start(t)
	keycloak := oauthtest.StartWith(t, oauthtest.Options{Public: true})
	m, store := issuerManager(t, keycloak.Issuer(), auth.WithOAuthEndpoint(google.Endpoint()))
	if err := store.Set(auth.ClientSecret("g"), []byte(oauthtest.ClientSecret)); err != nil {
		t.Fatal(err)
	}
	q := login(t, m, "gm")
	login(t, m, "mw")
	if google.Exchanges() != 1 || keycloak.Exchanges() != 1 {
		t.Errorf("exchanges: google %d, keycloak %d; want 1 each", google.Exchanges(), keycloak.Exchanges())
	}
	// The Google account asks for mail and, with its default google
	// calendar store, the calendar scopes.
	want := strings.Join(append([]string{"https://mail.google.com/"}, provider.GoogleCalendarScopes...), " ")
	if got := q.Get("scope"); got != want {
		t.Errorf("gm scope = %q, want %q", got, want)
	}
}

// discoveryServer serves handler at the discovery path of its issuer,
// counting requests.
func discoveryServer(t *testing.T, handler func(w http.ResponseWriter, issuer string)) (string, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	var issuer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/realms/r/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		n.Add(1)
		handler(w, issuer)
	}))
	t.Cleanup(srv.Close)
	issuer = srv.URL + "/realms/r"
	return issuer, &n
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v) // a failed write shows up as a client-side error
}

// A discovery that fails is the endpoint's trouble, reported without the
// response body and not cached.
func TestIssuerDiscoveryFailures(t *testing.T) {
	t.Parallel()
	const marker = "body-marker-5be1"
	tests := map[string]func(w http.ResponseWriter, issuer string){
		"server error": func(w http.ResponseWriter, _ string) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, marker)
		},
		"not JSON": func(w http.ResponseWriter, _ string) { _, _ = io.WriteString(w, marker) },
		"another issuer": func(w http.ResponseWriter, _ string) {
			writeJSON(w, map[string]string{"issuer": "https://" + marker, "authorization_endpoint": "https://a/auth", "token_endpoint": "https://a/token"})
		},
		"no token endpoint": func(w http.ResponseWriter, issuer string) {
			writeJSON(w, map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/auth"})
		},
		"plain http endpoint": func(w http.ResponseWriter, issuer string) {
			writeJSON(w, map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/auth", "token_endpoint": "http://" + marker + ".example/token"})
		},
		"too large": func(w http.ResponseWriter, issuer string) {
			writeJSON(w, map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/auth", "token_endpoint": issuer + "/token", "pad": strings.Repeat("x", 2<<20)})
		},
	}
	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			issuer, n := discoveryServer(t, handler)
			m, _ := issuerManager(t, issuer)
			for attempt := 1; attempt <= 2; attempt++ {
				_, err := m.BeginLogin(t.Context(), "mw")
				if !errors.Is(err, auth.ErrTokenEndpoint) {
					t.Fatalf("attempt %d: err = %v, want ErrTokenEndpoint", attempt, err)
				}
				if strings.Contains(err.Error(), marker) {
					t.Errorf("err = %q, quotes the response", err)
				}
			}
			if n.Load() != 2 {
				t.Errorf("discoveries = %d, want 2: a failure is not cached", n.Load())
			}
		})
	}
}

// A provider that issues no refresh token gets a hint about offline access,
// not Google's permissions page.
func TestIssuerNoRefreshTokenHintIsProviderNeutral(t *testing.T) {
	t.Parallel()
	var tokenURL string
	issuer, _ := discoveryServer(t, func(w http.ResponseWriter, issuer string) {
		writeJSON(w, map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/auth", "token_endpoint": tokenURL})
	})
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"access_token": "a", "token_type": "Bearer", "expires_in": 3600})
	}))
	t.Cleanup(tokens.Close)
	tokenURL = tokens.URL + "/token"
	m, _ := issuerManager(t, issuer)
	ch, err := m.BeginLogin(t.Context(), "mw")
	if err != nil {
		t.Fatal(err)
	}
	err = m.FinishLogin(t.Context(), "mw", oauthtest.RedirectFor(t, ch.URL, "code"))
	if !errors.Is(err, auth.ErrNoRefreshToken) {
		t.Fatalf("err = %v, want ErrNoRefreshToken", err)
	}
	if msg := err.Error(); strings.Contains(msg, "google") || !strings.Contains(msg, "offline_access") {
		t.Errorf("err = %q, want a hint about offline_access that does not name Google", msg)
	}
}

// A refused issuer client is not told to set a secret it may not have.
func TestIssuerClientRejectedHint(t *testing.T) {
	t.Parallel()
	var tokenURL string
	issuer, _ := discoveryServer(t, func(w http.ResponseWriter, issuer string) {
		writeJSON(w, map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/auth", "token_endpoint": tokenURL})
	})
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
	}))
	t.Cleanup(tokens.Close)
	tokenURL = tokens.URL + "/token"
	m, store := issuerManager(t, issuer)
	if err := store.Set(auth.RefreshTokenSecret("mw"), []byte("rt")); err != nil {
		t.Fatal(err)
	}
	_, err := m.Credentials(t.Context(), "mw")
	requireHint(t, err, "check client_id in oauth_clients.kc")
}
