package mcpserver

import (
	"slices"

	"google.golang.org/protobuf/types/known/fieldmaskpb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
)

// The bounds internal/rules sets on the numbers a rule tool takes. The
// tools check them before calling the daemon, so a value never wraps when
// it narrows to int32. TestRuleToolValuesMatchTheModel ties them to the
// model.
const (
	rulePriorityBound = 1000000 // priority runs from -rulePriorityBound to rulePriorityBound
	ruleMaxPerRun     = 5000    // the most a max_per_run, or a run's max, may be
)

// ruleActionKinds are the action kinds, in the order the descriptions list them.
var ruleActionKinds = []string{"label", "unlabel", "copy", "flag", "unflag", "read", "unread", "link_item", "forward", "move", "archive", "trash"}

// ruleActionArgs are the arguments each action kind takes, the required one
// first. A kind that is not listed takes none.
var ruleActionArgs = map[string][]string{
	"label": {"label"}, "unlabel": {"label"},
	"copy": {"folder"}, "move": {"folder"},
	"link_item": {"item_id", "anchor"},
	"forward":   {"to", "note"},
}

// ruleArgNames are every action argument, in the order a refusal names them.
var ruleArgNames = []string{"label", "folder", "item_id", "anchor", "to", "note"}

// ruleTriggers are the triggers dry_run_rule takes. Empty means manual.
var ruleTriggers = []string{"", "new", "schedule", "manual"}

// ruleUpdatePaths are UpdateRule's mask paths: the RuleDefinition field
// names, in the order the mask lists them.
var ruleUpdatePaths = []string{"name", "description", "accounts", "query", "on_new", "schedule", "priority", "stop", "actions", "max_per_run", "enabled"}

// ruleClearablePaths are the paths update_rule's clear may name. A rule
// with no accounts covers every account, and one with no schedule runs
// only on new mail or through run_rule.
var ruleClearablePaths = []string{"description", "accounts", "schedule"}

// ruleActionIn is one action. kind names it; the other fields are its
// arguments, and a kind takes only its own.
type ruleActionIn struct {
	Kind   string   `json:"kind" jsonschema:"label, unlabel, copy, flag, unflag, read, unread, link_item, forward, move, archive or trash"`
	Label  string   `json:"label,omitempty" jsonschema:"label and unlabel: the Gmail label, 1 to 255 bytes"`
	Folder string   `json:"folder,omitempty" jsonschema:"copy and move: the folder, 1 to 255 bytes"`
	ItemID string   `json:"item_id,omitempty" jsonschema:"link_item: the live work item to file the message's thread under"`
	Anchor bool     `json:"anchor,omitempty" jsonschema:"link_item: make the message the link's anchor"`
	To     []string `json:"to,omitempty" jsonschema:"forward: the addresses to forward to, up to the policy's max_recipients"`
	Note   string   `json:"note,omitempty" jsonschema:"forward: a note sent with the forward, at most 2000 characters"`
}

// ruleDefinitionIn is a whole rule: create_rule's input, and dry_run_rule's
// definition.
type ruleDefinitionIn struct {
	Name        string         `json:"name" jsonschema:"lowercase letters, digits and dashes, starting with a letter or digit, up to 63 characters, such as stripe-receipts; unique among live rules"`
	Description string         `json:"description,omitempty" jsonschema:"what the rule is for, at most 1000 characters"`
	Accounts    []string       `json:"accounts,omitempty" jsonschema:"the accounts the rule covers; empty means every account"`
	Query       string         `json:"query" jsonschema:"the messages to act on, in the language search takes, such as from:stripe.com subject:receipt"`
	OnNew       bool           `json:"on_new,omitempty" jsonschema:"run the rule on new mail as the local cache takes it"`
	Schedule    string         `json:"schedule,omitempty" jsonschema:"every:<duration> from 15m to 30d, such as every:6h or every:7d, or at:HH:MM once a day; empty for none"`
	Priority    *int           `json:"priority,omitempty" jsonschema:"-1000000 to 1000000; lower runs first; default 100"`
	Stop        bool           `json:"stop,omitempty" jsonschema:"keep later rules off every message this rule matched"`
	Actions     []ruleActionIn `json:"actions" jsonschema:"1 to 16 actions, run in order on each match"`
	MaxPerRun   int            `json:"max_per_run,omitempty" jsonschema:"the most messages one run acts on per account, 1 to 5000; default 500"`
	Enabled     *bool          `json:"enabled,omitempty" jsonschema:"false saves the rule switched off; default true"`
}

type updateRuleIn struct {
	Rule             string         `json:"rule" jsonschema:"the rule's ID (ru_...) or name"`
	Name             *string        `json:"name,omitempty" jsonschema:"a new name: lowercase letters, digits and dashes, starting with a letter or digit, up to 63 characters"`
	Description      *string        `json:"description,omitempty" jsonschema:"a new description, at most 1000 characters"`
	Accounts         []string       `json:"accounts,omitempty" jsonschema:"the accounts the rule covers, replacing the old list; to cover every account, name accounts in clear"`
	Query            *string        `json:"query,omitempty" jsonschema:"a new query, in the language search takes"`
	OnNew            *bool          `json:"on_new,omitempty" jsonschema:"run the rule on new mail, or not"`
	Schedule         *string        `json:"schedule,omitempty" jsonschema:"every:<duration> from 15m to 30d, or at:HH:MM; to unschedule, name schedule in clear"`
	Priority         *int           `json:"priority,omitempty" jsonschema:"-1000000 to 1000000; lower runs first"`
	Stop             *bool          `json:"stop,omitempty" jsonschema:"keep later rules off every message this rule matched, or not"`
	Actions          []ruleActionIn `json:"actions,omitempty" jsonschema:"1 to 16 actions that replace every old one, in order"`
	MaxPerRun        *int           `json:"max_per_run,omitempty" jsonschema:"1 to 5000"`
	Enabled          *bool          `json:"enabled,omitempty" jsonschema:"switch the rule on or off; pass it alone to do only that"`
	ExpectedRevision *int64         `json:"expected_revision,omitempty" jsonschema:"the revision get_rule showed; the change is refused if the rule changed since"`
	Clear            []string       `json:"clear,omitempty" jsonschema:"fields to clear: description, accounts (the rule then covers every account) or schedule (the rule then runs only on new mail or through run_rule); a field cannot be both given and cleared"`
}

type dryRunRuleIn struct {
	Rule       string            `json:"rule,omitempty" jsonschema:"a saved rule's ID (ru_...) or name; not with definition"`
	Definition *ruleDefinitionIn `json:"definition,omitempty" jsonschema:"an unsaved rule, with create_rule's fields; not with rule"`
	Trigger    string            `json:"trigger,omitempty" jsonschema:"new tries the newest 200 cached messages per account, as for new mail; schedule and manual, the default, try the whole cache"`
	Account    string            `json:"account,omitempty" jsonschema:"only this account; empty means every account the rule covers"`
}

type runRuleIn struct {
	Rule    string `json:"rule" jsonschema:"the rule's ID (ru_...) or name"`
	Max     int    `json:"max,omitempty" jsonschema:"the most messages to act on per account, 1 to 5000; default the rule's max_per_run"`
	Account string `json:"account,omitempty" jsonschema:"only this account; empty means every account the rule covers"`
}

// ruleActions checks each action's kind and the shape of its arguments and
// builds the daemon's actions. Every refusal is an invalidArg built from in
// alone, before any daemon call. The daemon checks the rest: how many
// actions, how they combine, and the names and addresses they hold.
func ruleActions(in []ruleActionIn) ([]*chainmailv1.RuleAction, error) {
	out := make([]*chainmailv1.RuleAction, 0, len(in))
	for i, a := range in {
		n := i + 1
		if !slices.Contains(ruleActionKinds, a.Kind) {
			return nil, invalidArg("action %d: unknown kind %q; kinds are label, unlabel, copy, flag, unflag, read, unread, link_item, forward, move, archive and trash", n, a.Kind)
		}
		given := map[string]bool{
			"label": a.Label != "", "folder": a.Folder != "", "item_id": a.ItemID != "",
			"anchor": a.Anchor, "to": len(a.To) > 0, "note": a.Note != "",
		}
		takes := ruleActionArgs[a.Kind]
		if len(takes) > 0 && !given[takes[0]] {
			return nil, invalidArg("action %d (%s) needs %s", n, a.Kind, takes[0])
		}
		for _, arg := range ruleArgNames {
			if given[arg] && !slices.Contains(takes, arg) {
				return nil, invalidArg("action %d (%s) takes no %s", n, a.Kind, arg)
			}
		}
		out = append(out, &chainmailv1.RuleAction{
			Kind: a.Kind, Label: a.Label, Folder: a.Folder, ItemId: a.ItemID, Anchor: a.Anchor, To: a.To, Note: a.Note,
		})
	}
	return out, nil
}

// checkPriority refuses a priority outside the rules' range.
func checkPriority(p *int) error {
	if p != nil && (*p < -rulePriorityBound || *p > rulePriorityBound) {
		return invalidArg("priority must be -1000000 to 1000000, got %d", *p)
	}
	return nil
}

// checkMaxPerRun refuses a max_per_run outside 0 to 5000. 0 means 500.
func checkMaxPerRun(n int) error {
	if n < 0 || n > ruleMaxPerRun {
		return invalidArg("max_per_run must be 1 to 5000, or 0 for 500, got %d", n)
	}
	return nil
}

// int32Ptr narrows a checked *int, keeping nil.
func int32Ptr(p *int) *int32 {
	if p == nil {
		return nil
	}
	return new(int32(*p))
}

// boolOf is *p, or false for nil.
func boolOf(p *bool) bool {
	return p != nil && *p
}

// ruleDefinition checks in and builds the daemon's definition. The daemon
// fills an unset priority, a zero max_per_run and an unset enabled with
// their defaults.
func ruleDefinition(in ruleDefinitionIn) (*chainmailv1.RuleDefinition, error) {
	if err := checkPriority(in.Priority); err != nil {
		return nil, err
	}
	actions, err := ruleActions(in.Actions)
	if err != nil {
		return nil, err
	}
	if err := checkMaxPerRun(in.MaxPerRun); err != nil {
		return nil, err
	}
	return &chainmailv1.RuleDefinition{
		Name: in.Name, Description: in.Description, Accounts: in.Accounts, Query: in.Query, OnNew: in.OnNew,
		Schedule: in.Schedule, Priority: int32Ptr(in.Priority), Stop: in.Stop, Actions: actions,
		MaxPerRun: int32(in.MaxPerRun), Enabled: in.Enabled,
	}, nil
}

// updateRuleRequest builds the UpdateRule request. The mask names every
// field in gives and every field clear names, in ruleUpdatePaths order; a
// list counts as given when it is not empty. Every refusal is an invalidArg
// built from in alone, before any daemon call.
func updateRuleRequest(in updateRuleIn) (*chainmailv1.UpdateRuleRequest, error) {
	given := map[string]bool{
		"name": in.Name != nil, "description": in.Description != nil, "accounts": len(in.Accounts) > 0,
		"query": in.Query != nil, "on_new": in.OnNew != nil, "schedule": in.Schedule != nil,
		"priority": in.Priority != nil, "stop": in.Stop != nil, "actions": len(in.Actions) > 0,
		"max_per_run": in.MaxPerRun != nil, "enabled": in.Enabled != nil,
	}
	cleared := map[string]bool{}
	for _, p := range in.Clear {
		if !slices.Contains(ruleClearablePaths, p) {
			return nil, invalidArg("clear cannot name %q; it takes description, accounts and schedule", p)
		}
		if given[p] {
			return nil, invalidArg("%s is both given and cleared; give a value or clear it", p)
		}
		cleared[p] = true
	}
	var paths []string
	for _, p := range ruleUpdatePaths {
		if given[p] || cleared[p] {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil, invalidArg("give at least one field to change, or name one in clear")
	}
	if err := checkPriority(in.Priority); err != nil {
		return nil, err
	}
	actions, err := ruleActions(in.Actions)
	if err != nil {
		return nil, err
	}
	maxPerRun := 0
	if in.MaxPerRun != nil {
		maxPerRun = *in.MaxPerRun
	}
	if err := checkMaxPerRun(maxPerRun); err != nil {
		return nil, err
	}
	patch := &chainmailv1.RuleDefinition{
		Name: valueOf(in.Name), Description: valueOf(in.Description), Accounts: in.Accounts, Query: valueOf(in.Query),
		OnNew: boolOf(in.OnNew), Schedule: valueOf(in.Schedule), Priority: int32Ptr(in.Priority), Stop: boolOf(in.Stop),
		Actions: actions, MaxPerRun: int32(maxPerRun), Enabled: in.Enabled,
	}
	return &chainmailv1.UpdateRuleRequest{
		Rule: in.Rule, Patch: patch, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}, ExpectedRevision: in.ExpectedRevision,
	}, nil
}

// dryRunRuleRequest builds the DryRunRule request: exactly one of a saved
// rule and an unsaved definition, and a known trigger.
func dryRunRuleRequest(in dryRunRuleIn) (*chainmailv1.DryRunRuleRequest, error) {
	switch {
	case in.Rule != "" && in.Definition != nil:
		return nil, invalidArg("give rule or definition, not both")
	case in.Rule == "" && in.Definition == nil:
		return nil, invalidArg("give rule, a saved rule's ID or name, or definition, an unsaved rule")
	}
	if !slices.Contains(ruleTriggers, in.Trigger) {
		return nil, invalidArg("trigger must be new, schedule or manual, got %q", in.Trigger)
	}
	req := &chainmailv1.DryRunRuleRequest{Rule: in.Rule, Trigger: in.Trigger, Account: in.Account}
	if in.Definition != nil {
		d, err := ruleDefinition(*in.Definition)
		if err != nil {
			return nil, err
		}
		req.Definition = d
	}
	return req, nil
}

// runRuleRequest builds the RunRule request. max 0 means the rule's own
// max_per_run.
func runRuleRequest(in runRuleIn) (*chainmailv1.RunRuleRequest, error) {
	if in.Max < 0 || in.Max > ruleMaxPerRun {
		return nil, invalidArg("max must be 1 to 5000, or 0 for the rule's max_per_run, got %d", in.Max)
	}
	return &chainmailv1.RunRuleRequest{Rule: in.Rule, Max: int32(in.Max), Account: in.Account}, nil
}
