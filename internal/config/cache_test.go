package config

import (
	"errors"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
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

// cacheDefaults are the settings every account gets with no cache tables.
var cacheDefaults = CacheSettings{
	Enabled:        true,
	PrefetchBodies: 90 * day,
	ReadRetention:  30 * day,
	DailyDownload:  1 << 30,
	BackfillBatch:  250,
	BackfillPause:  250 * time.Millisecond,
	FreshLag:       30 * time.Second,
}

func TestCacheDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := cfg.Cache
	if got.Enabled == nil || !*got.Enabled {
		t.Errorf("cache.enabled = %v, want true", got.Enabled)
	}
	got.Enabled = nil
	want := CacheConfig{
		PrefetchBodies: "90d", ReadRetention: "30d", MaxSize: "2GiB", DailyDownload: "1GiB",
		BackfillBatch: 250, BackfillPause: "250ms", FreshLag: "30s",
	}
	if got != want {
		t.Errorf("cache = %+v, want %+v", got, want)
	}
	for _, name := range []string{"hostinger", "icloud", "local", "work", "unknown"} {
		if got := cfg.CacheSettings(name); got != cacheDefaults {
			t.Errorf("CacheSettings(%q) = %+v, want %+v", name, got, cacheDefaults)
		}
	}
	for name, a := range cfg.Accounts {
		if a.Cache != nil {
			t.Errorf("%s: cache = %+v, want nil without a cache table", name, *a.Cache)
		}
	}
	if got := cfg.CacheMaxSize(); got != 2<<30 {
		t.Errorf("CacheMaxSize() = %d, want %d", got, int64(2<<30))
	}
	if got := cfg.CacheDir(); got != "" {
		t.Errorf("CacheDir() = %q, want empty", got)
	}
}

// Each key comes from the account's cache table when set there, else from
// [cache], else the default, one key at a time. Every key is overridden by
// some account and inherited by another, and a zero value overrides too.
func TestCacheSettingsPrecedence(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(watchAccount + `
[accounts.host.cache]
enabled = true
read_retention = "7d"
backfill_batch = 50
fresh_lag = "2m"

[accounts.other]
provider = "hostinger"
address = "other@example.com"

[accounts.third]
provider = "hostinger"
address = "third@example.com"

[accounts.third.cache]
prefetch_bodies = "0d"
daily_download = "16MiB"
backfill_pause = "0s"

[cache]
enabled = false
prefetch_bodies = "30d"
read_retention = "60d"
max_size = "8GiB"
daily_download = "512MiB"
backfill_batch = 100
backfill_pause = "1s"
dir = "/srv/chainmail-cache"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tests := map[string]CacheSettings{
		"host": {
			Enabled: true, PrefetchBodies: 30 * day, ReadRetention: 7 * day, DailyDownload: 512 << 20,
			BackfillBatch: 50, BackfillPause: time.Second, FreshLag: 2 * time.Minute,
		},
		"other": {
			Enabled: false, PrefetchBodies: 30 * day, ReadRetention: 60 * day, DailyDownload: 512 << 20,
			BackfillBatch: 100, BackfillPause: time.Second, FreshLag: 30 * time.Second,
		},
		"third": {
			Enabled: false, PrefetchBodies: 0, ReadRetention: 60 * day, DailyDownload: 16 << 20,
			BackfillBatch: 100, BackfillPause: 0, FreshLag: 30 * time.Second,
		},
	}
	for name, want := range tests {
		if got := cfg.CacheSettings(name); got != want {
			t.Errorf("CacheSettings(%q) = %+v, want %+v", name, got, want)
		}
	}
	if got := cfg.CacheMaxSize(); got != 8<<30 {
		t.Errorf("CacheMaxSize() = %d, want %d", got, int64(8<<30))
	}
	if got := cfg.CacheDir(); got != "/srv/chainmail-cache" {
		t.Errorf("CacheDir() = %q, want /srv/chainmail-cache", got)
	}
}

// A Config built without Parse, as tests sometimes do, gets the defaults
// for unset or unparsable values rather than zero.
func TestCacheSettingsUnparsedFallsBackToDefaults(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]*Config{
		"empty": {},
		"unparsable": {
			Cache: CacheConfig{
				PrefetchBodies: "a while", ReadRetention: "-1h", MaxSize: "big", DailyDownload: "lots",
				BackfillBatch: -3, BackfillPause: "x", FreshLag: "y",
			},
			Accounts: map[string]Account{"host": {Provider: "hostinger", Cache: &CacheConfig{FreshLag: "soon", BackfillBatch: -1}}},
		},
	} {
		if got := c.CacheSettings("host"); got != cacheDefaults {
			t.Errorf("%s: CacheSettings() = %+v, want the defaults %+v", name, got, cacheDefaults)
		}
		if got := c.CacheMaxSize(); got != 2<<30 {
			t.Errorf("%s: CacheMaxSize() = %d, want the 2GiB default", name, got)
		}
	}
}

func TestCacheRejectsUnknownKeys(t *testing.T) {
	t.Parallel()
	for name, config := range map[string]string{
		"cache table":   "[cache]\nprefetch = \"30d\"\n",
		"account cache": watchAccount + "[accounts.host.cache]\nwindow = \"30d\"\n",
		"inline cache":  watchAccount + "cache = { prefetch_body = \"30d\" }\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(config))
			var strict *toml.StrictMissingError
			if !errors.As(err, &strict) {
				t.Errorf("err = %v, want *toml.StrictMissingError", err)
			}
		})
	}
}
