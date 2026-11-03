package watch

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestCacheGroupNames pins which names are cache groups and which are
// client groups. No name is both.
func TestCacheGroupNames(t *testing.T) {
	t.Parallel()
	if got := CacheGroup("ainslies"); got != "cache.ainslies" {
		t.Errorf("CacheGroup(ainslies) = %q, want cache.ainslies", got)
	}
	long := strings.Repeat("a", 63)
	tests := []struct {
		name          string
		cache, client bool
	}{
		{"cache.ainslies", true, false},
		{"cache.a", true, false},
		{"cache.0", true, false},
		{"cache.work-2", true, false},
		// Account names reach 63 characters, so cache group names reach 69.
		{"cache." + long, true, false},
		{"cache." + long + "a", false, false},
		{"cache.", false, false},
		{"cache.Bad", false, false},
		{"cache.-work", false, false},
		{"cache.my_work", false, false},
		{"cache.a.b", false, false},
		{"cache.café", false, false},
		{"Cache.work", false, false},
		{"cache", false, true},
		{"cachex", false, true},
		{"caches.work", false, true},
		{"cache-work", false, true},
		{"cache_work", false, true},
		{"mcp.cache.work", false, true},
	}
	for _, tt := range tests {
		if got := IsCacheGroup(tt.name); got != tt.cache {
			t.Errorf("IsCacheGroup(%q) = %v, want %v", tt.name, got, tt.cache)
		}
		if got := ValidGroupName(tt.name); got != tt.client {
			t.Errorf("ValidGroupName(%q) = %v, want %v", tt.name, got, tt.client)
		}
	}
}

// TestCacheGroupInLog checks that every Log method that validates a group
// name takes a cache group, including one longer than ValidGroupName
// allows, and still refuses a malformed one.
func TestCacheGroupInLog(t *testing.T) {
	t.Parallel()
	l, _, clock := newLog(t)
	name := CacheGroup("ainslies")
	publish(t, l, newEvent("ainslies", "INBOX", 1), newEvent("ainslies", "INBOX", 2), newEvent("ainslies", "INBOX", 3))
	acked, created, err := l.Group(t.Context(), name)
	if err != nil || acked != 3 || !created {
		t.Fatalf("Group(%s) = %d, %v, %v, want 3, true, nil", name, acked, created, err)
	}
	publish(t, l, newEvent("ainslies", "INBOX", 4))
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

	long := CacheGroup(strings.Repeat("a", 63))
	if _, created, err := l.Group(t.Context(), long); err != nil || !created {
		t.Errorf("Group(69-character cache group) = created %v, %v, want true, nil", created, err)
	}
	for _, bad := range []string{"cache.", "cache.Bad", "cache.a_b"} {
		if _, _, err := l.Group(t.Context(), bad); err == nil {
			t.Errorf("Group(%q) accepted a malformed cache group", bad)
		}
		if err := l.Ack(t.Context(), bad, 1); err == nil {
			t.Errorf("Ack(%q) accepted a malformed cache group", bad)
		}
		if err := l.TouchGroup(t.Context(), bad); err == nil {
			t.Errorf("TouchGroup(%q) accepted a malformed cache group", bad)
		}
		if _, err := l.ResetGroup(t.Context(), bad, 0, 1); err == nil {
			t.Errorf("ResetGroup(%q) accepted a malformed cache group", bad)
		}
	}
}

// TestTrimKeepsIdleCacheGroups checks that idle-group expiry removes a
// quiet client group but never a cache group, however long it sat idle.
func TestTrimKeepsIdleCacheGroups(t *testing.T) {
	t.Parallel()
	l, _, clock := newLog(t)
	work, long := CacheGroup("work"), CacheGroup(strings.Repeat("w", 63))
	for _, g := range []string{"idle", work, long} {
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
	if want := []string{work, long}; !reflect.DeepEqual(names, want) {
		t.Errorf("groups after trim = %v, want %v", names, want)
	}
}
