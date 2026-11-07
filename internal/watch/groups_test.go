package watch

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestRulesGroupNames pins which names are rules groups, which are cache
// groups and which are client groups. No name is two of them.
func TestRulesGroupNames(t *testing.T) {
	t.Parallel()
	if got := RulesGroup("personal"); got != "rules.personal" {
		t.Errorf("RulesGroup(personal) = %q, want rules.personal", got)
	}
	long := strings.Repeat("a", 63)
	tests := []struct {
		name                 string
		rules, cache, client bool
	}{
		{"rules.personal", true, false, false},
		{"rules.a", true, false, false},
		{"rules.0", true, false, false},
		{"rules.work-2", true, false, false},
		// Account names reach 63 characters, so rules group names reach 69.
		{"rules." + long, true, false, false},
		{"rules." + long + "a", false, false, false},
		{"rules.", false, false, false},
		{"rules.Bad", false, false, false},
		{"rules.-work", false, false, false},
		{"rules.my_work", false, false, false},
		{"rules.a.b", false, false, false},
		{"rules.café", false, false, false},
		{"Rules.work", false, false, false},
		{"cache.work", false, true, false},
		{"rules", false, false, true},
		{"rulesx", false, false, true},
		{"rulesx.work", false, false, true},
		{"rule.work", false, false, true},
		{"rules-work", false, false, true},
		{"rules_work", false, false, true},
		{"mcp.rules.work", false, false, true},
		{"", false, false, false},
	}
	for _, tt := range tests {
		if got := IsRulesGroup(tt.name); got != tt.rules {
			t.Errorf("IsRulesGroup(%q) = %v, want %v", tt.name, got, tt.rules)
		}
		if got := IsCacheGroup(tt.name); got != tt.cache {
			t.Errorf("IsCacheGroup(%q) = %v, want %v", tt.name, got, tt.cache)
		}
		if got := ValidGroupName(tt.name); got != tt.client {
			t.Errorf("ValidGroupName(%q) = %v, want %v", tt.name, got, tt.client)
		}
	}
}

// TestRulesGroupInLog checks that every Log method that validates a group
// name takes a rules group, including one longer than ValidGroupName
// allows, and still refuses a malformed one.
func TestRulesGroupInLog(t *testing.T) {
	t.Parallel()
	l, _, clock := newLog(t)
	name := RulesGroup("personal")
	publish(t, l, newEvent("personal", "INBOX", 1), newEvent("personal", "INBOX", 2), newEvent("personal", "INBOX", 3))
	acked, created, err := l.Group(t.Context(), name)
	if err != nil || acked != 3 || !created {
		t.Fatalf("Group(%s) = %d, %v, %v, want 3, true, nil", name, acked, created, err)
	}
	publish(t, l, newEvent("personal", "INBOX", 4))
	if err := l.Ack(t.Context(), name, 4); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	clock.Set(t0.Add(time.Hour))
	if err := l.TouchGroup(t.Context(), name); err != nil {
		t.Fatalf("TouchGroup: %v", err)
	}
	applied, err := l.ResetGroup(t.Context(), name, 4, 2)
	if err != nil || !applied {
		t.Fatalf("ResetGroup = %v, %v, want true, nil", applied, err)
	}
	groups, err := l.Groups(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []GroupInfo{{Name: name, AckedID: 2, Lag: 2, CreatedAt: t0, LastSeenAt: t0.Add(time.Hour)}}
	if !reflect.DeepEqual(groups, want) {
		t.Errorf("Groups =\n%+v\nwant\n%+v", groups, want)
	}

	long := RulesGroup(strings.Repeat("a", 63))
	if _, created, err := l.Group(t.Context(), long); err != nil || !created {
		t.Errorf("Group(69-character rules group) = created %v, %v, want true, nil", created, err)
	}
	for _, bad := range []string{"rules.", "rules.Bad", "rules.a_b"} {
		if _, _, err := l.Group(t.Context(), bad); err == nil {
			t.Errorf("Group(%q) accepted a malformed rules group", bad)
		}
		if err := l.Ack(t.Context(), bad, 1); err == nil {
			t.Errorf("Ack(%q) accepted a malformed rules group", bad)
		}
		if err := l.TouchGroup(t.Context(), bad); err == nil {
			t.Errorf("TouchGroup(%q) accepted a malformed rules group", bad)
		}
		if _, err := l.ResetGroup(t.Context(), bad, 0, 1); err == nil {
			t.Errorf("ResetGroup(%q) accepted a malformed rules group", bad)
		}
	}
}

// TestTrimKeepsIdleRulesGroups checks that idle-group expiry removes a
// quiet client group but never a rules group or a cache group, however
// long they sat idle. A rule runner of an account whose cache is off reads
// nothing for weeks, and its group must not reopen at the tail.
func TestTrimKeepsIdleRulesGroups(t *testing.T) {
	t.Parallel()
	l, _, clock := newLog(t)
	work, long, cache := RulesGroup("work"), RulesGroup(strings.Repeat("r", 63)), CacheGroup("work")
	for _, g := range []string{"idle", "rulesx.work", work, long, cache} {
		if _, _, err := l.Group(t.Context(), g); err != nil {
			t.Fatal(err)
		}
	}
	clock.Set(t0.Add(week + time.Hour))
	if _, err := l.Trim(t.Context(), week, 1000); err != nil {
		t.Fatal(err)
	}
	groups, err := l.Groups(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range groups {
		names = append(names, g.Name)
	}
	if want := []string{cache, long, work}; !reflect.DeepEqual(names, want) {
		t.Errorf("groups after trim = %v, want %v", names, want)
	}
}
