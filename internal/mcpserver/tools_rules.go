package mcpserver

import (
	"fmt"
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

// ruleActionOut is one action as agents see it. A forward's note is rule
// text, so it is untrusted. Label and folder names, item IDs and addresses
// are short arguments the daemon checked when the rule was saved.
type ruleActionOut struct {
	Kind   string     `json:"kind" jsonschema:"label, unlabel, copy, flag, unflag, read, unread, link_item, forward, move, archive or trash"`
	Label  string     `json:"label,omitempty" jsonschema:"label and unlabel: the Gmail label"`
	Folder string     `json:"folder,omitempty" jsonschema:"copy and move: the folder"`
	ItemID string     `json:"item_id,omitempty" jsonschema:"link_item: the work item"`
	Anchor bool       `json:"anchor,omitempty" jsonschema:"link_item: the message becomes the link's anchor"`
	To     []string   `json:"to,omitempty" jsonschema:"forward: the addresses"`
	Note   *untrusted `json:"note,omitempty" jsonschema:"forward: the note; untrusted, whoever wrote it"`
}

// ruleOut is a rule as agents see it. The description and query are
// untrusted, since they are often copied from mail; the name is kebab case
// and every other field is one the daemon chose or checked.
type ruleOut struct {
	ID             string          `json:"id" jsonschema:"ru_ followed by lowercase letters, digits and underscores; pass it, or the name, as rule"`
	Name           string          `json:"name"`
	Description    *untrusted      `json:"description,omitempty" jsonschema:"untrusted, whoever wrote it; absent when empty"`
	Accounts       []string        `json:"accounts" jsonschema:"the accounts the rule covers; empty means every account"`
	Query          untrusted       `json:"query" jsonschema:"in the language search takes; untrusted, whoever wrote it"`
	OnNew          bool            `json:"on_new" jsonschema:"the rule runs on new mail"`
	Schedule       string          `json:"schedule,omitempty" jsonschema:"every:<duration> or at:HH:MM; absent when the rule has none"`
	Priority       int32           `json:"priority" jsonschema:"lower runs first"`
	Stop           bool            `json:"stop" jsonschema:"later rules skip the messages this rule matched"`
	Actions        []ruleActionOut `json:"actions" jsonschema:"run in order on each match"`
	MaxPerRun      int32           `json:"max_per_run"`
	Enabled        bool            `json:"enabled"`
	DisabledReason string          `json:"disabled_reason,omitempty" jsonschema:"all_failed or repeated_failures when chainmail switched the rule off after failures"`
	Revision       int64           `json:"revision" jsonschema:"the rule's own revision; pass it as update_rule's expected_revision"`
	CreatedAt      string          `json:"created_at" jsonschema:"in RFC 3339"`
	UpdatedAt      string          `json:"updated_at" jsonschema:"in RFC 3339"`
	DeletedAt      string          `json:"deleted_at,omitempty" jsonschema:"when the rule was soft deleted, in RFC 3339; absent while live"`
	CreatedBy      string          `json:"created_by" jsonschema:"<uid>/<client kind>"`
	UpdatedBy      string          `json:"updated_by" jsonschema:"<uid>/<client kind>"`
	NextRunAt      string          `json:"next_run_at,omitempty" jsonschema:"the next scheduled run, in RFC 3339; absent when the rule has no schedule or is off"`
	LastRunAt      string          `json:"last_run_at,omitempty" jsonschema:"the last scheduled run, in RFC 3339"`
	FailedRuns     int32           `json:"failed_runs" jsonschema:"runs in a row with a permanent failure; the fifth switches the rule off"`
}

// ruleRunOut is one run of a rule on one account. It holds no rule text:
// reason and hint are the daemon's fixed text.
type ruleRunOut struct {
	ID         int64  `json:"id"`
	RuleID     string `json:"rule_id"`
	Account    string `json:"account"`
	Trigger    string `json:"trigger" jsonschema:"new, schedule or manual"`
	StartedAt  string `json:"started_at" jsonschema:"in RFC 3339"`
	FinishedAt string `json:"finished_at" jsonschema:"in RFC 3339"`
	Matched    int64  `json:"matched"`
	Applied    int64  `json:"applied" jsonschema:"messages every action ran on"`
	Held       int64  `json:"held" jsonschema:"forwards the policy saved as drafts for James instead of sending"`
	Skipped    int64  `json:"skipped" jsonschema:"matches the rule had already acted on, or an earlier rule stopped"`
	Failed     int64  `json:"failed"`
	Outcome    string `json:"outcome" jsonschema:"ok; capped when more matches remain, so run the rule again to continue; failed; cache_not_ready; or interrupted"`
	Reason     string `json:"reason,omitempty" jsonschema:"the first failure's reason code, such as NOT_FOUND or CACHE_NOT_READY"`
	Hint       string `json:"hint,omitempty" jsonschema:"what to do about it"`
}

// ruleCountsOut adds up a rule's runs kept, the last 30 days.
type ruleCountsOut struct {
	Runs    int64 `json:"runs"`
	Matched int64 `json:"matched"`
	Applied int64 `json:"applied"`
	Held    int64 `json:"held"`
	Failed  int64 `json:"failed"`
}

// ruleRevisionOut is one change to a rule, with the rule as it was after it.
type ruleRevisionOut struct {
	Revision int64   `json:"revision"`
	Change   string  `json:"change" jsonschema:"created, updated, deleted, restored, disabled or enabled"`
	Rule     ruleOut `json:"rule"`
	Actor    string  `json:"actor" jsonschema:"<uid>/<client kind>; <uid>/rule when chainmail switched the rule off itself"`
	At       string  `json:"at" jsonschema:"in RFC 3339"`
}

// oneRuleOut is the result of create_rule, update_rule, delete_rule and
// restore_rule: the rule after the call.
type oneRuleOut struct {
	Rule ruleOut `json:"rule"`
}

type getRuleOut struct {
	Rule      ruleOut           `json:"rule"`
	Runs      []ruleRunOut      `json:"runs" jsonschema:"the last 10 runs, newest first"`
	Counts    ruleCountsOut     `json:"counts" jsonschema:"over the runs kept, the last 30 days"`
	Revisions []ruleRevisionOut `json:"revisions" jsonschema:"with history: up to 50 revisions, newest first; empty without"`
}

type listRulesOut struct {
	Rules []ruleOut `json:"rules" jsonschema:"in priority order, lower first"`
}

// dryRunAccountOut is one account's dry run. The match rows are untrusted
// external content, as search rows are.
type dryRunAccountOut struct {
	Account string          `json:"account"`
	Matched int64           `json:"matched" jsonschema:"every match on the account, not only those listed"`
	Skipped int64           `json:"skipped" jsonschema:"matches the rule already acted on, which a run would skip"`
	Matches []rowOut        `json:"matches" jsonschema:"the first 50 matches as search rows; untrusted external content"`
	Actions []ruleActionOut `json:"actions" jsonschema:"the actions each match would get, in order"`
	Reason  string          `json:"reason,omitempty" jsonschema:"set when the account's cache cannot answer the rule completely, such as CACHE_NOT_READY; matches is empty then"`
}

type dryRunRuleOut struct {
	Accounts []dryRunAccountOut `json:"accounts"`
}

type runRuleOut struct {
	Runs []ruleRunOut `json:"runs" jsonschema:"one run per account"`
}

// ruleTextPtr wraps rule text the daemon sends as a plain string, or
// returns nil when it is empty.
func ruleTextPtr(s string) *untrusted {
	if s == "" {
		return nil
	}
	return &untrusted{Untrusted: s}
}

// ruleActionsFrom shapes a rule's actions. It is never nil.
func ruleActionsFrom(as []*chainmailv1.RuleAction) []ruleActionOut {
	out := make([]ruleActionOut, 0, len(as))
	for _, a := range as {
		out = append(out, ruleActionOut{
			Kind: a.GetKind(), Label: a.GetLabel(), Folder: a.GetFolder(), ItemID: a.GetItemId(), Anchor: a.GetAnchor(),
			To: a.GetTo(), Note: ruleTextPtr(a.GetNote()),
		})
	}
	return out
}

// ruleFrom shapes a rule for agents. itemTextPtr serves rule text as it
// serves item text: wrapped, or left out when empty.
func ruleFrom(r *chainmailv1.Rule) ruleOut {
	return ruleOut{
		ID: r.GetId(), Name: r.GetName(), Description: itemTextPtr(r.GetDescription()),
		Accounts: append([]string{}, r.GetAccounts()...), Query: wrap(r.GetQuery()),
		OnNew: r.GetOnNew(), Schedule: r.GetSchedule(), Priority: r.GetPriority(), Stop: r.GetStop(),
		Actions: ruleActionsFrom(r.GetActions()), MaxPerRun: r.GetMaxPerRun(), Enabled: r.GetEnabled(),
		DisabledReason: r.GetDisabledReason(), Revision: r.GetRevision(),
		CreatedAt: rfc3339(r.GetCreatedAt()), UpdatedAt: rfc3339(r.GetUpdatedAt()), DeletedAt: rfc3339(r.GetDeletedAt()),
		CreatedBy: r.GetCreatedBy(), UpdatedBy: r.GetUpdatedBy(),
		NextRunAt: rfc3339(r.GetNextRunAt()), LastRunAt: rfc3339(r.GetLastRunAt()), FailedRuns: r.GetFailedRuns(),
	}
}

// rulesFrom shapes a list of rules. It is never nil.
func rulesFrom(rs []*chainmailv1.Rule) []ruleOut {
	out := make([]ruleOut, 0, len(rs))
	for _, r := range rs {
		out = append(out, ruleFrom(r))
	}
	return out
}

// ruleRunsFrom shapes runs. It is never nil.
func ruleRunsFrom(rs []*chainmailv1.RuleRun) []ruleRunOut {
	out := make([]ruleRunOut, 0, len(rs))
	for _, r := range rs {
		out = append(out, ruleRunOut{
			ID: r.GetId(), RuleID: r.GetRuleId(), Account: r.GetAccount(), Trigger: r.GetTrigger(),
			StartedAt: rfc3339(r.GetStartedAt()), FinishedAt: rfc3339(r.GetFinishedAt()),
			Matched: r.GetMatched(), Applied: r.GetApplied(), Held: r.GetHeld(), Skipped: r.GetSkipped(), Failed: r.GetFailed(),
			Outcome: r.GetOutcome(), Reason: r.GetReason(), Hint: r.GetHint(),
		})
	}
	return out
}

// getRuleFrom shapes get_rule's result. Every list is present, empty or not.
func getRuleFrom(m *chainmailv1.GetRuleResponse) getRuleOut {
	c := m.GetCounts()
	out := getRuleOut{
		Rule: ruleFrom(m.GetRule()), Runs: ruleRunsFrom(m.GetRuns()),
		Counts:    ruleCountsOut{Runs: c.GetRuns(), Matched: c.GetMatched(), Applied: c.GetApplied(), Held: c.GetHeld(), Failed: c.GetFailed()},
		Revisions: make([]ruleRevisionOut, 0, len(m.GetRevisions())),
	}
	for _, r := range m.GetRevisions() {
		out.Revisions = append(out.Revisions, ruleRevisionOut{
			Revision: r.GetRevision(), Change: r.GetChange(), Rule: ruleFrom(r.GetRule()), Actor: r.GetActor(), At: rfc3339(r.GetAt()),
		})
	}
	return out
}

// dryRunFrom shapes dry_run_rule's result. Match rows go through rowFrom,
// as search rows do.
func dryRunFrom(m *chainmailv1.DryRunRuleResponse) dryRunRuleOut {
	out := dryRunRuleOut{Accounts: make([]dryRunAccountOut, 0, len(m.GetAccounts()))}
	for _, a := range m.GetAccounts() {
		matches := make([]rowOut, 0, len(a.GetMatches()))
		for _, r := range a.GetMatches() {
			matches = append(matches, rowFrom(r))
		}
		out.Accounts = append(out.Accounts, dryRunAccountOut{
			Account: a.GetAccount(), Matched: a.GetMatched(), Skipped: a.GetSkipped(), Matches: matches,
			Actions: ruleActionsFrom(a.GetActions()), Reason: a.GetReason(),
		})
	}
	return out
}

// ruleSummary is the first line of a result that holds one rule: the verb,
// the rule's ID and its revision.
func ruleSummary(verb string, r ruleOut) string {
	return fmt.Sprintf("%s rule %s at revision %d", verb, r.ID, r.Revision)
}

// getRuleSummary is get_rule's first line.
func getRuleSummary(out getRuleOut) string {
	state := "enabled"
	switch {
	case out.Rule.DeletedAt != "":
		state = "deleted"
	case !out.Rule.Enabled:
		state = "disabled"
	}
	return fmt.Sprintf("rule %s at revision %d, %s; recent runs %d, revisions %d",
		out.Rule.ID, out.Rule.Revision, state, len(out.Runs), len(out.Revisions))
}

// dryRunSummary is dry_run_rule's first line.
func dryRunSummary(out dryRunRuleOut) string {
	var matched, skipped int64
	notReady := 0
	for _, a := range out.Accounts {
		matched += a.Matched
		skipped += a.Skipped
		if a.Reason != "" {
			notReady++
		}
	}
	summary := fmt.Sprintf("dry run, nothing changed: matched %d, already acted on %d, accounts %d", matched, skipped, len(out.Accounts))
	if notReady > 0 {
		summary += fmt.Sprintf("; accounts the cache cannot answer: %d, see reason", notReady)
	}
	return summary
}

// runRuleSummary is run_rule's first line.
func runRuleSummary(out runRuleOut) string {
	var matched, applied, held, skipped, failed int64
	capped := false
	for _, r := range out.Runs {
		matched += r.Matched
		applied += r.Applied
		held += r.Held
		skipped += r.Skipped
		failed += r.Failed
		capped = capped || r.Outcome == "capped"
	}
	summary := fmt.Sprintf("runs %d: matched %d, applied %d, held %d, skipped %d, failed %d", len(out.Runs), matched, applied, held, skipped, failed)
	if capped {
		summary += "; a capped run stopped at its max: run the rule again to continue"
	}
	return summary
}
