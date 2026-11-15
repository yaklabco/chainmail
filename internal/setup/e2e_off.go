//go:build !e2e

package setup

import "github.com/yaklabco/chainmail/internal/config"

// E2EEnabled reports whether this binary was built with the e2e tag, the
// only build that accepts endpoint overrides. It is false here.
func E2EEnabled() bool { return false }

// E2EOverrides reads the end-to-end test endpoints. Without the e2e build
// tag there are none, whatever the environment says.
func E2EOverrides(func(string) string) (imap, smtp *config.EndpointOverride, err error) {
	return nil, nil, nil
}
