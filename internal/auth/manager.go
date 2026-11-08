package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/vault"
)

// LoginRedirectURL is where the provider sends the browser after consent.
// Nothing listens there: the user copies the URL from the address bar.
const LoginRedirectURL = "http://127.0.0.1:53682/"

const (
	loginTTL     = 10 * time.Minute
	refreshEarly = 5 * time.Minute
	// readAttempts bounds how often Credentials rereads secrets that keep
	// changing under it.
	readAttempts = 3
)

// Login flow errors.
var (
	ErrNotOAuth       = errors.New("account does not use oauth2")
	ErrNoPendingLogin = errors.New("no login in progress for this account")
	ErrLoginExpired   = errors.New("login expired")
	ErrStateMismatch  = errors.New("redirect URL does not match the login in progress")
	ErrConsentDenied  = errors.New("consent was not granted")
	ErrNoRefreshToken = errors.New("provider returned no refresh token")
	// ErrTokenEndpoint means the OAuth2 token endpoint could not be reached
	// or failed in a way that says nothing about the secrets, such as a 5xx
	// or an error code chainmail does not act on.
	ErrTokenEndpoint = errors.New("oauth2 token endpoint unavailable")
)

// PasswordSecret names the vault entry holding an account's password.
func PasswordSecret(account string) string { return "accounts/" + account }

// RefreshTokenSecret names the vault entry holding an account's OAuth2 refresh token.
func RefreshTokenSecret(account string) string { return "tokens/" + account }

// ClientSecret names the vault entry holding an OAuth2 client's secret.
func ClientSecret(client string) string { return "oauth_clients/" + client }

// SecretStore is the part of the vault the manager needs. Get returns an
// error matching vault.ErrNotFound for a missing secret.
type SecretStore interface {
	Get(name string) ([]byte, error)
	Set(name string, value []byte) error
}

// State says whether an account can log in without a human.
type State struct {
	Required bool
	// Detail says why, and what to run, when Required is set.
	Detail string
}

// Challenge is the start of an OAuth2 login. URL carries the login's state
// parameter, which is shown to the user by design but should not be logged.
type Challenge struct {
	URL       string
	ExpiresAt time.Time
}

// pendingLogin holds the PKCE verifier and state of a login in progress.
// Both are secrets until the login finishes, so it never prints them.
type pendingLogin struct {
	state    string
	verifier string
	expires  time.Time
}

func (p pendingLogin) String() string {
	return fmt.Sprintf("login in progress until %s, state %s, verifier %s",
		p.expires.Format(time.RFC3339), redacted, redacted)
}

func (p pendingLogin) Format(f fmt.State, _ rune) { writeFormatted(f, p.String()) }

func (p pendingLogin) LogValue() slog.Value { return slog.StringValue(p.String()) }

// Manager hands out credentials and runs logins for every configured account.
// It holds token sources and logins in progress, so it prints and logs only
// a summary.
type Manager struct {
	cfg        *config.Config
	store      SecretStore
	now        func() time.Time
	httpClient *http.Client
	endpoint   *oauth2.Endpoint

	// tokenClient and discoveryClient are copies of httpClient with the
	// redirect policies of token requests and OIDC discovery.
	tokenClient     *http.Client
	discoveryClient *http.Client

	// discoverMu serializes OIDC discoveries, so concurrent first logins
	// of a client fetch its document once. discovered holds each issuer
	// client's endpoints by client name; a failed discovery is not kept.
	discoverMu sync.Mutex
	discovered map[string]oauth2.Endpoint

	mu       sync.Mutex
	required map[string]string
	pending  map[string]pendingLogin
	sources  map[string]oauth2.TokenSource
	// accountGen counts changes to each account's secrets and auth state.
	// Work that read a secret before its account's generation moved is
	// stale and must not be recorded. It is per account, so a change to one
	// account does not hide a rejection on another. Credentials carry the
	// generation of the secret they hold.
	accountGen map[string]uint64
}

// Option configures a Manager.
type Option func(*Manager)

// WithClock replaces time.Now for login expiry. Access token expiry follows
// the oauth2 package's own clock.
func WithClock(now func() time.Time) Option { return func(m *Manager) { m.now = now } }

// WithHTTPClient sets the client used for token requests and OIDC discovery,
// and whose transport HTTPClient builds on. A nil client keeps the default.
// Its redirect policy is replaced by chainmail's own: token requests follow
// no redirect, and discovery follows one only to the same scheme and host.
func WithHTTPClient(c *http.Client) Option {
	return func(m *Manager) {
		if c != nil {
			m.httpClient = c
		}
	}
}

// WithOAuthEndpoint replaces the OAuth2 endpoint of every preset with its own
// endpoints. Clients with an issuer still use discovery. Tests use it.
func WithOAuthEndpoint(ep oauth2.Endpoint) Option { return func(m *Manager) { m.endpoint = &ep } }

// NewManager returns a Manager for the accounts in cfg.
func NewManager(cfg *config.Config, store SecretStore, opts ...Option) *Manager {
	m := &Manager{
		cfg:        cfg,
		store:      store,
		now:        time.Now,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		required:   map[string]string{},
		pending:    map[string]pendingLogin{},
		sources:    map[string]oauth2.TokenSource{},
		accountGen: map[string]uint64{},
		discovered: map[string]oauth2.Endpoint{},
	}
	for _, opt := range opts {
		opt(m)
	}
	m.tokenClient = withRedirectPolicy(m.httpClient, refuseRedirect)
	m.discoveryClient = withRedirectPolicy(m.httpClient, sameHostRedirect)
	return m
}

// String summarizes m without the tokens and login state it holds. It takes
// m.mu, so formatting or logging m while holding m.mu deadlocks.
func (m *Manager) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fmt.Sprintf("auth.Manager{accounts: %d, needing a human: %d, logins in progress: %d}",
		len(m.cfg.Accounts), len(m.required), len(m.pending))
}

// Format prints the String summary for every verb, including %#v and %+v,
// which would otherwise reflect over the fields.
func (m *Manager) Format(f fmt.State, _ rune) { writeFormatted(f, m.String()) }

// LogValue keeps the manager's tokens out of slog output.
func (m *Manager) LogValue() slog.Value { return slog.StringValue(m.String()) }

// State reports whether account has the secrets it needs and hasn't been rejected.
func (m *Manager) State(account string) (State, error) {
	acct, err := m.cfg.Account(account)
	if err != nil {
		return State{}, err
	}
	if hint, ok := m.requiredHint(account); ok {
		return State{Required: true, Detail: hint}, nil
	}
	for _, name := range neededSecrets(acct) {
		_, err := m.store.Get(name)
		if errors.Is(err, vault.ErrNotFound) {
			return State{Required: true, Detail: hintFor(acct, name)}, nil
		}
		if err != nil {
			return State{}, fmt.Errorf("checking secret %s: %w", name, err)
		}
	}
	return State{}, nil
}

// Credentials returns what a dialer needs to log in to account now. OAuth2
// access tokens are cached and refreshed 5 minutes before they expire. A
// refresh runs to completion even if ctx ends, bounded by the HTTP client's
// timeout, because the oauth2 package keeps the context it was built with.
func (m *Manager) Credentials(ctx context.Context, account string) (Credentials, error) {
	acct, err := m.cfg.Account(account)
	if err != nil {
		return Credentials{}, err
	}
	if hint, ok := m.requiredHint(account); ok {
		return Credentials{}, &RequiredError{Account: account, Hint: hint}
	}
	if acct.Auth == provider.AuthPassword {
		return m.passwordCredentials(acct)
	}
	return m.oauthCredentials(ctx, acct)
}

// passwordCredentials reads account's password like a seqlock: the
// generation before and after the read must match, so the credentials are
// tagged with the generation of the password they carry. If the password
// keeps changing, the last read is tagged with the generation from before
// it, which errs toward Rejected ignoring a refusal rather than parking a
// password nobody tried.
func (m *Manager) passwordCredentials(acct config.ResolvedAccount) (Credentials, error) {
	for attempt := 1; ; attempt++ {
		gen := m.generation(acct.Name)
		pw, err := m.secret(acct, PasswordSecret(acct.Name))
		if err != nil {
			return Credentials{}, err
		}
		if m.generation(acct.Name) == gen || attempt == readAttempts {
			return Credentials{Method: provider.AuthPassword, Username: acct.Username, Secret: string(pw), gen: gen}, nil
		}
	}
}

// oauthCredentials returns a cached or refreshed access token, tagged with
// the generation of the secrets it came from, as passwordCredentials does.
// When the token endpoint refuses a secret that changed while the refresh
// was in flight, it tries again with the new secrets instead of telling the
// human who just fixed them to fix them again.
func (m *Manager) oauthCredentials(ctx context.Context, acct config.ResolvedAccount) (Credentials, error) {
	for attempt := 1; ; attempt++ {
		src, gen, err := m.tokenSource(ctx, acct)
		if err != nil {
			return Credentials{}, err
		}
		tok, err := src.Token()
		if err == nil {
			if m.generation(acct.Name) == gen || attempt == readAttempts {
				return Credentials{Method: provider.AuthOAuth2, Username: acct.Username, Secret: tok.AccessToken, gen: gen}, nil
			}
			continue
		}
		hint, ok := refreshHint(acct, err)
		if !ok {
			// Only the codes refreshHint knows say anything about the
			// secrets. Anything else, a 5xx, an unknown code or no
			// response at all, is the endpoint's trouble.
			return Credentials{}, fmt.Errorf("%w: refreshing access token for %s: %w", ErrTokenEndpoint, acct.Name, tokenError(err))
		}
		if m.markRequiredAt(acct.Name, hint, gen) || attempt == readAttempts {
			return Credentials{}, &RequiredError{Account: acct.Name, Hint: hint}
		}
	}
}

// refreshHint says what a human must do about a refresh the token endpoint
// refused, or false when the failure needs no human.
func refreshHint(acct config.ResolvedAccount, err error) (string, bool) {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return "", false
	}
	switch re.ErrorCode {
	case "invalid_grant":
		return "the refresh token was rejected; run: chainmail auth login " + acct.Name, true
	case "invalid_client":
		return clientRejectedHint(acct), true
	case "unauthorized_client":
		// Google sends this when the refresh token belongs to another
		// client, such as after client_id changed. Only a new login fixes it.
		return "the refresh token was issued to another OAuth client; run: chainmail auth login " + acct.Name, true
	default:
		return "", false
	}
}

// clientRejectedHint says what to do about an OAuth client the token
// endpoint refused. An issuer client may be public, with no secret to set.
func clientRejectedHint(acct config.ResolvedAccount) string {
	if acct.OAuthIssuer != "" {
		return "the OAuth client was rejected; check client_id in oauth_clients." + acct.OAuthClient +
			", and if the client has a secret, run: chainmail secret set " + ClientSecret(acct.OAuthClient)
	}
	return "the OAuth client was rejected; run: chainmail secret set " + ClientSecret(acct.OAuthClient)
}

// exchangeRefused reports whether the token endpoint refused an
// authorization code exchange, as opposed to failing to answer it: a 4xx
// with an OAuth2 error code, such as invalid_grant for a used code.
func exchangeRefused(err error) bool {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) || re.ErrorCode == "" {
		return false
	}
	return re.Response == nil || re.Response.StatusCode < http.StatusInternalServerError
}

// exchangeHint says what a human must do about a refused code exchange, and
// whether the login in progress is over. invalid_client leaves the code
// unused, so after the client secret is fixed the same URL still works.
// Every other refusal, such as invalid_grant for a used or expired code,
// needs a new login.
func exchangeHint(acct config.ResolvedAccount, err error) (hint string, definitive bool) {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.ErrorCode == "invalid_client" {
		return clientRejectedHint(acct), false
	}
	return "run: chainmail auth login " + acct.Name + " again", true
}

// Rejected records that a server refused creds, which Credentials issued
// for account. chainmail stops using them until a human changes a secret or
// logs in again. If account's secrets or auth state changed since creds were
// issued, the refusal is of credentials already replaced, and Rejected does
// nothing, so the new ones get their chance.
func (m *Manager) Rejected(account string, creds Credentials) {
	acct, err := m.cfg.Account(account)
	if err != nil {
		return
	}
	hint := "the server rejected the password; run: chainmail secret set " + PasswordSecret(account)
	if acct.Auth == provider.AuthOAuth2 {
		hint = "the server rejected the access token; run: chainmail auth login " + account
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.accountGen[account] != creds.gen {
		return
	}
	m.markRequiredLocked(account, hint)
}

// SecretChanged clears the auth state of every account that uses the secret called name.
func (m *Manager) SecretChanged(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, account := range m.cfg.AccountNames() {
		acct, err := m.cfg.Account(account)
		if err != nil {
			continue
		}
		if slices.Contains(usedSecrets(acct), name) {
			delete(m.required, account)
			delete(m.sources, account)
			m.accountGen[account]++
		}
	}
}

// BeginLogin starts an OAuth2 consent flow for account. ctx bounds the OIDC
// discovery of an issuer client's endpoints, which runs once per client.
func (m *Manager) BeginLogin(ctx context.Context, account string) (Challenge, error) {
	acct, err := m.cfg.Account(account)
	if err != nil {
		return Challenge{}, err
	}
	conf, err := m.oauthConfig(ctx, acct)
	if err != nil {
		return Challenge{}, err
	}
	p := pendingLogin{
		state:    rand.Text(),
		verifier: oauth2.GenerateVerifier(),
		expires:  m.now().Add(loginTTL),
	}
	consent := conf.AuthCodeURL(p.state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
		oauth2.S256ChallengeOption(p.verifier),
		oauth2.SetAuthURLParam("login_hint", acct.Address))
	m.mu.Lock()
	m.pending[account] = p
	m.mu.Unlock()
	return Challenge{URL: consent, ExpiresAt: p.expires}, nil
}

// FinishLogin completes the login for account with the URL the browser landed on.
func (m *Manager) FinishLogin(ctx context.Context, account, redirectURL string) error {
	acct, err := m.cfg.Account(account)
	if err != nil {
		return err
	}
	conf, err := m.oauthConfig(ctx, acct)
	if err != nil {
		return err
	}
	// The URL carries the authorization code, and url.Parse errors quote
	// their input, so the parse error stays out of the result.
	u, err := url.Parse(strings.TrimSpace(redirectURL))
	if err != nil {
		return fmt.Errorf("%w: the redirect URL is not a valid URL", ErrStateMismatch)
	}
	q := u.Query()
	m.mu.Lock()
	p, ok := m.pending[account]
	m.mu.Unlock()
	switch {
	case !ok:
		return fmt.Errorf("%w: %s", ErrNoPendingLogin, account)
	case m.now().After(p.expires):
		m.dropPending(account, p)
		return fmt.Errorf("%w: start again with chainmail auth login %s", ErrLoginExpired, account)
	case q.Get("error") != "":
		m.dropPending(account, p)
		// The error code comes from whoever built the URL, so it is quoted
		// and cut short.
		return fmt.Errorf("%w: %.64q", ErrConsentDenied, q.Get("error"))
	case subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(p.state)) != 1:
		return ErrStateMismatch
	case q.Get("code") == "":
		return fmt.Errorf("%w: the URL has no code", ErrStateMismatch)
	}
	tok, err := conf.Exchange(m.withHTTPClient(ctx), q.Get("code"), oauth2.VerifierOption(p.verifier))
	if err != nil {
		if exchangeRefused(err) {
			hint, definitive := exchangeHint(acct, err)
			if definitive {
				// The code is spent or the login cannot complete, so a
				// second paste of the same URL could only fail again.
				m.dropPending(account, p)
			}
			return &RejectedError{Hint: hint, Err: fmt.Errorf("token exchange failed: %w", tokenError(err))}
		}
		return fmt.Errorf("%w: token exchange for %s: %w", ErrTokenEndpoint, account, tokenError(err))
	}
	if tok.RefreshToken == "" {
		if acct.OAuthIssuer != "" {
			return fmt.Errorf("%w: give oauth_clients.%s offline access, such as the offline_access scope, and log in again", ErrNoRefreshToken, acct.OAuthClient)
		}
		return fmt.Errorf("%w: remove chainmail's access at https://myaccount.google.com/permissions and log in again", ErrNoRefreshToken)
	}
	if err := m.store.Set(RefreshTokenSecret(account), []byte(tok.RefreshToken)); err != nil {
		return fmt.Errorf("storing refresh token: %w", err)
	}
	src := oauth2.ReuseTokenSourceWithExpiry(tok, conf.TokenSource(m.refreshContext(ctx), tok), refreshEarly)
	m.mu.Lock()
	m.dropPendingLocked(account, p)
	delete(m.required, account)
	m.sources[account] = src
	m.accountGen[account]++
	m.mu.Unlock()
	return nil
}

func (m *Manager) requiredHint(account string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	hint, ok := m.required[account]
	return hint, ok
}

// markRequiredAt marks account only if its generation is still gen, so a
// refusal of a secret that has since been replaced is not recorded.
func (m *Manager) markRequiredAt(account, hint string, gen uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.accountGen[account] != gen {
		return false
	}
	m.markRequiredLocked(account, hint)
	return true
}

func (m *Manager) markRequiredLocked(account, hint string) {
	m.required[account] = hint
	delete(m.sources, account)
	m.accountGen[account]++
}

// generation returns account's generation, which Credentials carry.
func (m *Manager) generation(account string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.accountGen[account]
}

// dropPending ends the login p if it is still the one in progress, so a
// newer BeginLogin survives an older login finishing or failing.
func (m *Manager) dropPending(account string, p pendingLogin) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropPendingLocked(account, p)
}

func (m *Manager) dropPendingLocked(account string, p pendingLogin) {
	if cur, ok := m.pending[account]; ok && cur.state == p.state {
		delete(m.pending, account)
	}
}

func (m *Manager) secret(acct config.ResolvedAccount, name string) ([]byte, error) {
	v, err := m.store.Get(name)
	if errors.Is(err, vault.ErrNotFound) {
		return nil, &RequiredError{Account: acct.Name, Hint: hintFor(acct, name)}
	}
	if err != nil {
		return nil, fmt.Errorf("reading secret %s: %w", name, err)
	}
	return v, nil
}

// tokenSource returns account's cached token source, or builds one from the
// vault. It also returns the account generation the source is current for,
// read before the secrets, which markRequiredAt checks before blaming the
// secrets and oauthCredentials compares again after the refresh.
func (m *Manager) tokenSource(ctx context.Context, acct config.ResolvedAccount) (oauth2.TokenSource, uint64, error) {
	m.mu.Lock()
	src, ok := m.sources[acct.Name]
	gen := m.accountGen[acct.Name]
	m.mu.Unlock()
	if ok {
		return src, gen, nil
	}
	conf, err := m.oauthConfig(ctx, acct)
	if err != nil {
		return nil, 0, err
	}
	rt, err := m.secret(acct, RefreshTokenSecret(acct.Name))
	if err != nil {
		return nil, 0, err
	}
	src = oauth2.ReuseTokenSourceWithExpiry(nil,
		conf.TokenSource(m.refreshContext(ctx), &oauth2.Token{RefreshToken: string(rt)}), refreshEarly)
	m.mu.Lock()
	defer m.mu.Unlock()
	// A concurrent caller may have built one meanwhile. Sharing it keeps
	// the refreshes to one, since a source serializes its own.
	if existing, ok := m.sources[acct.Name]; ok {
		return existing, m.accountGen[acct.Name], nil
	}
	// A source built from secrets that changed while they were read is
	// used once but not cached.
	if m.accountGen[acct.Name] == gen {
		m.sources[acct.Name] = src
	}
	return src, gen, nil
}

// oauthConfig builds acct's OAuth2 client: the preset's endpoints, or for
// an issuer client those its discovery gives, and the account's scopes. An
// issuer client's secret is optional: without one it is a public client and
// no client_secret is sent, since AuthStyleInParams leaves an empty one out.
func (m *Manager) oauthConfig(ctx context.Context, acct config.ResolvedAccount) (*oauth2.Config, error) {
	if acct.Auth != provider.AuthOAuth2 || (acct.Provider.OAuth2 == nil && acct.OAuthIssuer == "") {
		return nil, fmt.Errorf("%w: %s", ErrNotOAuth, acct.Name)
	}
	secret, err := m.clientSecret(acct)
	if err != nil {
		return nil, err
	}
	var ep oauth2.Endpoint
	switch {
	case acct.OAuthIssuer != "":
		if ep, err = m.issuerEndpoint(ctx, acct); err != nil {
			return nil, err
		}
	case m.endpoint != nil:
		ep = *m.endpoint
	default:
		ep = oauth2.Endpoint{
			AuthURL:   acct.Provider.OAuth2.AuthURL,
			TokenURL:  acct.Provider.OAuth2.TokenURL,
			AuthStyle: oauth2.AuthStyleInParams,
		}
	}
	return &oauth2.Config{
		ClientID:     acct.ClientID,
		ClientSecret: string(secret),
		Endpoint:     ep,
		RedirectURL:  LoginRedirectURL,
		Scopes:       acct.OAuthScopes,
	}, nil
}

// clientSecret reads acct's OAuth client secret. A missing secret is a
// RequiredError, except for an issuer client, which may be public.
func (m *Manager) clientSecret(acct config.ResolvedAccount) ([]byte, error) {
	if acct.OAuthIssuer == "" {
		return m.secret(acct, ClientSecret(acct.OAuthClient))
	}
	v, err := m.store.Get(ClientSecret(acct.OAuthClient))
	switch {
	case errors.Is(err, vault.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("reading secret %s: %w", ClientSecret(acct.OAuthClient), err)
	}
	return v, nil
}

// withHTTPClient carries the HTTP client into a one-off oauth2 call.
func (m *Manager) withHTTPClient(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, m.tokenClient)
}

// refreshContext carries the HTTP client into a token source. Token sources
// keep the context for later refreshes, so it must outlive the request that
// made it.
func (m *Manager) refreshContext(ctx context.Context) context.Context {
	return m.withHTTPClient(context.WithoutCancel(ctx))
}

// oauthErrorCodes are the error codes RFC 6749 section 5.2 defines for a
// token endpoint, plus access_denied, server_error and
// temporarily_unavailable from section 4.1.2.1, which some servers send
// there too. Only these are named in an error.
var oauthErrorCodes = map[string]bool{
	"invalid_request":         true,
	"invalid_client":          true,
	"invalid_grant":           true,
	"unauthorized_client":     true,
	"unsupported_grant_type":  true,
	"invalid_scope":           true,
	"access_denied":           true,
	"server_error":            true,
	"temporarily_unavailable": true,
}

// tokenError describes a token endpoint failure by numeric status and RFC
// 6749 error code only. Everything else in the response is the server's
// text, which could echo a secret back: oauth2.RetrieveError's own message
// quotes the body, the status line carries a reason phrase of the server's
// choosing, and an unknown error code is arbitrary text. None of it reaches
// an error or a log.
func tokenError(err error) error {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return err
	}
	status := "no response"
	if re.Response != nil {
		status = "HTTP " + strconv.Itoa(re.Response.StatusCode)
	}
	switch {
	case re.ErrorCode == "":
		return fmt.Errorf("token endpoint returned %s", status)
	case oauthErrorCodes[re.ErrorCode]:
		return fmt.Errorf("token endpoint returned %s: %s", status, re.ErrorCode)
	default:
		return fmt.Errorf("token endpoint returned %s with an error code outside RFC 6749", status)
	}
}

// neededSecrets lists the secrets acct cannot log in without. An issuer
// client's secret is optional, so it is not among them.
func neededSecrets(acct config.ResolvedAccount) []string {
	switch {
	case acct.Auth == provider.AuthOAuth2 && acct.OAuthIssuer != "":
		return []string{RefreshTokenSecret(acct.Name)}
	case acct.Auth == provider.AuthOAuth2:
		return []string{ClientSecret(acct.OAuthClient), RefreshTokenSecret(acct.Name)}
	default:
		return []string{PasswordSecret(acct.Name)}
	}
}

// usedSecrets lists every secret acct logs in with when it is set: the
// needed ones and an issuer client's optional secret.
func usedSecrets(acct config.ResolvedAccount) []string {
	if acct.Auth == provider.AuthOAuth2 {
		return []string{ClientSecret(acct.OAuthClient), RefreshTokenSecret(acct.Name)}
	}
	return neededSecrets(acct)
}

func hintFor(acct config.ResolvedAccount, secret string) string {
	if secret == RefreshTokenSecret(acct.Name) {
		return "run: chainmail auth login " + acct.Name
	}
	return "run: chainmail secret set " + secret
}
