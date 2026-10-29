package auth

import (
	"errors"
	"fmt"
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

func TestCredentialsRedactSecret(t *testing.T) {
	t.Parallel()
	c := Credentials{Method: provider.AuthPassword, Username: "u", Secret: "hunter2"}
	for _, s := range []string{c.String(), fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c)} {
		if strings.Contains(s, "hunter2") {
			t.Errorf("formatted credentials leak the secret: %s", s)
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
