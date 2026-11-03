package config

import (
	"testing"
	"time"
)

// day is one cache day, as ParseCacheDuration reads "1d".
const day = 24 * time.Hour

func TestParseCacheDuration(t *testing.T) {
	t.Parallel()
	good := map[string]time.Duration{
		"0d": 0, "1d": day, "90d": 90 * day, "3650d": 3650 * day, "99999d": 99999 * day,
		"0s": 0, "250ms": 250 * time.Millisecond, "30s": 30 * time.Second, "1h30m": 90 * time.Minute,
		// A negative Go duration parses; validation refuses it by range.
		"-1s": -time.Second,
	}
	for in, want := range good {
		if got, err := ParseCacheDuration(in); err != nil || got != want {
			t.Errorf("ParseCacheDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	const hint = "want a number of days such as 90d, or a Go duration such as 30s, 5m or 168h"
	for _, bad := range []string{"", "d", "1.5d", "-1d", "1D", "1d12h", "100000d", " 1d", "1 d", "a week"} {
		_, err := ParseCacheDuration(bad)
		if err == nil {
			t.Errorf("ParseCacheDuration(%q) succeeded", bad)
			continue
		}
		if want := `duration "` + bad + `": ` + hint; err.Error() != want {
			t.Errorf("ParseCacheDuration(%q) error = %q, want %q", bad, err, want)
		}
	}
}
