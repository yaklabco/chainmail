package daemon

import (
	"cmp"
	"errors"
	"slices"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/rules"
)

// definitionOf converts a rule definition as a client sent it into the
// model, normalized. An unset priority is the default, an unset enabled is
// true, and a max_per_run of 0 is the default. It checks only what the
// conversion needs, the action kinds and the schedule's syntax; the caller
// runs Definition.Validate for the rest. A nil definition is an empty one.
func definitionOf(m *chainmailv1.RuleDefinition) (rules.Definition, error) {
	if m == nil {
		m = &chainmailv1.RuleDefinition{}
	}
	d := rules.Defaults()
	d.Name, d.Description, d.Query = m.GetName(), m.GetDescription(), m.GetQuery()
	d.Accounts = slices.Clone(m.GetAccounts())
	d.OnNew, d.Stop = m.GetOnNew(), m.GetStop()
	if m.Priority != nil {
		d.Priority = int(m.GetPriority())
	}
	d.MaxPerRun = int(m.GetMaxPerRun())
	if m.Enabled != nil {
		d.Enabled = m.GetEnabled()
	}
	schedule, scheduleErr := scheduleOf(m.GetSchedule())
	actions, actionsErr := actionsOf(m.GetActions())
	d.Schedule, d.Actions = schedule, actions
	return d.Normalize(), cmp.Or(scheduleErr, actionsErr)
}

// patchOf converts the fields an UpdateRule mask names into a store patch.
// A named field sent empty sets the empty value; a named priority left
// unset is the default. The first conversion failure, in mask order, is
// returned. A nil patch is an empty one.
func patchOf(m *chainmailv1.RuleDefinition, fields []string, expected *int64) (rules.Patch, error) {
	if m == nil {
		m = &chainmailv1.RuleDefinition{}
	}
	p := rules.Patch{ExpectedRevision: expected}
	for _, f := range fields {
		switch f {
		case "name":
			p.Name = new(m.GetName())
		case "description":
			p.Description = new(m.GetDescription())
		case "accounts":
			p.Accounts = new(slices.Clone(m.GetAccounts()))
		case "query":
			p.Query = new(m.GetQuery())
		case "on_new":
			p.OnNew = new(m.GetOnNew())
		case "schedule":
			sc, err := scheduleOf(m.GetSchedule())
			if err != nil {
				return rules.Patch{}, err
			}
			p.Schedule = &sc
		case "priority":
			v := rules.DefaultPriority
			if m.Priority != nil {
				v = int(m.GetPriority())
			}
			p.Priority = &v
		case "stop":
			p.Stop = new(m.GetStop())
		case "actions":
			as, err := actionsOf(m.GetActions())
			if err != nil {
				return rules.Patch{}, err
			}
			p.Actions = &as
		case "max_per_run":
			p.MaxPerRun = new(int(m.GetMaxPerRun()))
		case "enabled":
			p.Enabled = new(m.GetEnabled())
		}
	}
	return p, nil
}

// scheduleOf parses a schedule. The model's refusal passes unchanged;
// any other parse error reads as errRuleSchedule.
func scheduleOf(s string) (rules.Schedule, error) {
	sc, err := rules.ParseSchedule(s)
	if err == nil {
		return sc, nil
	}
	if _, ok := errors.AsType[*rules.InvalidError](err); ok {
		return rules.Schedule{}, err
	}
	return rules.Schedule{}, errRuleSchedule
}

// actionsOf converts actions, refusing an unknown kind. Arguments are
// Definition.Validate's to check. No actions converts to nil.
func actionsOf(in []*chainmailv1.RuleAction) ([]rules.Action, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]rules.Action, 0, len(in))
	for _, a := range in {
		k := rules.ActionKind(a.GetKind())
		if !k.Valid() {
			return nil, errActionKind
		}
		out = append(out, rules.Action{
			Kind: k, Label: a.GetLabel(), Folder: a.GetFolder(), ItemID: a.GetItemId(),
			Anchor: a.GetAnchor(), To: slices.Clone(a.GetTo()), Note: a.GetNote(),
		})
	}
	return out, nil
}

// ruleProto builds the API form of a rule. The description is wrapped with
// its line breaks and the query on one line, since both are often copied
// from mail. A zero time stays unset.
func ruleProto(r rules.Rule) *chainmailv1.Rule {
	return &chainmailv1.Rule{
		Id: r.ID, Name: r.Name, Description: untrustedBlock(r.Description), Accounts: slices.Clone(r.Accounts),
		Query: untrusted(r.Query), OnNew: r.OnNew, Schedule: r.Schedule.String(), Priority: int32(r.Priority), Stop: r.Stop,
		Actions: ruleActionsProto(r.Actions), MaxPerRun: int32(r.MaxPerRun), Enabled: r.Enabled, DisabledReason: r.DisabledReason,
		Revision: r.Revision, CreatedAt: timestamp(r.CreatedAt), UpdatedAt: timestamp(r.UpdatedAt), DeletedAt: timestamp(r.DeletedAt),
		CreatedBy: r.CreatedBy, UpdatedBy: r.UpdatedBy, NextRunAt: timestamp(r.NextRunAt), LastRunAt: timestamp(r.LastRunAt),
		FailedRuns: int32(r.FailedRuns),
	}
}

// ruleRunProto builds the API form of one run.
func ruleRunProto(r rules.Run) *chainmailv1.RuleRun {
	return &chainmailv1.RuleRun{
		Id: r.ID, RuleId: r.RuleID, Account: r.Account, Trigger: string(r.Trigger),
		StartedAt: timestamp(r.StartedAt), FinishedAt: timestamp(r.FinishedAt),
		Matched: int64(r.Matched), Applied: int64(r.Applied), Held: int64(r.Held), Skipped: int64(r.Skipped), Failed: int64(r.Failed),
		Outcome: string(r.Outcome), Reason: r.Reason, Hint: r.Hint,
	}
}

// ruleRunsProto builds the API form of runs, in their order.
func ruleRunsProto(runs []rules.Run) []*chainmailv1.RuleRun {
	out := make([]*chainmailv1.RuleRun, len(runs))
	for i, r := range runs {
		out[i] = ruleRunProto(r)
	}
	return out
}

// ruleCountsProto builds the API form of a rule's counts.
func ruleCountsProto(c rules.Counts) *chainmailv1.RuleCounts {
	return &chainmailv1.RuleCounts{Runs: c.Runs, Matched: c.Matched, Applied: c.Applied, Held: c.Held, Failed: c.Failed}
}

// ruleRevisionsProto builds the API form of revisions, in their order.
func ruleRevisionsProto(revs []rules.Revision) []*chainmailv1.RuleRevision {
	out := make([]*chainmailv1.RuleRevision, len(revs))
	for i, v := range revs {
		out[i] = &chainmailv1.RuleRevision{Revision: v.Revision, Change: v.Change, Rule: ruleProto(v.Rule), Actor: v.Actor, At: timestamp(v.At)}
	}
	return out
}
