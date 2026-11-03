package config

import (
	"errors"
	"slices"
	"strings"
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

// cacheBound is one value of one key, and whether Parse accepts it.
type cacheBound struct {
	key, value string
	ok         bool
}

// cacheSharedBounds are the values just inside and just outside each range
// of the keys [cache] and account cache tables share.
var cacheSharedBounds = []cacheBound{
	{"prefetch_bodies", `"0d"`, true},
	{"prefetch_bodies", `"3650d"`, true},
	{"prefetch_bodies", `"87600h"`, true},
	{"prefetch_bodies", `"3651d"`, false},
	{"prefetch_bodies", `"87600h1s"`, false},
	{"prefetch_bodies", `"-1s"`, false},
	{"read_retention", `"0d"`, true},
	{"read_retention", `"3650d"`, true},
	{"read_retention", `"3651d"`, false},
	{"read_retention", `"-1s"`, false},
	{"daily_download", `"16MiB"`, true},
	{"daily_download", `"100GiB"`, true},
	{"daily_download", `"16383KiB"`, false},
	{"daily_download", `"102401MiB"`, false},
	{"backfill_batch", "10", true},
	{"backfill_batch", "1000", true},
	{"backfill_batch", "9", false},
	{"backfill_batch", "1001", false},
	{"backfill_batch", "-5", false},
	{"backfill_pause", `"0s"`, true},
	{"backfill_pause", `"60s"`, true},
	{"backfill_pause", `"1m"`, true},
	{"backfill_pause", `"60001ms"`, false},
	{"backfill_pause", `"-1ms"`, false},
	{"fresh_lag", `"1s"`, true},
	{"fresh_lag", `"10m"`, true},
	{"fresh_lag", `"999ms"`, false},
	{"fresh_lag", `"10m1s"`, false},
}

// cacheGlobalBounds are the bounds of the keys only [cache] has.
var cacheGlobalBounds = []cacheBound{
	{"max_size", `"64MiB"`, true},
	{"max_size", `"1TiB"`, true},
	{"max_size", `"65535KiB"`, false},
	{"max_size", `"1025GiB"`, false},
	{"dir", `"/var/cache/chainmail"`, true},
	{"dir", `"cache"`, false},
}

func TestCacheBounds(t *testing.T) {
	t.Parallel()
	for _, b := range slices.Concat(cacheSharedBounds, cacheGlobalBounds) {
		checkCacheBound(t, "[cache]\n"+b.key+" = "+b.value+"\n", "cache."+b.key, b.ok)
	}
	for _, b := range cacheSharedBounds {
		checkCacheBound(t, watchAccount+"[accounts.host.cache]\n"+b.key+" = "+b.value+"\n", "accounts.host.cache."+b.key, b.ok)
	}
}

// checkCacheBound parses config and checks that it is valid when ok, else
// that it has a problem naming key.
func checkCacheBound(t *testing.T, config, key string, ok bool) {
	t.Helper()
	_, err := Parse([]byte(config))
	if ok {
		if err != nil {
			t.Errorf("%q: err = %v, want valid", config, err)
		}
		return
	}
	var ve *ValidationError
	if !errors.As(err, &ve) || !slices.ContainsFunc(ve.Problems, func(p string) bool { return strings.HasPrefix(p, key+": ") }) {
		t.Errorf("%q: err = %v, want a problem starting %q", config, err, key+": ")
	}
}

func TestCacheInvalid(t *testing.T) {
	t.Parallel()
	const durationHint = "want a number of days such as 90d, or a Go duration such as 30s, 5m or 168h"
	const sizeHint = "want a number of bytes, optionally followed by B, KiB, MiB, GiB or TiB"
	const account = watchAccount + "[accounts.host.cache]\n"
	tests := []struct {
		name    string
		config  string
		problem string
	}{
		{name: "prefetch too long", config: "[cache]\nprefetch_bodies = \"4000d\"\n", problem: "cache.prefetch_bodies: 4000d must be between 0d and 3650d"},
		{name: "retention negative", config: "[cache]\nread_retention = \"-1h\"\n", problem: "cache.read_retention: -1h must be between 0d and 3650d"},
		{name: "prefetch not a duration", config: "[cache]\nprefetch_bodies = \"3 months\"\n", problem: `cache.prefetch_bodies: duration "3 months": ` + durationHint},
		{name: "max_size too small", config: "[cache]\nmax_size = \"32MiB\"\n", problem: "cache.max_size: 32MiB must be between 64MiB and 1TiB"},
		{name: "max_size too large", config: "[cache]\nmax_size = \"2TiB\"\n", problem: "cache.max_size: 2TiB must be between 64MiB and 1TiB"},
		{name: "max_size not a size", config: "[cache]\nmax_size = \"2GB\"\n", problem: `cache.max_size: size "2GB": ` + sizeHint},
		{name: "daily_download too small", config: "[cache]\ndaily_download = \"1MiB\"\n", problem: "cache.daily_download: 1MiB must be between 16MiB and 100GiB"},
		{name: "daily_download too large", config: "[cache]\ndaily_download = \"101GiB\"\n", problem: "cache.daily_download: 101GiB must be between 16MiB and 100GiB"},
		{name: "backfill_batch too small", config: "[cache]\nbackfill_batch = 5\n", problem: "cache.backfill_batch: 5 must be between 10 and 1000"},
		{name: "backfill_pause too long", config: "[cache]\nbackfill_pause = \"2m\"\n", problem: "cache.backfill_pause: 2m must be between 0s and 60s"},
		{name: "fresh_lag too short", config: "[cache]\nfresh_lag = \"500ms\"\n", problem: "cache.fresh_lag: 500ms must be between 1s and 10m"},
		{name: "fresh_lag not a duration", config: "[cache]\nfresh_lag = \"soon\"\n", problem: `cache.fresh_lag: duration "soon": ` + durationHint},
		{name: "relative dir", config: "[cache]\ndir = \"var/cache\"\n", problem: "cache.dir: var/cache must be an absolute path"},
		{name: "account backfill_batch", config: account + "backfill_batch = 5\n", problem: "accounts.host.cache.backfill_batch: 5 must be between 10 and 1000"},
		{name: "account prefetch", config: account + "prefetch_bodies = \"4000d\"\n", problem: "accounts.host.cache.prefetch_bodies: 4000d must be between 0d and 3650d"},
		{name: "account daily_download", config: account + "daily_download = \"lots\"\n", problem: `accounts.host.cache.daily_download: size "lots": ` + sizeHint},
		{name: "account max_size", config: account + "max_size = \"4GiB\"\n", problem: "accounts.host.cache: max_size is global; set it in [cache]"},
		{name: "account dir", config: account + "dir = \"/srv/cache\"\n", problem: "accounts.host.cache: dir is global; set it in [cache]"},
		{name: "gmail account", config: gmailAccount + "[accounts.g.cache]\nfresh_lag = \"11m\"\n", problem: "accounts.g.cache.fresh_lag: 11m must be between 1s and 10m"},
		{name: "unknown provider still checks its cache", config: "[accounts.a]\nprovider = \"aol\"\naddress = \"a@b.c\"\n[accounts.a.cache]\ndir = \"/x\"\n", problem: "accounts.a.cache: dir is global; set it in [cache]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tt.config))
			var ve *ValidationError
			if !errors.As(err, &ve) || !slices.Contains(ve.Problems, tt.problem) {
				t.Errorf("err = %v, want a problem %q", err, tt.problem)
			}
		})
	}
}

// An account cache table that sets max_size and dir reports both, once each.
func TestCacheAccountGlobalKeysReportedOnce(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(watchAccount + "[accounts.host.cache]\nmax_size = \"4GiB\"\ndir = \"/srv/cache\"\n"))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	want := []string{
		"accounts.host.cache: max_size is global; set it in [cache]",
		"accounts.host.cache: dir is global; set it in [cache]",
	}
	if !slices.Equal(ve.Problems, want) {
		t.Errorf("problems = %q, want %q", ve.Problems, want)
	}
}

// SyncAuto marks the accounts whose supervisor syncs every folder a default
// search covers: presets other than Google with sync.folders absent,
// whatever watch and sync.enabled say. An explicit list, even an empty one,
// turns it off. SyncFolders stays the watch list either way.
func TestAccountSyncAuto(t *testing.T) {
	t.Parallel()
	const workspace = "[oauth_clients.ws]\nclient_id = \"id\"\n[accounts.w]\nprovider = \"google-workspace\"\naddress = \"me@ainslies.us\"\noauth_client = \"ws\"\n"
	tests := []struct {
		name        string
		config      string
		account     string
		want        bool
		syncFolders []string
	}{
		{name: "default", config: watchAccount, account: "host", want: true, syncFolders: []string{"INBOX"}},
		{name: "empty watch", config: watchAccount + "watch = []\n", account: "host", want: true, syncFolders: []string{}},
		{name: "two watched folders", config: watchAccount + "watch = [\"INBOX\", \"Lists\"]\n", account: "host", want: true, syncFolders: []string{"INBOX", "Lists"}},
		{name: "sync disabled", config: watchAccount + "sync = { enabled = false }\n", account: "host", want: true, syncFolders: []string{"INBOX"}},
		{name: "explicit sync folders", config: watchAccount + "sync = { folders = [\"Sent\"] }\n", account: "host", want: false, syncFolders: []string{"INBOX", "Sent"}},
		{name: "explicit empty sync folders", config: watchAccount + "sync = { folders = [] }\n", account: "host", want: false, syncFolders: []string{"INBOX"}},
		{name: "gmail", config: gmailAccount, account: "g", want: false},
		{name: "google workspace", config: workspace, account: "w", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Parse([]byte(tt.config))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			got, err := cfg.Account(tt.account)
			if err != nil {
				t.Fatal(err)
			}
			if got.SyncAuto != tt.want {
				t.Errorf("SyncAuto = %v, want %v", got.SyncAuto, tt.want)
			}
			if !slices.Equal(got.SyncFolders, tt.syncFolders) {
				t.Errorf("SyncFolders = %q, want %q", got.SyncFolders, tt.syncFolders)
			}
		})
	}
}

// A Config built without Parse resolves SyncAuto the same way.
func TestAccountSyncAutoUnparsed(t *testing.T) {
	t.Parallel()
	cfg := &Config{Accounts: map[string]Account{
		"host": {Provider: "hostinger", Address: "me@example.com"},
		"sent": {Provider: "hostinger", Address: "me@example.com", Sync: AccountSync{Folders: []string{"Sent"}}},
		"g":    {Provider: "gmail", Address: "me@gmail.com"},
	}}
	for name, want := range map[string]bool{"host": true, "sent": false, "g": false} {
		got, err := cfg.Account(name)
		if err != nil {
			t.Fatal(err)
		}
		if got.SyncAuto != want {
			t.Errorf("%s: SyncAuto = %v, want %v", name, got.SyncAuto, want)
		}
	}
}
