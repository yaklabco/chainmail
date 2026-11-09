package auth_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/testutil/oauthtest"
)

// rawResponder answers every request with a status line whose reason
// phrase is reason, which net/http's own server cannot send.
func rawResponder(t *testing.T, w http.ResponseWriter, status int, reason, body string) {
	t.Helper()
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	defer func() { _ = conn.Close() }() // the response is written or the test failed
	_, _ = fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, reason, len(body), body)
	_ = buf.Flush() // a failed write shows up as a client-side error
}

// Token endpoint errors carry the numeric status and an RFC 6749 error code
// only. Error codes outside the RFC and the reason phrase are the server's
// text and stay out, so a server that echoes a secret back cannot get it
// into an error or a log.
func TestTokenErrorsCarryOnlyStatusAndKnownCodes(t *testing.T) {
	t.Parallel()
	const (
		codeLeak   = "code-leak-3c71"
		reasonLeak = "reason-leak-9f02"
	)
	tests := []struct {
		name, body string
		want       []string
	}{
		{name: "unknown code", body: `{"error":"` + codeLeak + `"}`, want: []string{"400"}},
		{name: "known code", body: `{"error":"temporarily_unavailable"}`, want: []string{"400", "temporarily_unavailable"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				rawResponder(t, w, http.StatusBadRequest, reasonLeak, tt.body)
			}))
			t.Cleanup(srv.Close)
			f := newFixtureAt(t, oauth2.Endpoint{AuthURL: srv.URL + "/auth", TokenURL: srv.URL + "/token", AuthStyle: oauth2.AuthStyleInParams})
			f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
			f.set(t, auth.RefreshTokenSecret("gm"), oauthtest.RefreshToken)

			_, refreshErr := f.m.Credentials(t.Context(), "gm")
			if !errors.Is(refreshErr, auth.ErrTokenEndpoint) {
				t.Fatalf("refresh err = %v, want ErrTokenEndpoint", refreshErr)
			}
			ch, err := f.m.BeginLogin(t.Context(), "gm")
			if err != nil {
				t.Fatal(err)
			}
			exchangeErr := f.m.FinishLogin(t.Context(), "gm", oauthtest.RedirectFor(t, ch.URL, oauthtest.Code))
			if exchangeErr == nil {
				t.Fatal("FinishLogin succeeded against a refusing token endpoint")
			}
			for what, err := range map[string]error{"refresh": refreshErr, "exchange": exchangeErr} {
				msg := err.Error()
				requireNoSecrets(t, what+" error", msg, codeLeak, reasonLeak, "Bad Request")
				for _, want := range tt.want {
					if !strings.Contains(msg, want) {
						t.Errorf("%s error = %q, want it to contain %q", what, msg, want)
					}
				}
			}
		})
	}
}

// The hints for the codes chainmail acts on are unchanged by the allowlist.
func TestTokenErrorHintsSurviveTheAllowlist(t *testing.T) {
	t.Parallel()
	for code, hint := range map[string]string{
		"invalid_grant":       "the refresh token was rejected; run: chainmail auth login gm",
		"invalid_client":      "run: chainmail secret set oauth_clients/g",
		"unauthorized_client": "issued to another OAuth client; run: chainmail auth login gm",
	} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			ep, _ := cannedTokenServer(t, http.StatusBadRequest, `{"error":"`+code+`"}`)
			f := newFixtureAt(t, ep)
			f.set(t, auth.ClientSecret("g"), oauthtest.ClientSecret)
			f.set(t, auth.RefreshTokenSecret("gm"), oauthtest.RefreshToken)
			_, err := f.m.Credentials(t.Context(), "gm")
			requireHint(t, err, hint)
		})
	}
}
