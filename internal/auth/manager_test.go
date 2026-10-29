package auth_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/testutil/oauthtest"
	"github.com/jamesainslie/chainmail/internal/vault"
)

const testConfig = `
[oauth_clients.g]
client_id = "client-id"

[accounts.gm]
provider = "gmail"
address = "me@gmail.com"
auth = "oauth2"
oauth_client = "g"

[accounts.host]
provider = "hostinger"
address = "me@example.com"
`

type memStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *memStore) Get(name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", vault.ErrNotFound, name)
	}
	return v, nil
}

func (s *memStore) Set(name string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[name] = value
	return nil
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fixture struct {
	m     *auth.Manager
	store *memStore
	clock *clock
	oauth *oauthtest.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	srv := oauthtest.Start(t)
	f := newFixtureAt(t, srv.Endpoint())
	f.oauth = srv
	return f
}

// newFixtureAt builds a fixture whose OAuth2 requests go to ep.
func newFixtureAt(t *testing.T, ep oauth2.Endpoint) *fixture {
	t.Helper()
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		store: &memStore{m: map[string][]byte{}},
		clock: &clock{now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)},
	}
	f.m = auth.NewManager(cfg, f.store, auth.WithClock(f.clock.Now), auth.WithOAuthEndpoint(ep))
	return f
}

// credentials returns account's credentials, failing the test on error.
func (f *fixture) credentials(t *testing.T, account string) auth.Credentials {
	t.Helper()
	c, err := f.m.Credentials(t.Context(), account)
	if err != nil {
		t.Fatalf("Credentials(%s): %v", account, err)
	}
	return c
}

func (f *fixture) set(t *testing.T, name, value string) {
	t.Helper()
	if err := f.store.Set(name, []byte(value)); err != nil {
		t.Fatal(err)
	}
}

// cannedTokenServer answers every token request with status and body, and
// counts the requests.
func cannedTokenServer(t *testing.T, status int, body string) (oauth2.Endpoint, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body) // a failed write shows up as a client-side error
	}))
	t.Cleanup(srv.Close)
	return oauth2.Endpoint{AuthURL: srv.URL + "/auth", TokenURL: srv.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}, &n
}

func requireHint(t *testing.T, err error, hint string) {
	t.Helper()
	var re *auth.RequiredError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want *auth.RequiredError", err)
	}
	if !strings.Contains(re.Hint, hint) {
		t.Errorf("hint = %q, want it to contain %q", re.Hint, hint)
	}
}

func requireNoSecrets(t *testing.T, what, s string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(s, secret) {
			t.Errorf("%s contains secret %q: %s", what, secret, s)
		}
	}
}

func TestPasswordCredentials(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.PasswordSecret("host"), "pw")
	c, err := f.m.Credentials(t.Context(), "host")
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if c.Method != provider.AuthPassword || c.Username != "me@example.com" || c.Secret != "pw" {
		t.Errorf("Credentials = %+v", c)
	}
}

func TestMissingPasswordIsRequired(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := f.m.Credentials(t.Context(), "host")
	requireHint(t, err, "chainmail secret set accounts/host")
	st, err := f.m.State("host")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Required || !strings.Contains(st.Detail, "secret set accounts/host") {
		t.Errorf("State = %+v", st)
	}
}

func TestRejectedUntilSecretChanges(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.PasswordSecret("host"), "pw")
	f.m.Rejected("host", f.credentials(t, "host"))
	_, err := f.m.Credentials(t.Context(), "host")
	requireHint(t, err, "rejected")
	if st, _ := f.m.State("host"); !st.Required {
		t.Error("State not required after rejection")
	}
	f.m.SecretChanged(auth.PasswordSecret("host"))
	if _, err := f.m.Credentials(t.Context(), "host"); err != nil {
		t.Errorf("Credentials after SecretChanged: %v", err)
	}
}

func TestRejectionOfReplacedSecretIsIgnored(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.PasswordSecret("host"), "old-pw")
	old := f.credentials(t, "host")
	// A human fixes the password while a login with the old one is in flight.
	f.set(t, auth.PasswordSecret("host"), "new-pw")
	f.m.SecretChanged(auth.PasswordSecret("host"))
	f.m.Rejected("host", old)
	if st, _ := f.m.State("host"); st.Required {
		t.Errorf("State = %+v, want ok: the rejected password was already replaced", st)
	}
	c := f.credentials(t, "host")
	if c.Secret != "new-pw" {
		t.Errorf("Secret = %q, want the new password", c.Secret)
	}
}

func TestRejectionOfCurrentCredentialsParks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.PasswordSecret("host"), "pw")
	f.m.Rejected("host", f.credentials(t, "host"))
	if st, _ := f.m.State("host"); !st.Required {
		t.Errorf("State = %+v, want required", st)
	}
}

func TestOtherAccountsChangesDoNotHideRejection(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.PasswordSecret("host"), "pw")
	creds := f.credentials(t, "host")
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	f.m.SecretChanged(auth.ClientSecret("g"))
	f.m.Rejected("host", creds)
	if st, _ := f.m.State("host"); !st.Required {
		t.Errorf("State = %+v, want required: gm's secret changing says nothing about host", st)
	}
}

func TestRejectionBeforeFinishLoginIsIgnored(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	f.set(t, auth.RefreshTokenSecret("gm"), oauthtest.RefreshToken)
	old := f.credentials(t, "gm")
	ch, err := f.m.BeginLogin("gm")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.FinishLogin(t.Context(), "gm", oauthtest.RedirectFor(t, ch.URL, oauthtest.Code)); err != nil {
		t.Fatal(err)
	}
	f.m.Rejected("gm", old)
	if st, _ := f.m.State("gm"); st.Required {
		t.Errorf("State = %+v, want ok: the login replaced the rejected token", st)
	}
}

// hookStore runs hook once, the first time name is read, before the read.
// Tests use it to land a "chainmail secret set" between the manager reading
// an account's generation and reading its secret.
type hookStore struct {
	*memStore
	name string
	hook func()
	once sync.Once
}

func (s *hookStore) Get(name string) ([]byte, error) {
	if name == s.name {
		s.once.Do(s.hook)
	}
	return s.memStore.Get(name)
}

func newHookManager(t *testing.T, store auth.SecretStore, ep oauth2.Endpoint) *auth.Manager {
	t.Helper()
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	return auth.NewManager(cfg, store, auth.WithOAuthEndpoint(ep))
}

// A password set while Credentials reads it is tagged with the generation
// it belongs to, so a rejection of the new password parks the account.
func TestCredentialsTagMatchesPasswordRead(t *testing.T) {
	t.Parallel()
	store := &hookStore{memStore: &memStore{m: map[string][]byte{auth.PasswordSecret("host"): []byte("old-pw")}}, name: auth.PasswordSecret("host")}
	m := newHookManager(t, store, oauthtest.Start(t).Endpoint())
	store.hook = func() {
		_ = store.Set(auth.PasswordSecret("host"), []byte("new-pw")) // memStore.Set never fails
		m.SecretChanged(auth.PasswordSecret("host"))
	}
	c, err := m.Credentials(t.Context(), "host")
	if err != nil {
		t.Fatal(err)
	}
	if c.Secret != "new-pw" {
		t.Fatalf("Secret = %q, want new-pw", c.Secret)
	}
	m.Rejected("host", c)
	if st, _ := m.State("host"); !st.Required {
		t.Errorf("State = %+v, want required: the server rejected the password it was given", st)
	}
}

// The same holds for an access token refreshed from a secret that changed
// while it was read.
func TestCredentialsTagMatchesTokenRefresh(t *testing.T) {
	t.Parallel()
	store := &hookStore{memStore: &memStore{m: map[string][]byte{
		auth.ClientSecret("g"):        []byte(oauthtest.ClientSecret),
		auth.RefreshTokenSecret("gm"): []byte(oauthtest.RefreshToken),
	}}, name: auth.RefreshTokenSecret("gm")}
	m := newHookManager(t, store, oauthtest.Start(t).Endpoint())
	store.hook = func() { m.SecretChanged(auth.RefreshTokenSecret("gm")) }
	c, err := m.Credentials(t.Context(), "gm")
	if err != nil {
		t.Fatal(err)
	}
	m.Rejected("gm", c)
	if st, _ := m.State("gm"); !st.Required {
		t.Errorf("State = %+v, want required", st)
	}
}

// A refresh refusal parks its own account at once, even if another
// account's secret changed meanwhile.
func TestRefreshRefusalIgnoresOtherAccountsChanges(t *testing.T) {
	t.Parallel()
	ep, n := cannedTokenServer(t, http.StatusBadRequest, `{"error":"invalid_grant"}`)
	store := &hookStore{memStore: &memStore{m: map[string][]byte{
		auth.ClientSecret("g"):        []byte(oauthtest.ClientSecret),
		auth.RefreshTokenSecret("gm"): []byte("revoked"),
	}}, name: auth.RefreshTokenSecret("gm")}
	m := newHookManager(t, store, ep)
	store.hook = func() { m.SecretChanged(auth.PasswordSecret("host")) }
	_, err := m.Credentials(t.Context(), "gm")
	requireHint(t, err, "chainmail auth login gm")
	if st, _ := m.State("gm"); !st.Required {
		t.Errorf("State = %+v, want required", st)
	}
	if got := n.Load(); got != 1 {
		t.Errorf("token requests = %d, want 1: host's change says nothing about gm", got)
	}
}

func TestUnknownAccount(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := f.m.Credentials(t.Context(), "nope"); !errors.Is(err, config.ErrUnknownAccount) {
		t.Errorf("err = %v", err)
	}
}

func TestBeginLogin(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	ch, err := f.m.BeginLogin("gm")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	u, err := url.Parse(ch.URL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	checks := map[string]string{
		"client_id":             "client-id",
		"redirect_uri":          auth.LoginRedirectURL,
		"code_challenge_method": "S256",
		"access_type":           "offline",
		"prompt":                "consent",
		"login_hint":            "me@gmail.com",
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if q.Get("state") == "" || q.Get("code_challenge") == "" {
		t.Error("state or code_challenge missing")
	}
	if want := f.clock.Now().Add(10 * time.Minute); !ch.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", ch.ExpiresAt, want)
	}
	requireNoSecrets(t, "consent URL", ch.URL, oauthtest.ClientSecret)
}

func TestBeginLoginNeedsClientSecret(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := f.m.BeginLogin("gm")
	requireHint(t, err, "chainmail secret set oauth_clients/g")
}

func TestBeginLoginRejectsPasswordAccount(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := f.m.BeginLogin("host"); !errors.Is(err, auth.ErrNotOAuth) {
		t.Errorf("err = %v, want ErrNotOAuth", err)
	}
}

func TestFinishLogin(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	ch, err := f.m.BeginLogin("gm")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.FinishLogin(t.Context(), "gm", oauthtest.RedirectFor(t, ch.URL, oauthtest.Code)); err != nil {
		t.Fatalf("FinishLogin: %v", err)
	}
	rt, err := f.store.Get(auth.RefreshTokenSecret("gm"))
	if err != nil || string(rt) != oauthtest.RefreshToken {
		t.Errorf("stored refresh token = %q, %v", rt, err)
	}
	c, err := f.m.Credentials(t.Context(), "gm")
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if c.Method != provider.AuthOAuth2 || c.Secret != oauthtest.AccessToken || c.Username != "me@gmail.com" {
		t.Errorf("Credentials = %+v", c)
	}
	if f.oauth.Exchanges() != 1 || f.oauth.Refreshes() != 0 {
		t.Errorf("exchanges = %d, refreshes = %d; want the exchanged token reused", f.oauth.Exchanges(), f.oauth.Refreshes())
	}
}

func TestFinishLoginErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		begin   bool
		advance time.Duration
		url     func(consent string) string
		want    error
	}{
		{
			name: "no login in progress",
			url:  func(string) string { return auth.LoginRedirectURL + "?state=x&code=" + oauthtest.Code },
			want: auth.ErrNoPendingLogin,
		},
		{
			name:  "wrong state",
			begin: true,
			url:   func(string) string { return auth.LoginRedirectURL + "?state=wrong&code=" + oauthtest.Code },
			want:  auth.ErrStateMismatch,
		},
		{
			name:    "expired",
			begin:   true,
			advance: 11 * time.Minute,
			url:     func(c string) string { return c },
			want:    auth.ErrLoginExpired,
		},
		{
			name:  "consent denied",
			begin: true,
			url:   func(string) string { return auth.LoginRedirectURL + "?error=access_denied" },
			want:  auth.ErrConsentDenied,
		},
		{
			name:  "bad code",
			begin: true,
			url:   func(c string) string { return c },
			want:  auth.ErrRejected,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
			redirect := ""
			if tt.begin {
				ch, err := f.m.BeginLogin("gm")
				if err != nil {
					t.Fatal(err)
				}
				redirect = oauthtest.RedirectFor(t, ch.URL, "not-the-code")
			}
			f.clock.Advance(tt.advance)
			err := f.m.FinishLogin(t.Context(), "gm", tt.url(redirect))
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestFinishLoginUnparsableURLHidesCode(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	if _, err := f.m.BeginLogin("gm"); err != nil {
		t.Fatal(err)
	}
	err := f.m.FinishLogin(t.Context(), "gm", "http://%zz/?code="+oauthtest.Code)
	if !errors.Is(err, auth.ErrStateMismatch) {
		t.Fatalf("err = %v, want ErrStateMismatch", err)
	}
	requireNoSecrets(t, "error", err.Error(), oauthtest.Code)
}

func TestCredentialsRefreshOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	f.set(t, auth.RefreshTokenSecret("gm"), oauthtest.RefreshToken)
	for range 2 {
		c, err := f.m.Credentials(t.Context(), "gm")
		if err != nil {
			t.Fatalf("Credentials: %v", err)
		}
		if c.Secret != oauthtest.AccessToken {
			t.Errorf("Secret = %q", c.Secret)
		}
	}
	if n := f.oauth.Refreshes(); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
}

func TestConcurrentCredentialsRefreshOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	f.set(t, auth.RefreshTokenSecret("gm"), oauthtest.RefreshToken)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := f.m.Credentials(t.Context(), "gm"); err != nil {
				t.Errorf("Credentials: %v", err)
			}
		})
	}
	wg.Wait()
	if n := f.oauth.Refreshes(); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
}

func TestAccessTokenRefreshesFiveMinutesEarly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		expiresIn int
		want      int32
	}{
		{expiresIn: 360, want: 1}, // still a minute of use left after the 5 minute margin
		{expiresIn: 290, want: 2}, // already inside the margin, so every call refreshes
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.expiresIn), func(t *testing.T) {
			t.Parallel()
			body := fmt.Sprintf(`{"access_token":"at","token_type":"Bearer","expires_in":%d}`, tt.expiresIn)
			ep, n := cannedTokenServer(t, http.StatusOK, body)
			f := newFixtureAt(t, ep)
			f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
			f.set(t, auth.RefreshTokenSecret("gm"), oauthtest.RefreshToken)
			for range 2 {
				if _, err := f.m.Credentials(t.Context(), "gm"); err != nil {
					t.Fatalf("Credentials: %v", err)
				}
			}
			if got := n.Load(); got != tt.want {
				t.Errorf("token requests = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestTokenEndpointErrorsHideBodies(t *testing.T) {
	t.Parallel()
	const leak = "body-only-value"
	ep, _ := cannedTokenServer(t, http.StatusInternalServerError, `{"oops":"`+leak+`"}`)
	f := newFixtureAt(t, ep)
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	f.set(t, auth.RefreshTokenSecret("gm"), oauthtest.RefreshToken)
	secrets := []string{leak, oauthtest.ClientSecret, oauthtest.RefreshToken}

	_, err := f.m.Credentials(t.Context(), "gm")
	if err == nil {
		t.Fatal("Credentials succeeded against a failing token endpoint")
	}
	if errors.Is(err, auth.ErrAuthRequired) {
		t.Errorf("err = %v; a server error should not need a human", err)
	}
	requireNoSecrets(t, "refresh error", err.Error(), secrets...)

	ch, err := f.m.BeginLogin("gm")
	if err != nil {
		t.Fatal(err)
	}
	err = f.m.FinishLogin(t.Context(), "gm", oauthtest.RedirectFor(t, ch.URL, oauthtest.Code))
	if err == nil {
		t.Fatal("FinishLogin succeeded against a failing token endpoint")
	}
	requireNoSecrets(t, "exchange error", err.Error(), append(secrets, oauthtest.Code)...)
}

// A token endpoint that is down, or answers with an error chainmail does not
// know, says nothing about the secrets: the account stays usable and the
// error matches ErrTokenEndpoint, which the daemon reports as unavailable.
func TestTokenEndpointOutageIsUnavailable(t *testing.T) {
	t.Parallel()
	const leak = "body-only-value"
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "503", status: http.StatusServiceUnavailable, body: `{"oops":"` + leak + `"}`},
		{name: "unknown error code", status: http.StatusBadRequest, body: `{"error":"temporarily_unavailable","error_description":"` + leak + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ep, _ := cannedTokenServer(t, tt.status, tt.body)
			f := newFixtureAt(t, ep)
			f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
			f.set(t, auth.RefreshTokenSecret("gm"), oauthtest.RefreshToken)
			_, err := f.m.Credentials(t.Context(), "gm")
			if !errors.Is(err, auth.ErrTokenEndpoint) || errors.Is(err, auth.ErrAuthRequired) {
				t.Errorf("Credentials err = %v, want ErrTokenEndpoint and not ErrAuthRequired", err)
			}
			if err != nil {
				requireNoSecrets(t, "refresh error", err.Error(), leak, oauthtest.RefreshToken, oauthtest.ClientSecret)
			}
			if st, _ := f.m.State("gm"); st.Required {
				t.Errorf("State = %+v, want ok after an outage", st)
			}
		})
	}
}

func TestTokenExchangeOutageIsUnavailable(t *testing.T) {
	t.Parallel()
	ep, _ := cannedTokenServer(t, http.StatusServiceUnavailable, `{}`)
	f := newFixtureAt(t, ep)
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	ch, err := f.m.BeginLogin("gm")
	if err != nil {
		t.Fatal(err)
	}
	err = f.m.FinishLogin(t.Context(), "gm", oauthtest.RedirectFor(t, ch.URL, oauthtest.Code))
	if !errors.Is(err, auth.ErrTokenEndpoint) || errors.Is(err, auth.ErrRejected) {
		t.Errorf("FinishLogin err = %v, want ErrTokenEndpoint and not ErrRejected", err)
	}
}

func TestUnreachableTokenEndpointIsUnavailable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	ep := oauth2.Endpoint{AuthURL: srv.URL + "/auth", TokenURL: srv.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}
	srv.Close()
	f := newFixtureAt(t, ep)
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	f.set(t, auth.RefreshTokenSecret("gm"), oauthtest.RefreshToken)
	if _, err := f.m.Credentials(t.Context(), "gm"); !errors.Is(err, auth.ErrTokenEndpoint) {
		t.Errorf("Credentials err = %v, want ErrTokenEndpoint", err)
	}
}

func TestManagerPrintsNoSecrets(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	ch, err := f.m.BeginLogin("gm")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(ch.URL)
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	pending := fmt.Sprintf("%v %+v %#v %s", f.m, f.m, f.m, f.m)
	requireNoSecrets(t, "manager with a pending login", pending, state)

	if err := f.m.FinishLogin(t.Context(), "gm", oauthtest.RedirectFor(t, ch.URL, oauthtest.Code)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Credentials(t.Context(), "gm"); err != nil {
		t.Fatal(err)
	}
	loggedIn := fmt.Sprintf("%v %+v %#v %s", f.m, f.m, f.m, f.m)
	requireNoSecrets(t, "logged-in manager", loggedIn, oauthtest.AccessToken, oauthtest.RefreshToken, oauthtest.ClientSecret)
}

func TestRevokedRefreshTokenIsRequired(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	f.set(t, auth.RefreshTokenSecret("gm"), oauthtest.RefreshToken)
	f.oauth.Revoke()
	_, err := f.m.Credentials(t.Context(), "gm")
	requireHint(t, err, "chainmail auth login gm")
	if st, _ := f.m.State("gm"); !st.Required {
		t.Error("State not required after invalid_grant")
	}
}

func TestOAuthStateNeedsBothSecrets(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	st, err := f.m.State("gm")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Required || !strings.Contains(st.Detail, "chainmail auth login gm") {
		t.Errorf("State = %+v", st)
	}
	f.set(t, auth.RefreshTokenSecret("gm"), oauthtest.RefreshToken)
	if st, _ := f.m.State("gm"); st.Required {
		t.Errorf("State = %+v, want ok", st)
	}
}

func TestBadClientSecretIsRequired(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.ClientSecret("g"), "wrong-client-secret")
	f.set(t, auth.RefreshTokenSecret("gm"), oauthtest.RefreshToken)
	_, err := f.m.Credentials(t.Context(), "gm")
	requireHint(t, err, "chainmail secret set oauth_clients/g")
	if st, _ := f.m.State("gm"); !st.Required || !strings.Contains(st.Detail, "oauth_clients/g") {
		t.Errorf("State = %+v, want required with the client secret hint", st)
	}
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	f.m.SecretChanged(auth.ClientSecret("g"))
	if _, err := f.m.Credentials(t.Context(), "gm"); err != nil {
		t.Errorf("Credentials after SecretChanged: %v", err)
	}
}

// racingStore hands out a stale refresh token once, and replaces it with the
// good one while the manager is still using the stale value, as a concurrent
// "chainmail secret set" would.
type racingStore struct {
	*memStore
	m    *auth.Manager
	once sync.Once
}

func (s *racingStore) Get(name string) ([]byte, error) {
	v, err := s.memStore.Get(name)
	if name == auth.RefreshTokenSecret("gm") {
		s.once.Do(func() {
			_ = s.Set(name, []byte(oauthtest.RefreshToken)) // memStore.Set never fails
			s.m.SecretChanged(name)
		})
	}
	return v, err
}

func TestSecretChangedDuringRefreshIsNotLost(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	srv := oauthtest.Start(t)
	store := &racingStore{memStore: &memStore{m: map[string][]byte{
		auth.ClientSecret("g"):        []byte(oauthtest.ClientSecret),
		auth.RefreshTokenSecret("gm"): []byte("stale-refresh-token"),
	}}}
	store.m = auth.NewManager(cfg, store, auth.WithOAuthEndpoint(srv.Endpoint()))
	if _, err := store.m.Credentials(t.Context(), "gm"); err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if st, _ := store.m.State("gm"); st.Required {
		t.Errorf("State = %+v, want ok after the secret was fixed", st)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFinishLoginKeepsNewerLogin(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	srv := oauthtest.Start(t)
	store := &memStore{m: map[string][]byte{auth.ClientSecret("g"): []byte(oauthtest.ClientSecret)}}
	var (
		m      *auth.Manager
		second auth.Challenge
		once   sync.Once
	)
	// The first exchange starts a second login, as a user clicking login
	// twice would, before the exchange completes.
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		once.Do(func() {
			var err error
			if second, err = m.BeginLogin("gm"); err != nil {
				t.Errorf("second BeginLogin: %v", err)
			}
		})
		return http.DefaultTransport.RoundTrip(r)
	})
	m = auth.NewManager(cfg, store, auth.WithOAuthEndpoint(srv.Endpoint()),
		auth.WithHTTPClient(&http.Client{Transport: transport}))
	first, err := m.BeginLogin("gm")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.FinishLogin(t.Context(), "gm", oauthtest.RedirectFor(t, first.URL, oauthtest.Code)); err != nil {
		t.Fatalf("first FinishLogin: %v", err)
	}
	if err := m.FinishLogin(t.Context(), "gm", oauthtest.RedirectFor(t, second.URL, oauthtest.Code)); err != nil {
		t.Errorf("second FinishLogin: %v", err)
	}
}

func TestNilHTTPClientUsesDefault(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	srv := oauthtest.Start(t)
	store := &memStore{m: map[string][]byte{
		auth.ClientSecret("g"):        []byte(oauthtest.ClientSecret),
		auth.RefreshTokenSecret("gm"): []byte(oauthtest.RefreshToken),
	}}
	m := auth.NewManager(cfg, store, auth.WithOAuthEndpoint(srv.Endpoint()), auth.WithHTTPClient(nil))
	if _, err := m.Credentials(t.Context(), "gm"); err != nil {
		t.Errorf("Credentials: %v", err)
	}
}

func TestUnauthorizedClientNeedsLogin(t *testing.T) {
	t.Parallel()
	// Google returns unauthorized_client when the refresh token was issued
	// to another client, which only a new login fixes.
	ep, _ := cannedTokenServer(t, http.StatusBadRequest, `{"error":"unauthorized_client"}`)
	f := newFixtureAt(t, ep)
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	f.set(t, auth.RefreshTokenSecret("gm"), oauthtest.RefreshToken)
	_, err := f.m.Credentials(t.Context(), "gm")
	requireHint(t, err, "chainmail auth login gm")
	if st, _ := f.m.State("gm"); !st.Required || !strings.Contains(st.Detail, "chainmail auth login gm") {
		t.Errorf("State = %+v, want required with the login hint", st)
	}
}
