package auth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/testutil/oauthtest"
)

// recorder is an HTTP server that records the Authorization header of each
// request.
type recorder struct {
	*httptest.Server

	mu    sync.Mutex
	auths []string
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	r := &recorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.auths = append(r.auths, req.Header.Get("Authorization"))
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auths...)
}

// get fetches url with c, failing the test on error.
func get(t *testing.T, c *http.Client, url string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close() // the status is what matters
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// getErr fetches url with c and returns only the error, closing any body.
func getErr(c *http.Client, url string) error {
	resp, err := c.Get(url)
	if resp != nil {
		_ = resp.Body.Close() // only the error matters
	}
	return err
}

func TestHTTPClientBearerForOAuth2(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	f.set(t, auth.RefreshTokenSecret("gm"), oauthtest.RefreshToken)
	c, err := f.m.HTTPClient("gm")
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	if c.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", c.Timeout)
	}
	rec := newRecorder(t)
	get(t, c, rec.URL+"/calendars")
	get(t, c, rec.URL+"/events")
	want := "Bearer " + oauthtest.AccessToken
	for i, got := range rec.seen() {
		if got != want {
			t.Errorf("request %d: Authorization = %q, want %q", i, got, want)
		}
	}
	// The first request refreshed the access token and the second reused it.
	if n := f.oauth.Refreshes(); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
}

func TestHTTPClientBasicForPassword(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.PasswordSecret("host"), "pw-41c9")
	c, err := f.m.HTTPClient("host")
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	rec := newRecorder(t)
	get(t, c, rec.URL+"/dav/")
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("me@example.com", "pw-41c9")
	if got, want := rec.seen(), req.Header.Get("Authorization"); len(got) != 1 || got[0] != want {
		t.Errorf("Authorization = %q, want [%q]", got, want)
	}
}

// A parked account sends nothing and fails with *auth.RequiredError, so the
// caller can report AUTH_REQUIRED.
func TestHTTPClientParkedAccountIsRequired(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.PasswordSecret("host"), "pw")
	f.m.Rejected("host", f.credentials(t, "host"))
	c, err := f.m.HTTPClient("host")
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	rec := newRecorder(t)
	err = getErr(c, rec.URL+"/")
	requireHint(t, err, "chainmail secret set accounts/host")
	if n := len(rec.seen()); n != 0 {
		t.Errorf("server saw %d requests, want none", n)
	}
}

// A refresh token the endpoint refuses parks the account on the first
// request, as Credentials would.
func TestHTTPClientRevokedTokenIsRequired(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
	f.set(t, auth.RefreshTokenSecret("gm"), oauthtest.RefreshToken)
	f.oauth.Revoke()
	c, err := f.m.HTTPClient("gm")
	if err != nil {
		t.Fatal(err)
	}
	rec := newRecorder(t)
	err = getErr(c, rec.URL+"/")
	requireHint(t, err, "chainmail auth login gm")
	if n := len(rec.seen()); n != 0 {
		t.Errorf("server saw %d requests, want none", n)
	}
}

func TestHTTPClientUnknownAccount(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := f.m.HTTPClient("nope"); !errors.Is(err, config.ErrUnknownAccount) {
		t.Errorf("err = %v, want config.ErrUnknownAccount", err)
	}
}

// Credentials go only over https, or plain http to a loopback host.
func TestHTTPClientRefusesPlainHTTP(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.PasswordSecret("host"), "pw-plain-7d1e")
	c, err := f.m.HTTPClient("host")
	if err != nil {
		t.Fatal(err)
	}
	err = getErr(c, "http://calendar.example.invalid/")
	if !errors.Is(err, auth.ErrPlainHTTP) {
		t.Errorf("err = %v, want auth.ErrPlainHTTP", err)
	}
	if err != nil {
		requireNoSecrets(t, "error", err.Error(), "pw-plain-7d1e")
	}
}

// A redirect to another host is not followed, so the credentials stay with
// the server they were meant for.
func TestHTTPClientRefusesRedirectToAnotherHost(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set(t, auth.PasswordSecret("host"), "pw")
	c, err := f.m.HTTPClient("host")
	if err != nil {
		t.Fatal(err)
	}
	other := newRecorder(t)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/here" {
			http.Redirect(w, r, "/there", http.StatusFound)
			return
		}
		if r.URL.Path == "/there" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(w, r, other.URL+"/stolen", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)
	get(t, c, redirector.URL+"/here") // same host: followed
	err = getErr(c, redirector.URL+"/away")
	if err == nil || !strings.Contains(err.Error(), "redirect to another host") {
		t.Errorf("err = %v, want a refused redirect", err)
	}
	if n := len(other.seen()); n != 0 {
		t.Errorf("other host saw %d requests, want none", n)
	}
}
