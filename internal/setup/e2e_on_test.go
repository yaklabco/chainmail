//go:build e2e

package setup

import (
	"testing"

	"github.com/yaklabco/chainmail/internal/config"
)

func TestE2EOverrides(t *testing.T) {
	t.Parallel()
	if !E2EEnabled() {
		t.Fatal("E2EEnabled() = false under the e2e build tag")
	}

	getenv := func(env map[string]string) func(string) string {
		return func(k string) string { return env[k] }
	}

	imap, smtp, err := E2EOverrides(getenv(map[string]string{
		"CHAINMAIL_E2E_IMAP": "127.0.0.1:3993",
		"CHAINMAIL_E2E_SMTP": "localhost:3465",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if want := (config.EndpointOverride{Host: "127.0.0.1", Port: 3993, Security: "tls"}); imap == nil || *imap != want {
		t.Errorf("imap = %+v, want %+v", imap, want)
	}
	if want := (config.EndpointOverride{Host: "localhost", Port: 3465, Security: "tls"}); smtp == nil || *smtp != want {
		t.Errorf("smtp = %+v, want %+v", smtp, want)
	}

	imap, smtp, err = E2EOverrides(getenv(nil))
	if imap != nil || smtp != nil || err != nil {
		t.Errorf("unset: got %+v, %+v, %v; want nil, nil, nil", imap, smtp, err)
	}

	for _, bad := range []string{"nohost", "host:", ":993", "host:0", "host:70000", "host:abc"} {
		if _, _, err := E2EOverrides(getenv(map[string]string{"CHAINMAIL_E2E_IMAP": bad})); err == nil {
			t.Errorf("CHAINMAIL_E2E_IMAP=%q: no error", bad)
		}
	}
}

func TestBuildWithE2EOverrides(t *testing.T) {
	t.Parallel()
	l := LayoutFor(Linux)
	req := gmailRequest()
	req.Address = "alice@example.com"
	req.Detection = Detection{Provider: "hostinger", Auth: "password"}
	req.IMAP = e2eOverrideForTest()
	req.SMTP = &config.EndpointOverride{Host: "127.0.0.1", Port: 3465, Security: "tls"}
	p := mustBuild(t, l, freshHost(Linux).inspect(t, Linux), req)
	if p.Account.IMAP == nil || p.Account.SMTP == nil {
		t.Fatalf("Account = %+v, want both overrides", p.Account)
	}
	if err := Validate(l, p); err != nil {
		t.Errorf("Validate: %v", err)
	}
}
