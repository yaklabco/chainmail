package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/rules"
)

// ruleTOML writes body as a rule file for the test and returns its path.
func ruleTOML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rule.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ruleFileBody is a rule file that sets some keys, priority 0 among them,
// and two actions.
const ruleFileBody = `name = "receipts"
query = "from:stripe.com"
on_new = true
priority = 0

[[actions]]
kind = "label"
label = "Receipts"

[[actions]]
kind = "forward"
to = ["books@example.com", "me@example.com"]
note = "for the books"
`

// ruleFileDefinition is the definition ruleFileBody gives: priority set to
// 0, enabled left to the daemon.
func ruleFileDefinition() *chainmailv1.RuleDefinition {
	return &chainmailv1.RuleDefinition{
		Name: "receipts", Query: "from:stripe.com", OnNew: true, Priority: proto.Int32(0),
		Actions: []*chainmailv1.RuleAction{
			{Kind: "label", Label: "Receipts"},
			{Kind: "forward", To: []string{"books@example.com", "me@example.com"}, Note: "for the books"},
		},
	}
}

// ruleEchoFake answers every rule write with ruleFixture.
func ruleEchoFake() *fakeRules {
	return &fakeRules{
		create: func(*chainmailv1.CreateRuleRequest) (*chainmailv1.CreateRuleResponse, error) {
			return &chainmailv1.CreateRuleResponse{Rule: ruleFixture()}, nil
		},
		update: func(*chainmailv1.UpdateRuleRequest) (*chainmailv1.UpdateRuleResponse, error) {
			return &chainmailv1.UpdateRuleResponse{Rule: ruleFixture()}, nil
		},
		del: func(*chainmailv1.DeleteRuleRequest) (*chainmailv1.DeleteRuleResponse, error) {
			return &chainmailv1.DeleteRuleResponse{Rule: ruleFixture()}, nil
		},
		restore: func(*chainmailv1.RestoreRuleRequest) (*chainmailv1.RestoreRuleResponse, error) {
			return &chainmailv1.RestoreRuleResponse{Rule: ruleFixture()}, nil
		},
	}
}

// testRuleTable runs each case against the fake newFake returns, with
// table output, and checks that it printed exactly the case's out.
func testRuleTable(t *testing.T, newFake func() *fakeRules, cases []ruleCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"--socket", startFakeRules(t, newFake()), "--output", "table", "rule"}, tt.args...)
			got := runCLI(t, Env{}, args...)
			if got.code != 0 {
				t.Fatalf("rule %v: %+v", tt.args, got)
			}
			if got.stdout != tt.out {
				t.Errorf("stdout = %q, want %q", got.stdout, tt.out)
			}
		})
	}
}

func TestParseAction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want *chainmailv1.RuleAction
		err  string
	}{
		{in: "label=Receipts", want: &chainmailv1.RuleAction{Kind: "label", Label: "Receipts"}},
		{in: "unlabel=To do", want: &chainmailv1.RuleAction{Kind: "unlabel", Label: "To do"}},
		{in: "copy=Archive/2026", want: &chainmailv1.RuleAction{Kind: "copy", Folder: "Archive/2026"}},
		{in: "move=Receipts", want: &chainmailv1.RuleAction{Kind: "move", Folder: "Receipts"}},
		{in: "link_item=it_admin_work", want: &chainmailv1.RuleAction{Kind: "link_item", ItemId: "it_admin_work"}},
		{in: "link_item=it_admin_work,anchor", want: &chainmailv1.RuleAction{Kind: "link_item", ItemId: "it_admin_work", Anchor: true}},
		{in: "forward=a@example.com", want: &chainmailv1.RuleAction{Kind: "forward", To: []string{"a@example.com"}}},
		{in: "forward=a@example.com, b@example.com", want: &chainmailv1.RuleAction{Kind: "forward", To: []string{"a@example.com", "b@example.com"}}},
		{in: "flag", want: &chainmailv1.RuleAction{Kind: "flag"}},
		{in: "unflag", want: &chainmailv1.RuleAction{Kind: "unflag"}},
		{in: "read", want: &chainmailv1.RuleAction{Kind: "read"}},
		{in: "unread", want: &chainmailv1.RuleAction{Kind: "unread"}},
		{in: "archive", want: &chainmailv1.RuleAction{Kind: "archive"}},
		{in: "trash", want: &chainmailv1.RuleAction{Kind: "trash"}},
		{in: "bogus", err: "unknown --action kind: want label=NAME"},
		{in: "", err: "unknown --action kind: want label=NAME"},
		{in: "label", err: "--action label needs a value: label=NAME"},
		{in: "label=", err: "--action label needs a value: label=NAME"},
		{in: "move=", err: "--action move needs a value: move=FOLDER"},
		{in: "archive=Old", err: "--action archive takes no value"},
		{in: "link_item=,anchor", err: "--action link_item needs a value: link_item=ID or link_item=ID,anchor"},
		{in: "link_item=it_x,later", err: "--action link_item takes only ,anchor after the item ID"},
		{in: "forward=a@example.com,,b@example.com", err: "--action forward has an empty address"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseAction(tt.in)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("parseAction(%q) = %v, %v; want an error with %q", tt.in, got, err, tt.err)
				}
				return
			}
			if err != nil || !proto.Equal(got, tt.want) {
				t.Errorf("parseAction(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
			}
		})
	}
}

// TestRuleActionKindsMatchTheModel keeps --action's completion and parser
// in step with the rules model: the same kinds in the same order, a kind
// that takes a value refused without one, and the rest refused with one.
func TestRuleActionKindsMatchTheModel(t *testing.T) {
	t.Parallel()
	model := []rules.ActionKind{
		rules.ActLabel, rules.ActUnlabel, rules.ActCopy, rules.ActFlag, rules.ActUnflag, rules.ActRead,
		rules.ActUnread, rules.ActLinkItem, rules.ActForward, rules.ActMove, rules.ActArchive, rules.ActTrash,
	}
	var want []string
	for _, k := range model {
		if !k.Valid() {
			t.Errorf("rules.ActionKind %q is not Valid", k)
		}
		want = append(want, string(k))
	}
	var kinds []string
	for _, choice := range values(ruleActionChoices) {
		kind, takesValue := strings.CutSuffix(choice, "=")
		kinds = append(kinds, kind)
		good, bad := kind, kind+"=x"
		if takesValue {
			good, bad = kind+"=x", kind
		}
		if _, err := parseAction(good); err != nil {
			t.Errorf("--action %s: %v", good, err)
		}
		if _, err := parseAction(bad); err == nil {
			t.Errorf("--action %s parsed, want an error", bad)
		}
	}
	if !slices.Equal(kinds, want) {
		t.Errorf("--action completion offers %q, want %q", kinds, want)
	}
}

// TestRuleFileMatchesTheProto keeps the rule file's keys, and so the
// UpdateRule mask paths rule edit sends, the RuleDefinition field names,
// in the proto's order.
func TestRuleFileMatchesTheProto(t *testing.T) {
	t.Parallel()
	fields := (&chainmailv1.RuleDefinition{}).ProtoReflect().Descriptor().Fields()
	var names []string
	for i := range fields.Len() {
		names = append(names, string(fields.Get(i).Name()))
	}
	if !slices.Equal(names, ruleMaskOrder) {
		t.Errorf("ruleMaskOrder = %q, want the RuleDefinition fields %q", ruleMaskOrder, names)
	}
}

func TestRuleCreateRequestsAndUsage(t *testing.T) {
	t.Parallel()
	full := ruleTOML(t, ruleFileBody)
	notTOML := ruleTOML(t, "name = \n")
	unknownKey := ruleTOML(t, "name = 'x'\ncolor = 'red'\n")
	unknownActionKey := ruleTOML(t, "name = 'x'\n\n[[actions]]\nkind = 'label'\nlabels = 'Receipts'\n")
	badType := ruleTOML(t, "priority = 'high'\n")
	testRuleRequests(t, []ruleCase{
		{
			name: "create",
			args: []string{"create", "--name", "receipts", "--query", "from:stripe.com", "--action", "label=Receipts", "--action", "archive"},
			want: &chainmailv1.CreateRuleRequest{Rule: &chainmailv1.RuleDefinition{
				Name: "receipts", Query: "from:stripe.com",
				Actions: []*chainmailv1.RuleAction{{Kind: "label", Label: "Receipts"}, {Kind: "archive"}},
			}},
		},
		{
			name: "create every flag",
			args: []string{
				"create", "--name", "receipts", "--description", "Stripe receipts", "--account", "gmail", "--account", "gwork",
				"--query", "from:stripe.com", "--on-new", "--every", "6h", "--priority", "50", "--stop", "--max-per-run", "100", "--disabled",
				"--action", "forward=books@example.com,me@example.com", "--note", "for the books",
				"--action", "link_item=it_admin_work,anchor", "--action", "archive",
			},
			want: &chainmailv1.CreateRuleRequest{Rule: &chainmailv1.RuleDefinition{
				Name: "receipts", Description: "Stripe receipts", Accounts: []string{"gmail", "gwork"}, Query: "from:stripe.com",
				OnNew: true, Schedule: "every:6h", Priority: proto.Int32(50), Stop: true, MaxPerRun: 100, Enabled: proto.Bool(false),
				Actions: []*chainmailv1.RuleAction{
					{Kind: "forward", To: []string{"books@example.com", "me@example.com"}, Note: "for the books"},
					{Kind: "link_item", ItemId: "it_admin_work", Anchor: true},
					{Kind: "archive"},
				},
			}},
		},
		{
			name: "create at a time of day",
			args: []string{"create", "--name", "morning", "--query", "in:inbox", "--at", "07:00", "--action", "read"},
			want: &chainmailv1.CreateRuleRequest{Rule: &chainmailv1.RuleDefinition{
				Name: "morning", Query: "in:inbox", Schedule: "at:07:00", Actions: []*chainmailv1.RuleAction{{Kind: "read"}},
			}},
		},
		{
			name: "create with priority 0",
			args: []string{"create", "--name", "first", "--query", "in:inbox", "--priority", "0", "--action", "flag"},
			want: &chainmailv1.CreateRuleRequest{Rule: &chainmailv1.RuleDefinition{
				Name: "first", Query: "in:inbox", Priority: proto.Int32(0), Actions: []*chainmailv1.RuleAction{{Kind: "flag"}},
			}},
		},
		{name: "create from a file", args: []string{"create", "-f", full}, want: &chainmailv1.CreateRuleRequest{Rule: ruleFileDefinition()}},
		{name: "create from a file, long flag", args: []string{"create", "--file", full}, want: &chainmailv1.CreateRuleRequest{Rule: ruleFileDefinition()}},
	})
	testRuleUsage(t, []ruleCase{
		{name: "create with nothing", args: []string{"create"}, usage: "missing --name, --query, --action"},
		{name: "create without a query", args: []string{"create", "--name", "x", "--action", "archive"}, usage: "missing --query"},
		{name: "create without an action", args: []string{"create", "--name", "x", "--query", "in:inbox"}, usage: "missing --action"},
		{
			name:  "create on two schedules",
			args:  []string{"create", "--name", "x", "--query", "in:inbox", "--action", "archive", "--every", "6h", "--at", "07:00"},
			usage: "--every and --at cannot be used together",
		},
		{name: "create with an unknown action", args: []string{"create", "--name", "x", "--query", "q", "--action", "bogus"}, usage: "unknown --action"},
		{name: "create with a bare label", args: []string{"create", "--name", "x", "--query", "q", "--action", "label"}, usage: "--action label needs a value"},
		{
			name:  "create with a note and no forward",
			args:  []string{"create", "--name", "x", "--query", "q", "--action", "archive", "--note", "hi"},
			usage: "--note applies to a forward action",
		},
		{name: "create with an argument", args: []string{"create", "extra"}, usage: "accepts 0 arg(s)"},
		{name: "create with a bad priority", args: []string{"create", "--priority", "high"}, usage: "--priority"},
		{name: "create from a file and a flag", args: []string{"create", "-f", full, "--name", "x"}, usage: "--file and --name cannot be used together"},
		{name: "create from a file, disabled", args: []string{"create", "-f", full, "--disabled"}, usage: "--file and --disabled cannot be used together"},
		{name: "create from a missing file", args: []string{"create", "-f", "/nonexistent/rule.toml"}, usage: "no such file"},
		{name: "create from a file that is not TOML", args: []string{"create", "-f", notTOML}, usage: "is not a rule file"},
		{name: "create from a file with an unknown key", args: []string{"create", "-f", unknownKey}, usage: "is not a rule file: it holds a key a rule file does not have"},
		{name: "create from a file with an unknown action key", args: []string{"create", "-f", unknownActionKey}, usage: "is not a rule file"},
		{name: "create from a file with a bad type", args: []string{"create", "-f", badType}, usage: "is not a rule file"},
	})
}

// What rule show --toml prints, rule create -f reads back as the rule's
// definition, every field set.
func TestRuleFileRoundTrip(t *testing.T) {
	t.Parallel()
	get := &fakeRules{get: func(*chainmailv1.GetRuleRequest) (*chainmailv1.GetRuleResponse, error) { return ruleShowFixture(), nil }}
	shown := runCLI(t, Env{}, "--socket", startFakeRules(t, get), "rule", "show", "receipts", "--toml")
	if shown.code != 0 {
		t.Fatalf("rule show --toml: %+v", shown)
	}
	f := &fakeRules{}
	got := runCLI(t, Env{}, "--socket", startFakeRules(t, f), "--output", "json", "rule", "create", "-f", ruleTOML(t, shown.stdout))
	if got.code != 0 {
		t.Fatalf("rule create -f: %+v", got)
	}
	want := &chainmailv1.CreateRuleRequest{Rule: &chainmailv1.RuleDefinition{
		Name: "receipts", Description: "Stripe receipts.\n\nFile them under \x1b[31mAdmin.\n",
		Accounts: []string{"gmail", "gwork"}, Query: "from:stripe.com subject:receipt", OnNew: true, Schedule: "every:6h",
		Priority: proto.Int32(50), Actions: ruleFixture().GetActions(), MaxPerRun: 500, Enabled: proto.Bool(true),
	}}
	if reqs := f.requests(); !slices.EqualFunc(reqs, []proto.Message{want}, proto.Equal) {
		t.Errorf("requests = %v, want %v", reqs, want)
	}
}

func TestRuleCreateTable(t *testing.T) {
	t.Parallel()
	testRuleTable(t, ruleEchoFake, []ruleCase{
		{
			name: "create",
			args: []string{"create", "--name", "receipts", "--query", "from:stripe.com", "--action", "archive"},
			out:  "created rule receipts " + ruleID + ", revision 3\n",
		},
	})
}

func TestCompleteRuleCreate(t *testing.T) {
	t.Parallel()
	env := sockEnv(startFakeItems(t, completionItems()))
	const noSpace = cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	tests := []struct {
		name string
		args []string
		want []string
		dir  cobra.ShellCompDirective
	}{
		{
			name: "actions",
			args: []string{"rule", "create", "--action", ""},
			want: []string{"label=", "unlabel=", "copy=", "flag", "unflag", "read", "unread", "link_item=", "forward=", "move=", "archive", "trash"},
			dir:  noSpace,
		},
		{name: "action prefix", args: []string{"rule", "create", "--action", "un"}, want: []string{"unlabel=", "unflag", "unread"}, dir: noSpace},
		{name: "item to link", args: []string{"rule", "create", "--action", "link_item=it_t"}, want: []string{"link_item=it_task1"}, dir: cobra.ShellCompDirectiveNoFileComp},
		{name: "label value", args: []string{"rule", "create", "--action", "label=R"}, want: []string{}, dir: cobra.ShellCompDirectiveNoFileComp},
		{name: "every", args: []string{"rule", "create", "--every", ""}, want: []string{"15m", "1h", "6h", "1d", "7d"}, dir: cobra.ShellCompDirectiveNoFileComp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cands, d := completeArgs(t, env, tt.args...)
			if got := values(cands); !slices.Equal(got, tt.want) {
				t.Errorf("candidates %q, want %q", got, tt.want)
			}
			if d != tt.dir {
				t.Errorf("directive %d, want %d", d, tt.dir)
			}
		})
	}
}

// An --action the parser does not recognize may carry a forward address,
// a folder or a label: the usage error names the grammar but never echoes
// the value back.
func TestRuleCreateUnknownActionKeepsItsValueOffStderr(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"fwd=private@example.com", "mvoe=Secret Folder", "private@example.com"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{}, "--socket", "/nonexistent.sock", "rule", "create", "--name", "x", "--query", "q", "--action", action)
			if got.code != api.ExitUsage || errorReason(t, got.stderr) != api.ReasonUsage {
				t.Fatalf("rule create --action %q: %+v, want a usage error", action, got)
			}
			if !strings.Contains(got.stderr, "unknown --action kind") {
				t.Errorf("stderr = %s, want it to name the unknown kind", got.stderr)
			}
			for _, secret := range []string{"private@example.com", "Secret Folder", "fwd", "mvoe"} {
				if strings.Contains(got.stderr, secret) {
					t.Errorf("stderr = %s, want no %q from the --action value", got.stderr, secret)
				}
			}
		})
	}
}

// TestRuleFileErrorsKeepItsTextOffStderr checks that a rule file that
// does not parse, or holds a key a rule file does not have, is a usage
// error that quotes nothing from the file: keys and decoder messages can
// carry rule text, such as a forward address.
func TestRuleFileErrorsKeepItsTextOffStderr(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, body, want string
	}{
		{name: "quoted unknown key", body: "\"private@example.com\" = \"x\"\n", want: "holds a key a rule file does not have"},
		{name: "repeated table", body: "[\"private@example.com\"]\n[\"private@example.com\"]\n", want: "is not valid TOML"},
		{name: "bad value type", body: "name = \"x\"\n[[actions]]\nkind = \"forward\"\nto = \"private@example.com\"\nnote = 3\n", want: "has the wrong type"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "rule.toml")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			got := runCLI(t, Env{}, "--socket", "/nonexistent.sock", "rule", "create", "-f", path)
			if got.code != api.ExitUsage || errorReason(t, got.stderr) != api.ReasonUsage {
				t.Fatalf("rule create -f: %+v, want a usage error", got)
			}
			if !strings.Contains(got.stderr, "is not a rule file") || !strings.Contains(got.stderr, tt.want) {
				t.Errorf("stderr = %s, want a fixed rule file error saying %q", got.stderr, tt.want)
			}
			if strings.Contains(got.stderr, "private@example.com") {
				t.Errorf("stderr = %s, want nothing quoted from the file", got.stderr)
			}
		})
	}
}
