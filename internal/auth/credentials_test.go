package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jamesainslie/chainmail/internal/provider"
)

func TestPasswordSASLIsPlain(t *testing.T) {
	t.Parallel()
	c := Credentials{Method: provider.AuthPassword, Username: "me@example.com", Secret: "pw"}
	mech, ir, err := c.SASL().Start()
	if err != nil {
		t.Fatal(err)
	}
	if mech != "PLAIN" || string(ir) != "\x00me@example.com\x00pw" {
		t.Errorf("Start() = %q, %q", mech, ir)
	}
}

func TestOAuth2SASLIsXOAuth2(t *testing.T) {
	t.Parallel()
	c := Credentials{Method: provider.AuthOAuth2, Username: "me@gmail.com", Secret: "tok"}
	client := c.SASL()
	mech, ir, err := client.Start()
	if err != nil {
		t.Fatal(err)
	}
	if mech != XOAuth2 {
		t.Errorf("mech = %q", mech)
	}
	if want := "user=me@gmail.com\x01auth=Bearer tok\x01\x01"; string(ir) != want {
		t.Errorf("initial response = %q, want %q", ir, want)
	}
	resp, err := client.Next([]byte(`{"status":"401"}`))
	if err != nil || resp == nil || len(resp) != 0 {
		t.Errorf("Next() = %q, %v; want an empty non-nil response", resp, err)
	}
}

// renderings formats v every way a log line, error or API response might.
func renderings(t *testing.T, v any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d", "%t", "%q", "%x"} {
		out[verb] = fmt.Sprintf(verb, v)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	out["json"] = string(b)
	var jsonBuf, textBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&jsonBuf, nil)).Info("login", "creds", v)
	slog.New(slog.NewTextHandler(&textBuf, nil)).Info("login", "creds", v)
	out["slog json"] = jsonBuf.String()
	out["slog text"] = textBuf.String()
	return out
}

func assertNoLeak(t *testing.T, name, secret string, v any) {
	t.Helper()
	for how, s := range renderings(t, v) {
		if strings.Contains(s, secret) || strings.Contains(s, fmt.Sprintf("%x", secret)) {
			t.Errorf("%s leaks its secret via %s: %s", name, how, s)
		}
	}
}

func TestCredentialsRedactSecret(t *testing.T) {
	t.Parallel()
	c := Credentials{Method: provider.AuthPassword, Username: "u", Secret: "hunter2"}
	assertNoLeak(t, "Credentials", "hunter2", c)
	assertNoLeak(t, "*Credentials", "hunter2", &c)
	assertNoLeak(t, "PLAIN client", "hunter2", c.SASL())
	if got, want := c.String(), "Credentials{Method: password, Username: u, Secret: [redacted]}"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got := c.GoString(); got != c.String() {
		t.Errorf("GoString() = %q, want the redacted String", got)
	}
}

func TestXOAuth2ClientRedactsToken(t *testing.T) {
	t.Parallel()
	c := Credentials{Method: provider.AuthOAuth2, Username: "me@gmail.com", Secret: "ya29.secret-token"}
	assertNoLeak(t, "Credentials", "ya29.secret-token", c)
	assertNoLeak(t, "XOAUTH2 client from SASL()", "ya29.secret-token", c.SASL())
	assertNoLeak(t, "XOAUTH2 client", "ya29.secret-token", NewXOAuth2Client("me@gmail.com", "ya29.secret-token"))
}

func TestXOAuth2RejectsSeparatorInValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, user, token string }{
		{"username", "evil\x01auth=Bearer x", "tok-secret"},
		{"token", "me@gmail.com", "tok\x01secret"},
	} {
		_, ir, err := NewXOAuth2Client(tc.user, tc.token).Start()
		if err == nil {
			t.Errorf("%s with \\x01: Start() returned %q and no error", tc.name, ir)
			continue
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "evil") {
			t.Errorf("%s: error leaks the value: %v", tc.name, err)
		}
	}
}

func TestRequiredErrorMatchesSentinel(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("wrapped: %w", &RequiredError{Account: "work", Hint: "run: chainmail auth login work"})
	if !errors.Is(err, ErrAuthRequired) {
		t.Error("RequiredError does not match ErrAuthRequired")
	}
	var re *RequiredError
	if !errors.As(err, &re) || re.Hint == "" {
		t.Error("errors.As did not find the hint")
	}
}
