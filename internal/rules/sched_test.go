package rules

import (
	"slices"
	"testing"
	"time"
)

// TestDueAndNextScheduled checks that the scheduler sees only live, enabled
// scheduled rules: DueScheduled returns those whose next run has come, in
// next_run_at order, and NextScheduled the earliest next run, or none.
func TestDueAndNextScheduled(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	ctx := t.Context()
	if _, ok, err := r.store.NextScheduled(ctx); err != nil || ok {
		t.Fatalf("NextScheduled on no rules = %v, %v, want none", ok, err)
	}
	hourlyDef := storeDef("hourly")
	hourlyDef.Schedule = Schedule{Kind: ScheduleEvery, Every: time.Hour}
	hourly := r.create(t, hourlyDef)
	hourlyNext := r.clock.Last().Add(time.Hour)
	dailyDef := storeDef("daily")
	dailyDef.Schedule = Schedule{Kind: ScheduleAt, At: ClockTime{Hour: 7}}
	daily := r.create(t, dailyDef)
	dailyNext := time.Date(2026, 11, 7, 7, 0, 0, 0, time.UTC)
	r.create(t, storeDef("manual"))
	offDef := storeDef("off")
	offDef.Schedule, offDef.Enabled = Schedule{Kind: ScheduleEvery, Every: 15 * time.Minute}, false
	r.create(t, offDef)

	next, ok, err := r.store.NextScheduled(ctx)
	if err != nil || !ok || !next.Equal(hourlyNext) {
		t.Fatalf("NextScheduled = %v, %v, %v, want %v", next, ok, err, hourlyNext)
	}
	due := func(at time.Time) []string {
		t.Helper()
		rules, err := r.store.DueScheduled(ctx, at)
		if err != nil {
			t.Fatalf("DueScheduled: %v", err)
		}
		return ruleIDs(rules)
	}
	for _, tc := range []struct {
		name string
		at   time.Time
		want []string
	}{
		{"just before the hourly run", hourlyNext.Add(-time.Nanosecond), nil},
		{"at the hourly run", hourlyNext, []string{hourly.ID}},
		{"at the daily run", dailyNext, []string{hourly.ID, daily.ID}},
	} {
		if got := due(tc.at); !slices.Equal(got, tc.want) {
			t.Errorf("%s: DueScheduled = %v, want %v", tc.name, got, tc.want)
		}
	}
	if _, err := r.store.Delete(ctx, storeActor, hourly.ID); err != nil {
		t.Fatal(err)
	}
	if next, ok, err := r.store.NextScheduled(ctx); err != nil || !ok || !next.Equal(dailyNext) {
		t.Errorf("NextScheduled after the delete = %v, %v, %v, want %v", next, ok, err, dailyNext)
	}
	if got := due(dailyNext.Add(48 * time.Hour)); !slices.Equal(got, []string{daily.ID}) {
		t.Errorf("DueScheduled after the delete = %v, want only the daily rule", got)
	}
	if _, err := r.store.Update(ctx, storeActor, daily.ID, Patch{Enabled: storePtr(false)}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.store.NextScheduled(ctx); err != nil || ok {
		t.Errorf("NextScheduled with every scheduled rule disabled or deleted = %v, %v, want none", ok, err)
	}
}
