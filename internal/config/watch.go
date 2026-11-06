package config

import (
	"fmt"
	"slices"
	"time"

	"github.com/yaklabco/chainmail/internal/provider"
)

// WatchConfig is the [watch] table.
type WatchConfig struct {
	// Retention is how long events and idle groups are kept, such as
	// "168h": 1h to 2160h.
	Retention string `toml:"retention"`
	// MaxEvents is the most events the log keeps: 1000 to 10000000.
	MaxEvents int `toml:"max_events"`
	// ExpungeSweep is how often CONDSTORE folders are swept for expunged
	// mail: 5m to 24h.
	ExpungeSweep string `toml:"expunge_sweep"`
	// IdleFallbackPoll is how often watched folders are polled on servers
	// without IDLE: 10s to 10m.
	IdleFallbackPoll string `toml:"idle_fallback_poll"`
	// GoogleCheck is how often a Google preset account's All Mail is synced
	// while it idles, since Gmail can tell an IDLE about a change 30 seconds
	// or more after it: 2s to 10m.
	GoogleCheck string `toml:"google_check"`
}

// AccountSync is an account's sync table.
type AccountSync struct {
	// Enabled defaults to true. False turns off watching and syncing.
	Enabled *bool `toml:"enabled"`
	// Folders lists the synced folders besides the watch list. Nil means,
	// on accounts other than Google, every folder a default search covers
	// (ResolvedAccount.SyncAuto); an empty list syncs the watch list alone.
	Folders []string `toml:"folders"`
	// Poll is how often synced folders that are not watched are checked:
	// 30s to 24h.
	Poll string `toml:"poll"`
}

// WatchSettings are the resolved [watch] values.
type WatchSettings struct {
	Retention        time.Duration
	MaxEvents        int
	ExpungeSweep     time.Duration
	IdleFallbackPoll time.Duration
	GoogleCheck      time.Duration
}

// Watch and sync defaults, as written in config.
const (
	defaultRetention        = "168h"
	defaultMaxEvents        = 200000
	defaultExpungeSweep     = "1h"
	defaultIdleFallbackPoll = "30s"
	defaultGoogleCheck      = "5s"
	defaultSyncPoll         = "5m"
	// defaultWatchFolder is the folder an account watches when it sets no
	// watch list.
	defaultWatchFolder = "INBOX"
)

// WatchSettings returns the [watch] values as durations. Parse has validated
// them. A Config that never went through Parse gets the defaults for unset
// or unparsable values.
func (c *Config) WatchSettings() WatchSettings {
	maxEvents := c.Watch.MaxEvents
	if maxEvents <= 0 {
		maxEvents = defaultMaxEvents
	}
	return WatchSettings{
		Retention:        durationOr(c.Watch.Retention, defaultRetention),
		MaxEvents:        maxEvents,
		ExpungeSweep:     durationOr(c.Watch.ExpungeSweep, defaultExpungeSweep),
		IdleFallbackPoll: durationOr(c.Watch.IdleFallbackPoll, defaultIdleFallbackPoll),
		GoogleCheck:      durationOr(c.Watch.GoogleCheck, defaultGoogleCheck),
	}
}

// applyDefaults fills the unset [watch] keys.
func (w *WatchConfig) applyDefaults() {
	if w.Retention == "" {
		w.Retention = defaultRetention
	}
	if w.MaxEvents == 0 {
		w.MaxEvents = defaultMaxEvents
	}
	if w.ExpungeSweep == "" {
		w.ExpungeSweep = defaultExpungeSweep
	}
	if w.IdleFallbackPoll == "" {
		w.IdleFallbackPoll = defaultIdleFallbackPoll
	}
	if w.GoogleCheck == "" {
		w.GoogleCheck = defaultGoogleCheck
	}
}

// applyWatchDefaults fills an account's unset watch list and sync table.
// known reports whether p is the account's preset. Gmail presets keep a nil
// watch list, since they watch their All Mail folder.
func (a *Account) applyWatchDefaults(p provider.Preset, known bool) {
	if known && !p.Gmail && a.Watch == nil {
		a.Watch = &[]string{defaultWatchFolder}
	}
	if a.Sync.Poll == "" {
		a.Sync.Poll = defaultSyncPoll
	}
	if a.Sync.Enabled == nil {
		enabled := true
		a.Sync.Enabled = &enabled
	}
}

// durationOr returns s as a duration, or def when s is not a positive Go
// duration. Parse has validated every duration, so def only applies to a
// Config built without it.
func durationOr(s, def string) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	d, _ := time.ParseDuration(def) // every default is a valid duration
	return d
}

// WatchCount is how many watcher connections the account holds open: none
// when syncing is off, one for a Gmail preset's All Mail folder, else one
// per watched folder.
func (a ResolvedAccount) WatchCount() int {
	switch {
	case !a.SyncEnabled:
		return 0
	case a.Provider.Gmail:
		return 1
	default:
		return len(a.Watch)
	}
}

// watchList returns a copy of an account's watch list, or the default
// ["INBOX"] for a Config that never went through Parse.
func watchList(w *[]string) []string {
	if w == nil {
		return []string{defaultWatchFolder}
	}
	return slices.Clone(*w)
}

// syncFolders lists watch, then the folders in folders that watch does not
// already hold, in order and without duplicates.
func syncFolders(watch, folders []string) []string {
	out := make([]string, 0, len(watch)+len(folders))
	for _, f := range slices.Concat(watch, folders) {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// Bounds of the watch and sync keys.
const (
	minRetention        = time.Hour
	maxRetention        = 2160 * time.Hour
	minMaxEvents        = 1000
	maxMaxEvents        = 10000000
	minExpungeSweep     = 5 * time.Minute
	maxExpungeSweep     = 24 * time.Hour
	minIdleFallbackPoll = 10 * time.Second
	maxIdleFallbackPoll = 10 * time.Minute
	minGoogleCheck      = 2 * time.Second
	maxGoogleCheck      = 10 * time.Minute
	minSyncPoll         = 30 * time.Second
	maxSyncPoll         = 24 * time.Hour
)

// validate reports each [watch] key that does not parse or lies out of range.
func (w WatchConfig) validate(add func(string, ...any)) {
	validateDuration("watch.retention", w.Retention, minRetention, maxRetention, add)
	if w.MaxEvents < minMaxEvents || w.MaxEvents > maxMaxEvents {
		add("watch.max_events must be %d to %d", minMaxEvents, maxMaxEvents)
	}
	validateDuration("watch.expunge_sweep", w.ExpungeSweep, minExpungeSweep, maxExpungeSweep, add)
	validateDuration("watch.idle_fallback_poll", w.IdleFallbackPoll, minIdleFallbackPoll, maxIdleFallbackPoll, add)
	validateDuration("watch.google_check", w.GoogleCheck, minGoogleCheck, maxGoogleCheck, add)
}

// validateAccountWatch reports problems in an account's watch list and sync
// table. where is "accounts.<name>" and p is the account's preset.
func validateAccountWatch(where string, a Account, p provider.Preset, add func(string, ...any)) {
	validateDuration(where+": sync.poll", a.Sync.Poll, minSyncPoll, maxSyncPoll, add)
	if p.Gmail {
		if a.Watch != nil || a.Sync.Folders != nil {
			add("%s: watch and sync.folders are not allowed on Gmail presets; they always watch All Mail", where)
		}
		return
	}
	var watch []string
	if a.Watch != nil {
		watch = *a.Watch
	}
	if slices.Contains(watch, "") {
		add("%s: watch has an empty folder name", where)
	}
	seen := make(map[string]int, len(watch))
	for _, f := range watch {
		if f == "" {
			continue
		}
		seen[f]++
		if seen[f] == 2 {
			add("%s: watch lists %s twice", where, f)
		}
	}
	if slices.Contains(a.Sync.Folders, "") {
		add("%s: sync.folders has an empty folder name", where)
	}
}

// parseDuration parses a Go duration such as 30s, 5m or 168h.
func parseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("duration %q: want a Go duration such as 30s, 5m or 168h", s)
	}
	return d, nil
}

// validateDuration reports the duration value at key when it does not parse
// or lies outside lo to hi.
func validateDuration(key, value string, lo, hi time.Duration, add func(string, ...any)) {
	d, err := parseDuration(value)
	if err != nil {
		add("%s: %v", key, err)
		return
	}
	if d < lo || d > hi {
		add("%s: %s must be between %s and %s", key, value, shortDuration(lo), shortDuration(hi))
	}
}

// shortDuration writes a whole number of seconds in its largest whole unit,
// such as 2160h, 5m or 30s.
func shortDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int64(d/time.Second))
	}
}
