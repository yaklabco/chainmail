package daemon

import (
	"testing"

	"github.com/yaklabco/chainmail/internal/rules"
)

// TestNewBuildsTheRulesStore checks that New gives the server its rule
// store over state.db, the clock the rule runners wait on, and the
// act-once lookup the dry-run counts skipped matches with.
func TestNewBuildsTheRulesStore(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	if s.rules == nil {
		t.Fatal("New left the rule store nil")
	}
	if _, ok := s.rulesClock.(daemonClock); !ok {
		t.Errorf("rulesClock = %T, want daemonClock", s.rulesClock)
	}
	if lookup, ok := s.ruleApplied.(*rules.Store); !ok || lookup != s.rules {
		t.Errorf("ruleApplied = %T, want the rule store", s.ruleApplied)
	}
	list, err := s.rules.List(t.Context(), rules.ListQuery{})
	if err != nil || len(list) != 0 {
		t.Errorf("List = %v, %v, want no rules", list, err)
	}
}
