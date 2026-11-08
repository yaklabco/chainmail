package config

import "time"

// RulesConfig is the [rules] table: the time zone that rule schedules and
// work item due events use, and when due events fire. Every key is
// optional.
type RulesConfig struct {
	// Timezone is an IANA time zone name, such as "America/Los_Angeles" or
	// "UTC". Empty means the host's local zone.
	Timezone string `toml:"timezone"`
	// DueTime is the wall-clock time, HH:MM from 00:00 to 23:59, at which
	// due events fire. It defaults to "08:00".
	DueTime string `toml:"due_time"`
	// DueSoonDays is how many days before its due date an open item gets
	// item_due_soon: 0 to 30, default 2. 0 turns item_due_soon off, so a
	// nil pointer tells an unset key from 0.
	DueSoonDays *int `toml:"due_soon_days"`
}

// RulesSettings are the resolved [rules] values.
type RulesSettings struct {
	// Location is the zone of at: schedules and due events.
	Location *time.Location
	// DueHour and DueMinute are when due events fire, in Location.
	DueHour   int
	DueMinute int
	// DueSoonDays is how many days ahead item_due_soon fires; 0 is off.
	DueSoonDays int
}

// Rules defaults and bounds, as written in config.
const (
	defaultDueTime     = "08:00"
	defaultDueSoonDays = 2
	maxDueSoonDays     = 30
	// clockLayout is the HH:MM form of due_time.
	clockLayout = "15:04"
)

// RulesSettings returns the [rules] values resolved: the time zone loaded,
// the due time split into hour and minute. Parse has validated them. A
// Config that never went through Parse gets the defaults for unset or
// unusable values: the host's local zone, 08:00 and 2 days.
func (c *Config) RulesSettings() RulesSettings {
	loc := time.Local
	if c.Rules.Timezone != "" {
		if l, err := time.LoadLocation(c.Rules.Timezone); err == nil {
			loc = l
		}
	}
	hour, minute, ok := parseClock(c.Rules.DueTime)
	if !ok {
		hour, minute, _ = parseClock(defaultDueTime) // the default always parses
	}
	days := defaultDueSoonDays
	if d := c.Rules.DueSoonDays; d != nil && *d >= 0 && *d <= maxDueSoonDays {
		days = *d
	}
	return RulesSettings{Location: loc, DueHour: hour, DueMinute: minute, DueSoonDays: days}
}

// applyDefaults fills the unset [rules] keys. Timezone stays empty, which
// means the host's local zone.
func (r *RulesConfig) applyDefaults() {
	if r.DueTime == "" {
		r.DueTime = defaultDueTime
	}
	if r.DueSoonDays == nil {
		days := defaultDueSoonDays
		r.DueSoonDays = &days
	}
}

// parseClock parses a 24-hour wall-clock time written HH:MM, such as
// 08:00. time.Parse reads a one-digit hour, so the value must also format
// back to itself: 8:00 is refused.
func parseClock(s string) (hour, minute int, ok bool) {
	t, err := time.Parse(clockLayout, s)
	if err != nil || t.Format(clockLayout) != s {
		return 0, 0, false
	}
	return t.Hour(), t.Minute(), true
}

// validate reports each [rules] key that does not parse or lies out of
// range. An empty timezone is the host's local zone and is not checked.
func (r RulesConfig) validate(add func(string, ...any)) {
	if r.Timezone != "" {
		if _, err := time.LoadLocation(r.Timezone); err != nil {
			add("rules.timezone: unknown time zone %q; want an IANA name such as America/Los_Angeles or UTC", r.Timezone)
		}
	}
	if _, _, ok := parseClock(r.DueTime); !ok {
		add("rules.due_time: %q must be a 24-hour time HH:MM, 00:00 to 23:59", r.DueTime)
	}
	if d := r.DueSoonDays; d != nil && (*d < 0 || *d > maxDueSoonDays) {
		add("rules.due_soon_days must be 0 to %d", maxDueSoonDays)
	}
}
