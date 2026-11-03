package config

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// cacheDaysRE matches a whole number of days, such as 90d. Five digits keep
// any match inside a time.Duration; validation bounds the cache keys.
var cacheDaysRE = regexp.MustCompile(`^([0-9]{1,5})d$`)

// ParseCacheDuration parses a [cache] duration: a whole number of days such
// as 90d, or anything time.ParseDuration accepts, such as 250ms or 1h30m.
// The d unit is only for the [cache] keys; every other duration in the
// config is a Go duration.
func ParseCacheDuration(s string) (time.Duration, error) {
	if m := cacheDaysRE.FindStringSubmatch(s); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64) // at most 5 digits always fits
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("duration %q: want a number of days such as 90d, or a Go duration such as 30s, 5m or 168h", s)
	}
	return d, nil
}

// CacheConfig is the [cache] table, and an account's [accounts.<name>.cache]
// table, which may set every key but max_size and dir. An unset key in an
// account table takes the [cache] value.
type CacheConfig struct {
	// Enabled defaults to true. False stops the cache's work, and every
	// search and get uses the server.
	Enabled *bool `toml:"enabled"`
	// PrefetchBodies is the body window by arrival date, such as "90d":
	// 0d to 3650d. "0d" caches bodies only when they are read.
	PrefetchBodies string `toml:"prefetch_bodies"`
	// ReadRetention keeps bodies read within this long outside the window,
	// such as "30d": 0d to 3650d.
	ReadRetention string `toml:"read_retention"`
	// MaxSize bounds the whole cache.db file, such as "2GiB": 64MiB to 1TiB.
	// Only in [cache].
	MaxSize string `toml:"max_size"`
	// DailyDownload is what background cache work may download per account
	// per UTC day, such as "1GiB": 16MiB to 100GiB.
	DailyDownload string `toml:"daily_download"`
	// BackfillBatch is how many headers one backfill batch fetches: 10 to
	// 1000. 0 means not set.
	BackfillBatch int `toml:"backfill_batch"`
	// BackfillPause is the wait between backfill batches, such as "250ms":
	// 0s to 60s.
	BackfillPause string `toml:"backfill_pause"`
	// FreshLag is how far the cache may lag the event log and still count
	// as fresh, such as "30s": 1s to 10m.
	FreshLag string `toml:"fresh_lag"`
	// Dir is the directory of cache.db, an absolute path. Empty means the
	// daemon's state directory. Only in [cache].
	Dir string `toml:"dir"`
}

// CacheSettings are one account's resolved cache values.
type CacheSettings struct {
	Enabled        bool
	PrefetchBodies time.Duration
	ReadRetention  time.Duration
	// DailyDownload is in bytes.
	DailyDownload int64
	BackfillBatch int
	BackfillPause time.Duration
	FreshLag      time.Duration
}

// Cache defaults, as written in config.
const (
	defaultCachePrefetch  = "90d"
	defaultCacheRetention = "30d"
	defaultCacheMaxSize   = "2GiB"
	defaultCacheDaily     = "1GiB"
	defaultCacheBatch     = 250
	defaultCachePause     = "250ms"
	defaultCacheFreshLag  = "30s"
)

// applyDefaults fills the unset [cache] keys. Dir stays empty, which means
// the daemon's state directory.
func (cc *CacheConfig) applyDefaults() {
	if cc.Enabled == nil {
		enabled := true
		cc.Enabled = &enabled
	}
	if cc.PrefetchBodies == "" {
		cc.PrefetchBodies = defaultCachePrefetch
	}
	if cc.ReadRetention == "" {
		cc.ReadRetention = defaultCacheRetention
	}
	if cc.MaxSize == "" {
		cc.MaxSize = defaultCacheMaxSize
	}
	if cc.DailyDownload == "" {
		cc.DailyDownload = defaultCacheDaily
	}
	if cc.BackfillBatch == 0 {
		cc.BackfillBatch = defaultCacheBatch
	}
	if cc.BackfillPause == "" {
		cc.BackfillPause = defaultCachePause
	}
	if cc.FreshLag == "" {
		cc.FreshLag = defaultCacheFreshLag
	}
}

// CacheSettings returns the account's settings: each key from
// [accounts.<name>.cache] when set there, else from [cache], else the
// default. Parse has validated every value. A Config that never went
// through Parse gets the defaults for unset or unparsable values.
func (c *Config) CacheSettings(account string) CacheSettings {
	var own CacheConfig
	if a, ok := c.Accounts[account]; ok && a.Cache != nil {
		own = *a.Cache
	}
	g := c.Cache
	return CacheSettings{
		Enabled:        boolOr(true, own.Enabled, g.Enabled),
		PrefetchBodies: cacheDurationOr(defaultCachePrefetch, own.PrefetchBodies, g.PrefetchBodies),
		ReadRetention:  cacheDurationOr(defaultCacheRetention, own.ReadRetention, g.ReadRetention),
		DailyDownload:  cacheSizeOr(defaultCacheDaily, own.DailyDownload, g.DailyDownload),
		BackfillBatch:  positiveOr(defaultCacheBatch, own.BackfillBatch, g.BackfillBatch),
		BackfillPause:  cacheDurationOr(defaultCachePause, own.BackfillPause, g.BackfillPause),
		FreshLag:       cacheDurationOr(defaultCacheFreshLag, own.FreshLag, g.FreshLag),
	}
}

// CacheMaxSize returns [cache] max_size in bytes.
func (c *Config) CacheMaxSize() int64 {
	return cacheSizeOr(defaultCacheMaxSize, c.Cache.MaxSize)
}

// CacheDir returns [cache] dir, "" when unset, which means the daemon's
// state directory.
func (c *Config) CacheDir() string { return c.Cache.Dir }

// boolOr returns the first of values that is set, else def.
func boolOr(def bool, values ...*bool) bool {
	for _, v := range values {
		if v != nil {
			return *v
		}
	}
	return def
}

// positiveOr returns the first of values above 0, else def.
func positiveOr(def int, values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return def
}

// cacheDurationOr returns the first of values that ParseCacheDuration
// accepts and that is not negative, else def. Parse has validated every
// value, so def only applies to a Config built without it.
func cacheDurationOr(def string, values ...string) time.Duration {
	for _, v := range values {
		if d, err := ParseCacheDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	d, _ := ParseCacheDuration(def) // every default is a valid duration
	return d
}

// cacheSizeOr returns the first of values that ParseByteSize accepts and
// that is above 0, else def.
func cacheSizeOr(def string, values ...string) int64 {
	for _, v := range values {
		if n, err := ParseByteSize(v); err == nil && n > 0 {
			return n
		}
	}
	n, _ := ParseByteSize(def) // every default is a valid size
	return n
}
