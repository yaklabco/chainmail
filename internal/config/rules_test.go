package config

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	// cmd/chainmail embeds the zone database, so the tests do too and pass
	// on hosts without zoneinfo.
	_ "time/tzdata"
)

// describeRules writes resolved [rules] settings as "<zone> HH:MM soon <n>",
// so a test compares them, the zone's name included, as one string.
func describeRules(s RulesSettings) string {
	return fmt.Sprintf("%s %02d:%02d soon %d", s.Location, s.DueHour, s.DueMinute, s.DueSoonDays)
}

// showIntPtr writes an *int for a test message.
func showIntPtr(p *int) string {
	if p == nil {
		return "nil"
	}
	return strconv.Itoa(*p)
}

func TestRulesDefaults(t *testing.T) {
	t.Parallel()
	for name, config := range map[string]string{
		"no rules table":    validConfig,
		"empty rules table": validConfig + "\n[rules]\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Parse([]byte(config))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			r := cfg.Rules
			if r.Timezone != "" || r.DueTime != "08:00" || r.DueSoonDays == nil || *r.DueSoonDays != 2 {
				t.Errorf("rules = {Timezone:%q DueTime:%q DueSoonDays:%s}, want {\"\" 08:00 2}",
					r.Timezone, r.DueTime, showIntPtr(r.DueSoonDays))
			}
			s := cfg.RulesSettings()
			if s.Location != time.Local {
				t.Errorf("Location = %v, want time.Local", s.Location)
			}
			if got, want := describeRules(s), "Local 08:00 soon 2"; got != want {
				t.Errorf("RulesSettings() = %s, want %s", got, want)
			}
		})
	}
}

func TestRulesSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		rules string
		want  string
	}{
		{
			name:  "los angeles",
			rules: "timezone = \"America/Los_Angeles\"\ndue_time = \"07:30\"\ndue_soon_days = 3\n",
			want:  "America/Los_Angeles 07:30 soon 3",
		},
		{
			name:  "utc at midnight with due soon off",
			rules: "timezone = \"UTC\"\ndue_time = \"00:00\"\ndue_soon_days = 0\n",
			want:  "UTC 00:00 soon 0",
		},
		{
			name:  "last minute and most days",
			rules: "due_time = \"23:59\"\ndue_soon_days = 30\n",
			want:  "Local 23:59 soon 30",
		},
		{
			name:  "zone alone",
			rules: "timezone = \"Europe/London\"\n",
			want:  "Europe/London 08:00 soon 2",
		},
		{
			name:  "empty timezone is local",
			rules: "timezone = \"\"\n",
			want:  "Local 08:00 soon 2",
		},
		{
			name:  "empty due time is the default",
			rules: "due_time = \"\"\n",
			want:  "Local 08:00 soon 2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Parse([]byte("[rules]\n" + tt.rules))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := describeRules(cfg.RulesSettings()); got != tt.want {
				t.Errorf("RulesSettings() = %s, want %s", got, tt.want)
			}
		})
	}
}

// A Config built without Parse, as tests sometimes do, gets the defaults
// for unset or unusable values rather than zero.
func TestRulesSettingsUnparsedFallsBackToDefaults(t *testing.T) {
	t.Parallel()
	zero, five, negative, many := 0, 5, -1, 31
	tests := []struct {
		name string
		cfg  *Config
		want string
	}{
		{name: "empty", cfg: &Config{}, want: "Local 08:00 soon 2"},
		{
			name: "unparsable",
			cfg:  &Config{Rules: RulesConfig{Timezone: "Mars/Base", DueTime: "8:00", DueSoonDays: &negative}},
			want: "Local 08:00 soon 2",
		},
		{
			name: "out of range",
			cfg:  &Config{Rules: RulesConfig{DueTime: "24:00", DueSoonDays: &many}},
			want: "Local 08:00 soon 2",
		},
		{name: "zero due soon days kept", cfg: &Config{Rules: RulesConfig{DueSoonDays: &zero}}, want: "Local 08:00 soon 0"},
		{
			name: "valid values kept",
			cfg:  &Config{Rules: RulesConfig{Timezone: "UTC", DueTime: "21:15", DueSoonDays: &five}},
			want: "UTC 21:15 soon 5",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := describeRules(tt.cfg.RulesSettings()); got != tt.want {
				t.Errorf("RulesSettings() = %s, want %s", got, tt.want)
			}
		})
	}
	if loc := (&Config{}).RulesSettings().Location; loc != time.Local {
		t.Errorf("empty Config: Location = %v, want time.Local itself", loc)
	}
}

func TestParseClock(t *testing.T) {
	t.Parallel()
	type clock struct {
		hour, minute int
		ok           bool
	}
	// A slice, not a map, so the leading-space input is a plain value.
	for _, tt := range []struct {
		in   string
		want clock
	}{
		{"00:00", clock{0, 0, true}},
		{"08:00", clock{8, 0, true}},
		{"07:30", clock{7, 30, true}},
		{"23:59", clock{23, 59, true}},
		{"8:00", clock{}},
		{"24:00", clock{}},
		{"08:60", clock{}},
		{"08:0", clock{}},
		{"08:00:00", clock{}},
		{" 08:00", clock{}},
		{"0800", clock{}},
		{"", clock{}},
		{"noon", clock{}},
	} {
		hour, minute, ok := parseClock(tt.in)
		if got := (clock{hour, minute, ok}); got != tt.want {
			t.Errorf("parseClock(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestRulesRejectsUnknownKeys(t *testing.T) {
	t.Parallel()
	for name, config := range map[string]string{
		"misspelled timezone": "[rules]\ntimezon = \"UTC\"\n",
		"unknown key":         "[rules]\ndue_soon = 2\n",
		"nested table":        "[rules.work]\ntimezone = \"UTC\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse([]byte(config)); !errors.Is(err, ErrUnknownKey) {
				t.Errorf("err = %v, want ErrUnknownKey", err)
			}
		})
	}
}

func TestRulesRefusesWrongTypes(t *testing.T) {
	t.Parallel()
	for name, extra := range map[string]string{
		"string for int":  `due_soon_days = "2"`,
		"float for int":   "due_soon_days = 2.5",
		"int for string":  "due_time = 800",
		"bool for string": "timezone = true",
		"time for string": "due_time = 08:00:00",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse([]byte("[rules]\n" + extra + "\n")); err == nil {
				t.Errorf("Parse accepted %q", extra)
			}
		})
	}
}

func TestRulesValidBounds(t *testing.T) {
	t.Parallel()
	for _, config := range []string{
		"[rules]\ntimezone = \"UTC\"\n",
		"[rules]\ntimezone = \"America/Los_Angeles\"\n",
		"[rules]\ntimezone = \"Europe/London\"\n",
		"[rules]\ntimezone = \"\"\n",
		"[rules]\ndue_time = \"00:00\"\n",
		"[rules]\ndue_time = \"23:59\"\n",
		"[rules]\ndue_time = \"\"\n",
		"[rules]\ndue_soon_days = 0\n",
		"[rules]\ndue_soon_days = 30\n",
		"[rules]\ntimezone = \"America/Los_Angeles\"\ndue_time = \"08:00\"\ndue_soon_days = 2\n",
	} {
		if _, err := Parse([]byte(config)); err != nil {
			t.Errorf("%q: err = %v, want valid", config, err)
		}
	}
}

func TestRulesInvalid(t *testing.T) {
	t.Parallel()
	const zoneHint = "; want an IANA name such as America/Los_Angeles or UTC"
	const clockHint = " must be a 24-hour time HH:MM, 00:00 to 23:59"
	tests := []struct {
		name    string
		config  string
		problem string
	}{
		{name: "unknown zone", config: "[rules]\ntimezone = \"Mars/Base\"\n", problem: `rules.timezone: unknown time zone "Mars/Base"` + zoneHint},
		{name: "zone path", config: "[rules]\ntimezone = \"../etc/passwd\"\n", problem: `rules.timezone: unknown time zone "../etc/passwd"` + zoneHint},
		{name: "one-digit hour", config: "[rules]\ndue_time = \"8:00\"\n", problem: `rules.due_time: "8:00"` + clockHint},
		{name: "hour 24", config: "[rules]\ndue_time = \"24:00\"\n", problem: `rules.due_time: "24:00"` + clockHint},
		{name: "minute 60", config: "[rules]\ndue_time = \"08:60\"\n", problem: `rules.due_time: "08:60"` + clockHint},
		{name: "seconds", config: "[rules]\ndue_time = \"08:00:00\"\n", problem: `rules.due_time: "08:00:00"` + clockHint},
		{name: "words", config: "[rules]\ndue_time = \"noon\"\n", problem: `rules.due_time: "noon"` + clockHint},
		{name: "due soon negative", config: "[rules]\ndue_soon_days = -1\n", problem: "rules.due_soon_days must be 0 to 30"},
		{name: "due soon too many", config: "[rules]\ndue_soon_days = 31\n", problem: "rules.due_soon_days must be 0 to 30"},
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

// Parse reports every [rules] problem at once, in key order, and nothing
// else for a config that sets only [rules].
func TestRulesReportsEveryProblem(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte("[rules]\ntimezone = \"Mars/Base\"\ndue_time = \"8:00\"\ndue_soon_days = 31\n"))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	want := []string{
		`rules.timezone: unknown time zone "Mars/Base"; want an IANA name such as America/Los_Angeles or UTC`,
		`rules.due_time: "8:00" must be a 24-hour time HH:MM, 00:00 to 23:59`,
		"rules.due_soon_days must be 0 to 30",
	}
	if !slices.Equal(ve.Problems, want) {
		t.Errorf("problems = %q, want %q", ve.Problems, want)
	}
}
