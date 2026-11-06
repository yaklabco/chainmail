package items

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// modelMarker stands in for item text an agent wrote. No error text or hint
// may ever contain it, since error text reaches agents outside the
// untrusted wrapping.
const modelMarker = "zq-item-text-marker"

// checkInvalid fails the test unless err is an *InvalidError with Msg msg
// and a hint, and neither holds modelMarker. It returns the error.
func checkInvalid(t *testing.T, what string, err error, msg string) *InvalidError {
	t.Helper()
	var inv *InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("%s: err = %v (%T), want an *InvalidError", what, err, err)
	}
	if inv.Msg != msg {
		t.Errorf("%s: Msg = %q, want %q", what, inv.Msg, msg)
	}
	if inv.Hint == "" {
		t.Errorf("%s: Hint is empty", what)
	}
	if strings.Contains(inv.Error(), modelMarker) || strings.Contains(inv.Hint, modelMarker) {
		t.Errorf("%s: error %q with hint %q holds the request's text", what, inv.Error(), inv.Hint)
	}
	return inv
}

func TestParseType(t *testing.T) {
	t.Parallel()
	for _, want := range modelTypes {
		got, err := ParseType(string(want))
		if err != nil || got != want {
			t.Errorf("ParseType(%q) = %q, %v; want %q", want, got, err, want)
		}
	}
	const hint = "types: objective, key-result, initiative, epic, feature, story, bug, support-ticket, task"
	for _, bad := range []string{"", "Task", "TASK", " task", "task ", "key_result", "keyresult", "sub-task", "todo", modelMarker} {
		got, err := ParseType(bad)
		if got != "" {
			t.Errorf("ParseType(%q) = %q, want \"\"", bad, got)
		}
		if inv := checkInvalid(t, fmt.Sprintf("ParseType(%q)", bad), err, "unknown item type"); inv.Hint != hint {
			t.Errorf("ParseType(%q) hint = %q, want %q", bad, inv.Hint, hint)
		}
	}
}

// parseCase is one input to a parse function: what it returns, or the Msg
// of the *InvalidError it refuses with.
type parseCase struct {
	in      string
	want    string
	wantMsg string
}

// runParseCases checks parse against tests, and every refusal's hint
// against hint.
func runParseCases(t *testing.T, name string, parse func(string) (string, error), hint string, tests []parseCase) {
	t.Helper()
	for _, tt := range tests {
		what := fmt.Sprintf("%s(%q)", name, tt.in)
		got, err := parse(tt.in)
		if tt.wantMsg == "" {
			if err != nil || got != tt.want {
				t.Errorf("%s = %q, %v; want %q", what, got, err, tt.want)
			}
			continue
		}
		if got != "" {
			t.Errorf("%s = %q, want \"\" with the error", what, got)
		}
		if inv := checkInvalid(t, what, err, tt.wantMsg); inv.Hint != hint {
			t.Errorf("%s hint = %q, want %q", what, inv.Hint, hint)
		}
	}
}

func TestParseState(t *testing.T) {
	t.Parallel()
	const bad = "unknown item state"
	runParseCases(t, "ParseState", func(s string) (string, error) {
		st, err := ParseState(s)
		return string(st), err
	}, "states: backlog, in-progress, in-review, done", []parseCase{
		{in: "backlog", want: "backlog"},
		{in: "in-progress", want: "in-progress"},
		{in: "in-review", want: "in-review"},
		{in: "done", want: "done"},
		{in: "", wantMsg: bad}, // callers default an empty state to backlog
		{in: "Done", wantMsg: bad},
		{in: "in_progress", wantMsg: bad},
		{in: " done", wantMsg: bad},
		{in: "closed", wantMsg: bad},
		{in: modelMarker, wantMsg: bad},
	})
	for _, st := range []State{StateBacklog, StateInProgress, StateInReview, StateDone} {
		if got, err := ParseState(string(st)); err != nil || got != st {
			t.Errorf("ParseState(%q) = %q, %v", st, got, err)
		}
	}
}

func TestParseArea(t *testing.T) {
	t.Parallel()
	const bad = "unknown area"
	runParseCases(t, "ParseArea", func(s string) (string, error) {
		a, err := ParseArea(s)
		return string(a), err
	}, "areas: work, personal; leave it empty to inherit the parent's", []parseCase{
		{in: "", want: ""}, // inherits
		{in: "work", want: "work"},
		{in: "personal", want: "personal"},
		{in: "Work", wantMsg: bad},
		{in: "home", wantMsg: bad},
		{in: "work ", wantMsg: bad},
		{in: modelMarker, wantMsg: bad},
	})
	if AreaWork != "work" || AreaPersonal != "personal" {
		t.Errorf("areas = %q, %q", AreaWork, AreaPersonal)
	}
}

func TestParsePriority(t *testing.T) {
	t.Parallel()
	const bad = "unknown priority"
	runParseCases(t, "ParsePriority", func(s string) (string, error) {
		p, err := ParsePriority(s)
		return string(p), err
	}, "priorities: p0, p1, p2, p3; leave it empty to inherit the parent's", []parseCase{
		{in: "", want: ""}, // inherits
		{in: "p0", want: "p0"},
		{in: "p1", want: "p1"},
		{in: "p2", want: "p2"},
		{in: "p3", want: "p3"},
		{in: "P1", wantMsg: bad},
		{in: "p4", wantMsg: bad},
		{in: "1", wantMsg: bad},
		{in: "high", wantMsg: bad},
		{in: modelMarker, wantMsg: bad},
	})
	if DefaultPriority != PriorityP2 || PriorityP2 != "p2" {
		t.Errorf("DefaultPriority = %q, want p2", DefaultPriority)
	}
	if PriorityP0 != "p0" || PriorityP1 != "p1" || PriorityP3 != "p3" {
		t.Errorf("priorities = %q, %q, %q", PriorityP0, PriorityP1, PriorityP3)
	}
}

func TestParseDate(t *testing.T) {
	t.Parallel()
	const bad = "the date is not a real YYYY-MM-DD date"
	runParseCases(t, "ParseDate", ParseDate, "dates are YYYY-MM-DD, such as 2026-11-30", []parseCase{
		{in: "", want: ""}, // no date
		{in: "2026-11-30", want: "2026-11-30"},
		{in: "2024-02-29", want: "2024-02-29"}, // leap day
		{in: "2026-12-31", want: "2026-12-31"},
		{in: "2026-02-29", wantMsg: bad}, // not a leap year
		{in: "2026-02-30", wantMsg: bad},
		{in: "2026-13-01", wantMsg: bad},
		{in: "2026-00-10", wantMsg: bad},
		{in: "2026-1-02", wantMsg: bad},
		{in: "26-01-02", wantMsg: bad},
		{in: "2026/01/02", wantMsg: bad},
		{in: " 2026-01-02", wantMsg: bad},
		{in: "2026-01-02 ", wantMsg: bad},
		{in: "2026-01-02T00:00:00Z", wantMsg: bad},
		{in: "tomorrow", wantMsg: bad},
		{in: modelMarker, wantMsg: bad},
	})
}

func TestActorString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a    Actor
		want string
	}{
		{Actor{UID: 1000, Client: "cli"}, "1000/cli"},
		{Actor{UID: 0, Client: "daemon"}, "0/daemon"},
		{Actor{UID: 4294967295, Client: "mcp"}, "4294967295/mcp"},
		{Actor{UID: 501}, "501/"},
	}
	for _, tt := range tests {
		if got := tt.a.String(); got != tt.want {
			t.Errorf("%+v.String() = %q, want %q", tt.a, got, tt.want)
		}
	}
}

// The ops are stored in item events and read by the Lathe sync, so their
// values never change.
func TestOps(t *testing.T) {
	t.Parallel()
	tests := []struct{ got, want string }{
		{OpCreated, "created"},
		{OpUpdated, "updated"},
		{OpMoved, "moved"},
		{OpDeleted, "deleted"},
		{OpRestored, "restored"},
		{OpLinked, "linked"},
		{OpUnlinked, "unlinked"},
		{OpCriteria, "criteria"},
		{OpBlockers, "blockers"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("op = %q, want %q", tt.got, tt.want)
		}
	}
}
