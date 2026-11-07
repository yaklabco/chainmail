package rules

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		id   string
		want bool
	}{
		{"ru_01jcz8v2kq4f9w3m7xg5t6h8ya", true},
		{"ru_a", true},
		{"ru_" + strings.Repeat("a", 60), true},
		{"ru_" + strings.Repeat("a", 61), false},
		{"ru_", false},
		{"ru-01jcz8v2kq", false},
		{"it_01jcz8v2kq", false},
		{"RU_abc", false},
		{"ru_ABC", false},
		{"ru_a.b", false},
		{"", false},
		{"stripe-receipts", false},
	}
	for _, tt := range tests {
		if got := ValidID(tt.id); got != tt.want {
			t.Errorf("ValidID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestValidName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{"stripe-receipts", true},
		{"a", true},
		{"0day", true},
		{"a" + strings.Repeat("b", 62), true},
		{"a" + strings.Repeat("b", 63), false},
		{"", false},
		{"-leading", false},
		{"Upper", false},
		{"ru_abc", false},
		{"under_score", false},
		{"with space", false},
		{"dot.name", false},
		{"café", false},
	}
	for _, tt := range tests {
		if got := ValidName(tt.name); got != tt.want {
			t.Errorf("ValidName(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// The stored values are in state.db's CHECK constraints and in clients, so
// they never change.
func TestStoredValues(t *testing.T) {
	t.Parallel()
	got := []string{
		IDPrefix,
		string(TriggerNew), string(TriggerSchedule), string(TriggerManual),
		string(OutcomeOK), string(OutcomeCapped), string(OutcomeFailed), string(OutcomeCacheNotReady), string(OutcomeInterrupted),
		string(AppliedPending), string(AppliedDone), string(AppliedHeld),
		NoteReplayed, NoteOutcomeUnknown,
		ChangeCreated, ChangeUpdated, ChangeDeleted, ChangeRestored, ChangeDisabled, ChangeEnabled,
		ReasonAllFailed, ReasonRepeatedFailures,
	}
	want := []string{
		"ru_",
		"new", "schedule", "manual",
		"ok", "capped", "failed", "cache_not_ready", "interrupted",
		"pending", "applied", "held",
		"replayed", "outcome_unknown",
		"created", "updated", "deleted", "restored", "disabled", "enabled",
		"all_failed", "repeated_failures",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stored values = %q\nwant %q", got, want)
	}
	limits := []int{DefaultPriority, MinPriority, MaxPriority, DefaultMaxPerRun, MaxMaxPerRun, MaxActions, MaxDescription, MaxNote, MaxFolderName}
	wantLimits := []int{100, -1000000, 1000000, 500, 5000, 16, 1000, 2000, 255}
	if !reflect.DeepEqual(limits, wantLimits) {
		t.Errorf("limits = %v, want %v", limits, wantLimits)
	}
}

func TestDefaults(t *testing.T) {
	t.Parallel()
	want := Definition{Priority: 100, MaxPerRun: 500, Enabled: true}
	if got := Defaults(); !reflect.DeepEqual(got, want) {
		t.Errorf("Defaults() = %+v, want %+v", got, want)
	}
}

func TestNormalize(t *testing.T) {
	t.Parallel()
	full := Definition{
		Name:        "stripe-receipts",
		Description: "receipts",
		Accounts:    []string{"work", "gmail", "work", "personal", "gmail"},
		Query:       "from:stripe.com",
		OnNew:       true,
		Schedule:    Schedule{Kind: ScheduleAt, At: ClockTime{Hour: 7}},
		Priority:    -5,
		Stop:        true,
		Actions:     []Action{{Kind: ActForward, To: []string{"a@example.com"}}, {Kind: ActArchive}},
		MaxPerRun:   20,
		Enabled:     true,
	}
	tests := []struct {
		name string
		in   Definition
		want Definition
	}{
		{
			name: "accounts sorted and de-duplicated, other fields kept",
			in:   full,
			want: func() Definition {
				d := full
				d.Accounts = []string{"gmail", "personal", "work"}
				return d
			}(),
		},
		{
			name: "zero max_per_run filled, zero priority kept",
			in:   Definition{Name: "x", Actions: []Action{{Kind: ActRead}}},
			want: Definition{Name: "x", Actions: []Action{{Kind: ActRead}}, MaxPerRun: 500},
		},
		{
			name: "empty accounts become nil",
			in:   Definition{Name: "x", Accounts: []string{}, MaxPerRun: 1},
			want: Definition{Name: "x", MaxPerRun: 1},
		},
		{
			name: "defaults stay defaults",
			in:   Defaults(),
			want: Defaults(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.in.Normalize(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Normalize() = %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// Normalize returns slices of its own, so changing the result never
// changes the definition it came from.
func TestNormalizeCopies(t *testing.T) {
	t.Parallel()
	in := Definition{
		Accounts: []string{"b", "a"},
		Actions:  []Action{{Kind: ActForward, To: []string{"a@example.com"}}},
	}
	out := in.Normalize()
	out.Accounts[0] = "changed"
	out.Actions[0].To[0] = "changed@example.com"
	out.Actions[0].Note = "changed"
	if in.Accounts[0] != "b" || in.Actions[0].To[0] != "a@example.com" || in.Actions[0].Note != "" {
		t.Errorf("Normalize shares slices with its input: %+v", in)
	}
}
