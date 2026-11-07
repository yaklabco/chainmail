package rules

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/query"
)

// testEnv has two Google accounts and one other, and the policy default of
// ten recipients.
var testEnv = Env{
	Accounts: map[string]AccountInfo{
		"gmail":    {Gmail: true},
		"personal": {Gmail: true},
		"yaklab":   {},
	},
	MaxRecipients: 10,
}

// googleEnv has only Google accounts.
var googleEnv = Env{
	Accounts:      map[string]AccountInfo{"gmail": {Gmail: true}, "personal": {Gmail: true}},
	MaxRecipients: 10,
}

// validDef is a definition every check accepts in testEnv. Each test row
// changes one thing.
func validDef() Definition {
	d := Defaults()
	d.Name = "stripe-receipts"
	d.Description = "Stripe receipts " + ruleMarker
	d.Accounts = []string{"gmail"}
	d.Query = "from:stripe.com " + ruleMarker
	d.OnNew = true
	d.Actions = []Action{
		{Kind: ActLabel, Label: "Receipts"},
		{Kind: ActLinkItem, ItemID: "it_admin_work", Anchor: true},
		{Kind: ActArchive},
	}
	return d.Normalize()
}

func TestValidateAccepts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  Env
		edit func(*Definition)
	}{
		{"the base definition", testEnv, func(*Definition) {}},
		{"every account, all Google", googleEnv, func(d *Definition) { d.Accounts = nil }},
		{"empty query", testEnv, func(d *Definition) { d.Query = "" }},
		{"query naming a known account", testEnv, func(d *Definition) { d.Query = "account:yaklab in:inbox is:read older_than:30d" }},
		{"no trigger, manual runs only", testEnv, func(d *Definition) { d.OnNew = false }},
		{"every schedule at the lower bound", testEnv, func(d *Definition) { d.Schedule = Schedule{Kind: ScheduleEvery, Every: MinEvery} }},
		{"every schedule at the upper bound", testEnv, func(d *Definition) { d.Schedule = Schedule{Kind: ScheduleEvery, Every: MaxEvery} }},
		{"at schedule", testEnv, func(d *Definition) { d.Schedule = Schedule{Kind: ScheduleAt, At: ClockTime{Hour: 23, Minute: 59}} }},
		{"description of 1000 characters", testEnv, func(d *Definition) { d.Description = strings.Repeat("é", MaxDescription) }},
		{"sixteen actions", testEnv, func(d *Definition) {
			d.Actions = nil
			for range MaxActions - 1 {
				d.Actions = append(d.Actions, Action{Kind: ActRead})
			}
			d.Actions = append(d.Actions, Action{Kind: ActTrash})
		}},
		{"move last, with copy and forward", testEnv, func(d *Definition) {
			d.Accounts = []string{"yaklab"}
			d.Actions = []Action{
				{Kind: ActCopy, Folder: "Receipts/Copies"},
				{Kind: ActForward, To: []string{"a@example.com"}, Note: strings.Repeat("n", MaxNote)},
				{Kind: ActFlag},
				{Kind: ActUnflag},
				{Kind: ActRead},
				{Kind: ActUnread},
				{Kind: ActLinkItem, ItemID: "it_01jcz8v2kq4f9w3m7xg5t6h8ya"},
				{Kind: ActMove, Folder: strings.Repeat("f", MaxFolderName)},
			}
		}},
		{"unlabel on Google", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActUnlabel, Label: "Later"}} }},
		{"forward to max_recipients addresses", testEnv, func(d *Definition) {
			to := make([]string, 10)
			for i := range to {
				to[i] = "r" + string(rune('a'+i)) + "@example.com"
			}
			d.Actions = []Action{{Kind: ActForward, To: to}}
		}},
		{"priority bounds", testEnv, func(d *Definition) { d.Priority = MinPriority }},
		{"priority zero", testEnv, func(d *Definition) { d.Priority = 0 }},
		{"priority upper bound", testEnv, func(d *Definition) { d.Priority = MaxPriority }},
		{"max_per_run bounds", testEnv, func(d *Definition) { d.MaxPerRun = 1 }},
		{"max_per_run upper bound", testEnv, func(d *Definition) { d.MaxPerRun = MaxMaxPerRun }},
		{"disabled", testEnv, func(d *Definition) { d.Enabled = false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := validDef()
			tt.edit(&d)
			if err := d.Normalize().Validate(tt.env); err != nil {
				t.Errorf("Validate = %v, want nil", err)
			}
		})
	}
}

// Each row breaks one check, in the order Validate runs them, and wants
// that check's fixed message.
func TestValidateRefuses(t *testing.T) {
	t.Parallel()
	forwardTo := func(to ...string) []Action { return []Action{{Kind: ActForward, To: to}} }
	tests := []struct {
		name string
		env  Env
		edit func(*Definition)
		msg  string
	}{
		{"empty name", testEnv, func(d *Definition) { d.Name = "" }, "the rule name is not valid"},
		{"name with rule text", testEnv, func(d *Definition) { d.Name = ruleMarker + "_x" }, "the rule name is not valid"},
		{"name like an ID", testEnv, func(d *Definition) { d.Name = "ru_abc" }, "the rule name is not valid"},
		{"description not UTF-8", testEnv, func(d *Definition) { d.Description = ruleMarker + "\xff" }, "the description is not valid UTF-8"},
		{"description too long", testEnv, func(d *Definition) { d.Description = ruleMarker + strings.Repeat("d", MaxDescription) }, "the description is longer than 1000 characters"},
		{"unknown account", testEnv, func(d *Definition) { d.Accounts = []string{"gmail", "zq-rule-text-marker"} }, "the rule names an account that is not configured"},
		{"query account unknown", testEnv, func(d *Definition) { d.Query = "account:zq-rule-text-marker from:x" }, "the query names an account that is not configured"},
		{"every too short", testEnv, func(d *Definition) { d.Schedule = Schedule{Kind: ScheduleEvery, Every: MinEvery - time.Second} }, "the every: duration is shorter than 15 minutes"},
		{"every too long", testEnv, func(d *Definition) { d.Schedule = Schedule{Kind: ScheduleEvery, Every: MaxEvery + time.Second} }, "the every: duration is longer than 30 days"},
		{"at out of range", testEnv, func(d *Definition) { d.Schedule = Schedule{Kind: ScheduleAt, At: ClockTime{Hour: 24}} }, "the at: time is outside 00:00 to 23:59"},
		{"unknown schedule kind", testEnv, func(d *Definition) { d.Schedule = Schedule{Kind: ruleMarker} }, "the schedule kind is not known"},
		{"no actions", testEnv, func(d *Definition) { d.Actions = nil }, "the rule has no actions"},
		{"seventeen actions", testEnv, func(d *Definition) {
			d.Actions = nil
			for range MaxActions + 1 {
				d.Actions = append(d.Actions, Action{Kind: ActRead})
			}
		}, "the rule has more than 16 actions"},
		{"unknown action kind", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ruleMarker}} }, "an action kind is not known"},
		{"stray label on archive", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActArchive, Label: ruleMarker}} }, "an action has an argument its kind does not take"},
		{"stray folder on label", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActLabel, Label: "x", Folder: ruleMarker}} }, "an action has an argument its kind does not take"},
		{"stray anchor on move", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActMove, Folder: "x", Anchor: true}} }, "an action has an argument its kind does not take"},
		{"stray recipients on flag", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActFlag, To: []string{ruleMarker + "@example.com"}}} }, "an action has an argument its kind does not take"},
		{"stray note on link_item", testEnv, func(d *Definition) {
			d.Actions = []Action{{Kind: ActLinkItem, ItemID: "it_admin_work", Note: ruleMarker}}
		}, "an action has an argument its kind does not take"},
		{"empty label", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActLabel}} }, "a label name is not valid"},
		{"label too long", testEnv, func(d *Definition) {
			d.Actions = []Action{{Kind: ActLabel, Label: ruleMarker + strings.Repeat("l", MaxFolderName)}}
		}, "a label name is not valid"},
		{"label with a control character", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActUnlabel, Label: ruleMarker + "\n"}} }, "a label name is not valid"},
		{"empty copy folder", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActCopy}} }, "a folder name is not valid"},
		{"move folder not UTF-8", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActMove, Folder: ruleMarker + "\xff"}} }, "a folder name is not valid"},
		{"move folder with a tab", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActMove, Folder: ruleMarker + "\t"}} }, "a folder name is not valid"},
		{"link_item without an item", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActLinkItem}} }, "a link_item action needs a valid item ID"},
		{"link_item with a rule ID", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActLinkItem, ItemID: "ru_abc"}} }, "a link_item action needs a valid item ID"},
		{"link_item with text", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActLinkItem, ItemID: ruleMarker}} }, "a link_item action needs a valid item ID"},
		{"note not UTF-8", testEnv, func(d *Definition) {
			d.Actions = []Action{{Kind: ActForward, To: []string{"a@example.com"}, Note: ruleMarker + "\xff"}}
		}, "a forward note is not valid UTF-8"},
		{"note too long", testEnv, func(d *Definition) {
			d.Actions = []Action{{Kind: ActForward, To: []string{"a@example.com"}, Note: ruleMarker + strings.Repeat("n", MaxNote)}}
		}, "a forward note is longer than 2000 characters"},
		{"two relocations", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActArchive}, {Kind: ActTrash}} }, "the rule has more than one move, archive or trash action"},
		{"move and archive with label between", testEnv, func(d *Definition) {
			d.Actions = []Action{{Kind: ActMove, Folder: "x"}, {Kind: ActLabel, Label: "y"}, {Kind: ActArchive}}
		}, "the rule has more than one move, archive or trash action"},
		{"relocation not last", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActTrash}, {Kind: ActRead}} }, "a move, archive or trash action must be the last action"},
		{"two forwards", testEnv, func(d *Definition) {
			d.Actions = []Action{{Kind: ActForward, To: []string{"a@example.com"}}, {Kind: ActForward, To: []string{"b@example.com"}}}
		}, "the rule has more than one forward action"},
		{"two copies", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActCopy, Folder: "a"}, {Kind: ActCopy, Folder: "b"}} }, "the rule has more than one copy action"},
		{"label on a non-Google account", testEnv, func(d *Definition) { d.Accounts = []string{"gmail", "yaklab"} }, "label and unlabel work only on Google accounts"},
		{"label on every account, one not Google", testEnv, func(d *Definition) { d.Accounts = nil }, "label and unlabel work only on Google accounts"},
		{"unlabel on a non-Google account", testEnv, func(d *Definition) {
			d.Accounts = []string{"yaklab"}
			d.Actions = []Action{{Kind: ActUnlabel, Label: "x"}}
		}, "label and unlabel work only on Google accounts"},
		{"forward with no recipients", testEnv, func(d *Definition) { d.Actions = forwardTo() }, "a forward action has no recipients"},
		{"forward over max_recipients", Env{Accounts: testEnv.Accounts, MaxRecipients: 2}, func(d *Definition) {
			d.Actions = forwardTo("a@example.com", "b@example.com", "c@example.com")
		}, "a forward action has more recipients than the send policy allows"},
		{"forward with zero max_recipients reads as one", Env{Accounts: testEnv.Accounts}, func(d *Definition) {
			d.Actions = forwardTo("a@example.com", "b@example.com")
		}, "a forward action has more recipients than the send policy allows"},
		{"forward with a display name", testEnv, func(d *Definition) {
			d.Actions = forwardTo("a@example.com", "Rule Text <"+ruleMarker+"@example.com>")
		}, "a forward recipient is not a plain address"},
		{"forward with no domain", testEnv, func(d *Definition) { d.Actions = forwardTo(ruleMarker) }, "a forward recipient is not a plain address"},
		{"forward with a quoted local part", testEnv, func(d *Definition) { d.Actions = forwardTo(`"` + ruleMarker + `"@example.com`) }, "a forward recipient is not a plain address"},
		{"priority too low", testEnv, func(d *Definition) { d.Priority = MinPriority - 1 }, "the priority is outside -1000000 to 1000000"},
		{"priority too high", testEnv, func(d *Definition) { d.Priority = MaxPriority + 1 }, "the priority is outside -1000000 to 1000000"},
		{"max_per_run zero", testEnv, func(d *Definition) { d.MaxPerRun = 0 }, "max_per_run is outside 1 to 5000"},
		{"max_per_run too high", testEnv, func(d *Definition) { d.MaxPerRun = MaxMaxPerRun + 1 }, "max_per_run is outside 1 to 5000"},
		// When two checks fail, the earlier one in the documented order wins.
		{"name before actions", testEnv, func(d *Definition) {
			d.Name = ""
			d.Actions = nil
		}, "the rule name is not valid"},
		{"schedule before actions", testEnv, func(d *Definition) {
			d.Schedule = Schedule{Kind: ScheduleEvery, Every: time.Minute}
			d.Actions = nil
		}, "the every: duration is shorter than 15 minutes"},
		{"arguments before order", testEnv, func(d *Definition) { d.Actions = []Action{{Kind: ActTrash}, {Kind: ActLabel}} }, "a label name is not valid"},
		{"order before labels", testEnv, func(d *Definition) {
			d.Accounts = []string{"yaklab"}
			d.Actions = []Action{{Kind: ActArchive}, {Kind: ActLabel, Label: "x"}}
		}, "a move, archive or trash action must be the last action"},
		{"labels before recipients", testEnv, func(d *Definition) {
			d.Accounts = []string{"yaklab"}
			d.Actions = []Action{{Kind: ActLabel, Label: "x"}, {Kind: ActForward}}
		}, "label and unlabel work only on Google accounts"},
		{"recipients before priority", testEnv, func(d *Definition) {
			d.Actions = forwardTo()
			d.Priority = MaxPriority + 1
		}, "a forward action has no recipients"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := validDef()
			tt.edit(&d)
			wantInvalid(t, tt.name, d.Validate(tt.env), tt.msg)
		})
	}
}

// A query that does not parse reports the column and the parser's
// expectation, never the query or the parser's message, which can quote
// the query.
func TestValidateQuerySyntax(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, query, msg, hint string
	}{
		{"unknown operator", ruleMarker + ":x", "the query does not parse at column 1", "expected one of " + operatorNames()},
		{"bad is value", "from:a is:" + ruleMarker, "the query does not parse at column 11", "expected is:unread, is:read or is:starred"},
		{"unclosed quote", `subject:"` + ruleMarker, "the query does not parse at column 9", `expected a closing "`},
		{"two in terms", "in:inbox in:" + ruleMarker, "the query does not parse at column 10", "expected a single in: term"},
		{"lone minus", "-", "the query does not parse at column 1", "expected a term right after -, such as -from:alex"},
		{"too long", strings.Repeat("a", 4097), "the query does not parse at column 1", "expected at most 4096 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := validDef()
			d.Query = tt.query
			if hint := wantInvalid(t, tt.name, d.Validate(testEnv), tt.msg); hint != tt.hint {
				t.Errorf("Hint = %q, want %q", hint, tt.hint)
			}
		})
	}
}

// operatorNames is the operator list the query parser expects, as its
// error text writes it.
func operatorNames() string {
	names := make([]string, len(query.Operators))
	for i, f := range query.Operators {
		names[i] = string(f) + ":"
	}
	return strings.Join(names, " ")
}

func TestCoveredAccounts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		accounts []string
		env      Env
		want     []string
	}{
		{"explicit accounts", []string{"yaklab", "gmail"}, testEnv, []string{"gmail", "yaklab"}},
		{"explicit duplicates", []string{"gmail", "gmail"}, testEnv, []string{"gmail"}},
		{"every account, sorted", nil, testEnv, []string{"gmail", "personal", "yaklab"}},
		{"every account of an empty env", nil, Env{}, []string{}},
	}
	for _, tt := range tests {
		got := Definition{Accounts: tt.accounts}.CoveredAccounts(tt.env)
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s: CoveredAccounts = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// A parser error with no column, or with no expectation, still gets fixed
// text.
func TestQuerySyntaxError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		in        *query.SyntaxError
		msg, hint string
	}{
		{"no column", &query.SyntaxError{Msg: ruleMarker, Expected: "a term"}, "the query does not parse", "expected a term"},
		{"no expectation", &query.SyntaxError{Pos: 3, Msg: ruleMarker}, "the query does not parse at column 3", queryHint},
		{"expectation cut at the semicolon", &query.SyntaxError{Pos: 2, Msg: ruleMarker, Expected: "one of from:; quote it: " + ruleMarker}, "the query does not parse at column 2", "expected one of from:"},
	}
	for _, tt := range tests {
		got := querySyntaxError(tt.in)
		if got.Msg != tt.msg || got.Hint != tt.hint {
			t.Errorf("%s: querySyntaxError = %q, %q; want %q, %q", tt.name, got.Msg, got.Hint, tt.msg, tt.hint)
		}
	}
}
