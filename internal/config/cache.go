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
