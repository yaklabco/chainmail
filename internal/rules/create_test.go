package rules

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/watch"
)

// TestCreateWithDefaults checks a rule created from Defaults: the stored
// row, the returned rule, lookup by ID and by name, revision 1 with its
// rule_revisions row, and one created event that holds no rule text.
func TestCreateWithDefaults(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	tail := r.tail(t)
	d := storeDef("receipts")
	if d.Priority != DefaultPriority || d.MaxPerRun != DefaultMaxPerRun || !d.Enabled {
		t.Fatalf("Defaults gave %+v, want priority 100, max_per_run 500 and enabled", d)
	}
	got := r.create(t, d)
	at := r.clock.Last()
	if !ValidID(got.ID) || !strings.HasPrefix(got.ID, IDPrefix) {
		t.Fatalf("ID = %q, want a valid ID starting %s", got.ID, IDPrefix)
	}
	want := Rule{
		ID: got.ID, Definition: d, Revision: 1,
		CreatedAt: at, UpdatedAt: at, CreatedBy: "1000/cli", UpdatedBy: "1000/cli",
	}
	sameRule(t, got, want)
	sameRule(t, r.get(t, got.ID), want)
	sameRule(t, r.get(t, "receipts"), want)
	r.wantColumns(t, got.ID, map[string]string{
		"accounts": "[]", "schedule": "", "on_new": "0", "stop": "0", "enabled": "1",
		"disabled_reason": "", "revision": "1", "deleted_at": "NULL", "next_run_at": "NULL",
		"last_run_at": "NULL", "failed_runs": "0", "created_at": watch.FormatTime(at),
		"updated_at": watch.FormatTime(at), "created_by": "1000/cli", "updated_by": "1000/cli",
	})
	wantRuleChanges(t, r.ruleChanges(t, tail),
		watch.RuleChange{RuleID: got.ID, Name: "receipts", Change: ChangeCreated, Revision: 1, Actor: "1000/cli"})
	rev := r.lastRevision(t, got.ID)
	if rev.Revision != 1 || rev.Change != ChangeCreated || rev.Actor != "1000/cli" || !rev.At.Equal(at) {
		t.Errorf("revision row = %d %s %s %v, want 1 created 1000/cli %v", rev.Revision, rev.Change, rev.Actor, rev.At, at)
	}
	sameRule(t, rev.Rule, want)
	r.noRuleText(t)
}

// TestCreateWithEveryField checks a rule with every field set: accounts
// sorted and de-duplicated, the schedule stored as text with its first run
// planned, and the forward, copy, link and move arguments kept, while the
// event still holds no rule text.
func TestCreateWithEveryField(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	got := r.create(t, fullDef("stripe"))
	at := r.clock.Last()
	want := Rule{
		ID: got.ID, Definition: fullDef("stripe"), Revision: 1,
		CreatedAt: at, UpdatedAt: at, CreatedBy: "1000/cli", UpdatedBy: "1000/cli",
		NextRunAt: at.Add(6 * time.Hour),
	}
	want.Accounts = []string{"gmail", "work"}
	sameRule(t, got, want)
	sameRule(t, r.get(t, got.ID), want)
	sameRule(t, r.get(t, "stripe"), want)
	r.wantColumns(t, got.ID, map[string]string{
		"accounts": `["gmail","work"]`, "schedule": "every:6h", "on_new": "1", "stop": "1",
		"priority": "7", "max_per_run": "42", "next_run_at": watch.FormatTime(at.Add(6 * time.Hour)),
	})
	sameRule(t, r.lastRevision(t, got.ID).Rule, want)
	r.noRuleText(t)
}

// TestCreatePlansTheFirstRun checks next_run_at on create: every runs its
// duration after the create, at runs at the next wall time in the store's
// location, and a disabled or unscheduled rule has none.
func TestCreatePlansTheFirstRun(t *testing.T) {
	t.Parallel()
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	first := storeStart.Add(time.Millisecond) // the instant Create reads
	seven := Schedule{Kind: ScheduleAt, At: ClockTime{Hour: 7}}
	tests := []struct {
		name     string
		loc      *time.Location
		schedule Schedule
		enabled  bool
		want     time.Time
	}{
		{"every 15m", time.UTC, Schedule{Kind: ScheduleEvery, Every: 15 * time.Minute}, true, first.Add(15 * time.Minute)},
		{"every 7d", time.UTC, Schedule{Kind: ScheduleEvery, Every: 7 * 24 * time.Hour}, true, first.Add(7 * 24 * time.Hour)},
		// 09:00 UTC is past 07:00 UTC, so the first run is tomorrow.
		{"at 07:00 in UTC", time.UTC, seven, true, time.Date(2026, 11, 7, 7, 0, 0, 0, time.UTC)},
		// 09:00 UTC is 01:00 PST on 2026-11-06, so the first run is 07:00 PST that day.
		{"at 07:00 in Los Angeles", la, seven, true, time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC)},
		{"at 07:00 disabled", time.UTC, seven, false, time.Time{}},
		{"no schedule", time.UTC, Schedule{}, true, time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newStoreRigWith(t, func(o *StoreOptions) { o.Location = tc.loc })
			d := storeDef("scheduled")
			d.Schedule, d.Enabled = tc.schedule, tc.enabled
			got := r.create(t, d)
			if at := r.clock.Last(); !at.Equal(first) {
				t.Fatalf("Create read the clock at %v, want %v", at, first)
			}
			if !got.NextRunAt.Equal(tc.want) {
				t.Errorf("NextRunAt = %v, want %v", got.NextRunAt, tc.want)
			}
			stored := "NULL"
			if !tc.want.IsZero() {
				stored = watch.FormatTime(tc.want)
			}
			r.wantColumns(t, got.ID, map[string]string{"next_run_at": stored, "schedule": tc.schedule.String()})
		})
	}
}

// TestNewStoreDefaults checks a Store built with only DB and Log: the wall
// clock stamps the rule and at schedules run on the host's local zone.
func TestNewStoreDefaults(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	s := NewStore(StoreOptions{DB: r.db, Log: r.log})
	d := storeDef("daily")
	d.Schedule = Schedule{Kind: ScheduleAt, At: ClockTime{Hour: 7}}
	before := time.Now()
	got, err := s.Create(t.Context(), storeActor, d)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	after := time.Now()
	if got.CreatedAt.Before(before.Round(0)) || got.CreatedAt.After(after.Round(0)) {
		t.Errorf("CreatedAt = %v, want between %v and %v", got.CreatedAt, before, after)
	}
	local := got.NextRunAt.In(time.Local)
	if !got.NextRunAt.After(got.CreatedAt) || got.NextRunAt.Sub(got.CreatedAt) > 25*time.Hour ||
		local.Hour() != 7 || local.Minute() != 0 || local.Second() != 0 {
		t.Errorf("NextRunAt = %v (local %v), want the next 07:00 local after %v", got.NextRunAt, local, got.CreatedAt)
	}
}

// TestCreateUsesRand checks that rule IDs take their random part from
// StoreOptions.Rand.
func TestCreateUsesRand(t *testing.T) {
	t.Parallel()
	r := newStoreRigWith(t, func(o *StoreOptions) { o.Rand = bytes.NewReader(make([]byte, 10)) })
	got := r.create(t, storeDef("receipts"))
	if !strings.HasSuffix(got.ID, "0000000000000000") || len(got.ID) != len(IDPrefix)+26 {
		t.Errorf("ID = %q, want %s, ten time characters and sixteen zeros", got.ID, IDPrefix)
	}
}

// TestCreateRefusesATakenName checks that a second live rule cannot take a
// name, and that the refusal writes nothing.
func TestCreateRefusesATakenName(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	r.create(t, storeDef("receipts"))
	tail := r.tail(t)
	_, err := r.store.Create(t.Context(), storeActor, storeDef("receipts"))
	wantRefused(t, err, errNameTaken)
	if n := r.count(t, "SELECT count(*) FROM rules"); n != 1 {
		t.Errorf("%d rules, want 1", n)
	}
	if n := r.count(t, "SELECT count(*) FROM rule_revisions"); n != 1 {
		t.Errorf("%d revision rows, want 1", n)
	}
	if got := r.tail(t); got != tail {
		t.Errorf("log tail = %d, want %d unchanged", got, tail)
	}
}

// TestCreateRefusesWhatTheTablesCannotHold checks the store's own floor:
// each refusal is its fixed InvalidError and writes nothing.
func TestCreateRefusesWhatTheTablesCannotHold(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*Definition)
		want error
	}{
		{"upper case name", func(d *Definition) { d.Name = "Receipts" }, errStoreName},
		{"name like an ID", func(d *Definition) { d.Name = "ru_receipts" }, errStoreName},
		{"empty name", func(d *Definition) { d.Name = "" }, errStoreName},
		{"no actions", func(d *Definition) { d.Actions = nil }, errStoreActions},
		{"17 actions", func(d *Definition) {
			d.Actions = make([]Action, MaxActions+1)
			for i := range d.Actions {
				d.Actions[i] = Action{Kind: ActFlag}
			}
		}, errStoreActions},
		{"unknown action kind", func(d *Definition) { d.Actions = []Action{{Kind: "explode"}} }, errStoreAction},
		{"schedule under 15 minutes", func(d *Definition) {
			d.Schedule = Schedule{Kind: ScheduleEvery, Every: 10 * time.Minute}
		}, errStoreSchedule},
		{"unknown schedule kind", func(d *Definition) {
			d.Schedule = Schedule{Kind: "weekly", Every: time.Hour}
		}, errStoreSchedule},
		{"no schedule with a gap", func(d *Definition) {
			d.Schedule = Schedule{Every: time.Hour}
		}, errStoreSchedule},
		{"every schedule with a clock time", func(d *Definition) {
			d.Schedule = Schedule{Kind: ScheduleEvery, Every: time.Hour, At: ClockTime{Hour: 7}}
		}, errStoreSchedule},
		{"label action with a folder", func(d *Definition) {
			d.Actions = []Action{{Kind: ActLabel, Label: "Receipts", Folder: "Archive"}}
		}, errStoreAction},
		{"flag action with a recipient", func(d *Definition) {
			d.Actions = []Action{{Kind: ActFlag, To: []string{markAddress}}}
		}, errStoreAction},
		{"forward action with an item", func(d *Definition) {
			d.Actions = []Action{{Kind: ActForward, To: []string{markAddress}, ItemID: "it_x"}}
		}, errStoreAction},
		{"priority too low", func(d *Definition) { d.Priority = MinPriority - 1 }, errStorePriority},
		{"priority too high", func(d *Definition) { d.Priority = MaxPriority + 1 }, errStorePriority},
		{"max_per_run negative", func(d *Definition) { d.MaxPerRun = -1 }, errStoreMaxPerRun},
		{"max_per_run too high", func(d *Definition) { d.MaxPerRun = MaxMaxPerRun + 1 }, errStoreMaxPerRun},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newStoreRig(t)
			d := storeDef("receipts")
			tc.edit(&d)
			_, err := r.store.Create(t.Context(), storeActor, d)
			wantRefused(t, err, tc.want)
			if n := r.count(t, "SELECT count(*) FROM rules"); n != 0 {
				t.Errorf("%d rules, want 0", n)
			}
			if got := r.tail(t); got != 0 {
				t.Errorf("log tail = %d, want 0", got)
			}
		})
	}
}

// TestGetUnknown checks that an unknown ID, an unknown name and the empty
// string are not found.
func TestGetUnknown(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	r.create(t, storeDef("receipts"))
	for _, rule := range []string{"ru_unknown", "unknown", ""} {
		_, err := r.store.Get(t.Context(), rule)
		wantRefused(t, err, ErrNotFound)
	}
}
