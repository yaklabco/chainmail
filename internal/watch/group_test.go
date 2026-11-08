package watch

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGroupIsCreatedAtTail(t *testing.T) {
	t.Parallel()
	l, _, clock := newLog(t)
	acked, created, err := l.Group(t.Context(), "empty")
	if err != nil || acked != 0 || !created {
		t.Errorf("Group(empty log) = %d, %v, %v, want 0, true, nil", acked, created, err)
	}
	publish(t, l, newEvent("a", "INBOX", 1), newEvent("a", "INBOX", 2), newEvent("a", "INBOX", 3))
	acked, created, err = l.Group(t.Context(), "bot")
	if err != nil || acked != 3 || !created {
		t.Fatalf("Group = %d, %v, %v, want 3, true, nil", acked, created, err)
	}
	publish(t, l, newEvent("a", "INBOX", 4))
	clock.Set(t0.Add(time.Hour))
	acked, created, err = l.Group(t.Context(), "bot")
	if err != nil || acked != 3 || created {
		t.Fatalf("second Group = %d, %v, %v, want 3, false, nil", acked, created, err)
	}
	groups, err := l.Groups(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []GroupInfo{
		{Name: "bot", AckedID: 3, Lag: 1, CreatedAt: t0, LastSeenAt: t0.Add(time.Hour)},
		{Name: "empty", AckedID: 0, Lag: 4, CreatedAt: t0, LastSeenAt: t0},
	}
	if !reflect.DeepEqual(groups, want) {
		t.Errorf("Groups =\n%+v\nwant\n%+v", groups, want)
	}
	if _, _, err := l.Group(t.Context(), "Bad Name"); err == nil {
		t.Error("Group accepted an invalid name")
	}
}

func TestAck(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	if _, _, err := l.Group(t.Context(), "bot"); err != nil {
		t.Fatal(err)
	}
	publish(t, l, newEvent("a", "INBOX", 1), newEvent("a", "INBOX", 2), newEvent("a", "INBOX", 3),
		newEvent("a", "INBOX", 4), newEvent("a", "INBOX", 5))
	steps := []struct {
		name  string
		group string
		ack   int64
		want  int64
	}{
		{name: "raises", group: "bot", ack: 3, want: 3},
		{name: "never lowers", group: "bot", ack: 2, want: 3},
		{name: "negative is ignored", group: "bot", ack: -4, want: 3},
		{name: "caps at tail", group: "bot", ack: 99, want: 5},
		{name: "creates an unknown group", group: "fresh", ack: 4, want: 4},
	}
	for _, s := range steps {
		if err := l.Ack(t.Context(), s.group, s.ack); err != nil {
			t.Fatalf("%s: Ack: %v", s.name, err)
		}
		acked, created, err := l.Group(t.Context(), s.group)
		if err != nil || created || acked != s.want {
			t.Errorf("%s: Group = %d, created %v, %v, want %d", s.name, acked, created, err, s.want)
		}
	}
	if err := l.Ack(t.Context(), "-bad", 1); err == nil {
		t.Error("Ack accepted an invalid name")
	}
}

func TestGroupsLagCountsRetainedEvents(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	publish(t, l, newEvent("a", "INBOX", 1), newEvent("a", "INBOX", 2), newEvent("a", "INBOX", 3),
		newEvent("a", "INBOX", 4), newEvent("a", "INBOX", 5))
	if err := l.Ack(t.Context(), "behind", 1); err != nil {
		t.Fatal(err)
	}
	if err := l.Ack(t.Context(), "current", 5); err != nil {
		t.Fatal(err)
	}
	lags := func() map[string]int64 {
		groups, err := l.Groups(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		out := make(map[string]int64)
		for _, g := range groups {
			out[g.Name] = g.Lag
		}
		return out
	}
	if got, want := lags(), map[string]int64{"behind": 4, "current": 0}; !reflect.DeepEqual(got, want) {
		t.Errorf("lags = %v, want %v", got, want)
	}
	if _, err := l.Trim(t.Context(), week, 2); err != nil {
		t.Fatal(err)
	}
	if got, want := lags(), map[string]int64{"behind": 2, "current": 0}; !reflect.DeepEqual(got, want) {
		t.Errorf("lags after trim = %v, want %v", got, want)
	}
}

func TestDeleteGroup(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	if _, _, err := l.Group(t.Context(), "bot"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []bool{true, false} {
		existed, err := l.DeleteGroup(t.Context(), "bot")
		if err != nil || existed != want {
			t.Errorf("DeleteGroup = %v, %v, want %v", existed, err, want)
		}
	}
}

func TestTrimPrunesIdleGroups(t *testing.T) {
	t.Parallel()
	l, _, clock := newLog(t)
	if _, _, err := l.Group(t.Context(), "idle"); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(100 * time.Hour))
	if _, _, err := l.Group(t.Context(), "busy"); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(week + time.Hour))
	if _, err := l.Trim(t.Context(), week, 1000); err != nil {
		t.Fatal(err)
	}
	groups, err := l.Groups(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Name != "busy" {
		t.Errorf("groups after trim = %+v, want only busy", groups)
	}
}

func TestValidGroupName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{"a", true},
		{"0", true},
		{"bot", true},
		{"my-bot_2.prod", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"", false},
		{"-bot", false},
		{".bot", false},
		{"_bot", false},
		{"Bot", false},
		{"bot name", false},
		{"bot/1", false},
		{"bot:1", false},
		{"bøt", false},
		{"cache.work", false},
		{"cache.", false},
		{"cache", true},
		{"caches.work", true},
		{"rules.work", false},
		{"rules.", false},
		{"rules", true},
		{"rulesx.work", true},
		{"rule.work", true},
	}
	for _, tt := range tests {
		if got := ValidGroupName(tt.name); got != tt.want {
			t.Errorf("ValidGroupName(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestResetGroup checks the compare-and-set recovery for a group left past
// the tail: it moves the acknowledged ID from exactly from to to, lowering
// it if need be, stamps last_seen_at and keeps created_at. It does not
// apply when the group has moved since from was read, or does not exist,
// and it refuses an invalid name or a negative to.
func TestResetGroup(t *testing.T) {
	t.Parallel()
	l, conn, clock := newLog(t)
	publish(t, l, newEvent("a", "INBOX", 1), newEvent("a", "INBOX", 2), newEvent("a", "INBOX", 3))
	if _, _, err := l.Group(t.Context(), "bot"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), "UPDATE event_groups SET acked_id = 50 WHERE name = 'bot'"); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(time.Hour))
	steps := []struct {
		name     string
		group    string
		from, to int64
		applied  bool
	}{
		{name: "applies", group: "bot", from: 50, to: 2, applied: true},
		{name: "moved since", group: "bot", from: 50, to: 1, applied: false},
		{name: "unknown group", group: "nobody", from: 0, to: 1, applied: false},
	}
	for _, s := range steps {
		applied, err := l.ResetGroup(t.Context(), s.group, s.from, s.to)
		if err != nil || applied != s.applied {
			t.Errorf("%s: ResetGroup = %v, %v; want %v, nil", s.name, applied, err, s.applied)
		}
	}
	groups, err := l.Groups(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []GroupInfo{{Name: "bot", AckedID: 2, Lag: 1, CreatedAt: t0, LastSeenAt: t0.Add(time.Hour)}}
	if !reflect.DeepEqual(groups, want) {
		t.Errorf("Groups =\n%+v\nwant\n%+v", groups, want)
	}
	if _, err := l.ResetGroup(t.Context(), "Bad Name", 0, 1); err == nil {
		t.Error("ResetGroup accepted an invalid name")
	}
	if _, err := l.ResetGroup(t.Context(), "bot", 2, -1); err == nil {
		t.Error("ResetGroup accepted a negative id")
	}
}

// TouchGroup stamps last_seen_at and nothing else, so a group a stream
// keeps open outlives the retention window without an Ack. It never
// creates a group: one deleted while its stream runs stays deleted.
func TestTouchGroup(t *testing.T) {
	t.Parallel()
	l, _, clock := newLog(t)
	publish(t, l, newEvent("a", "INBOX", 1), newEvent("a", "INBOX", 2))
	if _, _, err := l.Group(t.Context(), "bot"); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.Add(200 * time.Hour))
	if err := l.TouchGroup(t.Context(), "bot"); err != nil {
		t.Fatalf("TouchGroup: %v", err)
	}
	if err := l.TouchGroup(t.Context(), "gone"); err != nil {
		t.Fatalf("TouchGroup(unknown group): %v", err)
	}
	if _, err := l.Trim(t.Context(), 168*time.Hour, 1000); err != nil {
		t.Fatal(err)
	}
	groups, err := l.Groups(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []GroupInfo{{Name: "bot", AckedID: 2, Lag: 0, CreatedAt: t0, LastSeenAt: t0.Add(200 * time.Hour)}}
	if !reflect.DeepEqual(groups, want) {
		t.Errorf("Groups =\n%+v\nwant\n%+v", groups, want)
	}
	if err := l.TouchGroup(t.Context(), "Bad Name"); err == nil {
		t.Error("TouchGroup accepted an invalid name")
	}
}
