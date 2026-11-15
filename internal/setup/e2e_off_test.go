//go:build !e2e

package setup

import "testing"

func TestE2EOffByDefault(t *testing.T) {
	t.Parallel()
	if E2EEnabled() {
		t.Error("E2EEnabled() = true without the e2e build tag")
	}
	env := map[string]string{
		"CHAINMAIL_E2E_IMAP": "127.0.0.1:3993",
		"CHAINMAIL_E2E_SMTP": "127.0.0.1:3465",
	}
	imap, smtp, err := E2EOverrides(func(k string) string { return env[k] })
	if imap != nil || smtp != nil || err != nil {
		t.Errorf("E2EOverrides = %+v, %+v, %v; want nil, nil, nil", imap, smtp, err)
	}
}
