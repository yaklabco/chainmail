package cli

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
)

// ruleState is a rule's state for a table: enabled, disabled with the
// reason an automatic disable gave, or deleted.
func ruleState(r *chainmailv1.Rule) string {
	switch {
	case r.GetDeletedAt() != nil:
		return "deleted"
	case r.GetEnabled():
		return "enabled"
	case r.GetDisabledReason() != "":
		return "disabled (" + termText(r.GetDisabledReason()) + ")"
	default:
		return "disabled"
	}
}

// ruleAccountsText names the accounts a rule covers, or "all" for a rule
// that names none.
func ruleAccountsText(r *chainmailv1.Rule) string {
	if len(r.GetAccounts()) == 0 {
		return "all"
	}
	names := make([]string, len(r.GetAccounts()))
	for i, a := range r.GetAccounts() {
		names[i] = termText(a)
	}
	return strings.Join(names, ", ")
}

// ruleTriggersText names what runs a rule: new mail, its schedule, or
// "manual" when only rule run does.
func ruleTriggersText(r *chainmailv1.Rule) string {
	var parts []string
	if r.GetOnNew() {
		parts = append(parts, "new")
	}
	if s := r.GetSchedule(); s != "" {
		parts = append(parts, termText(s))
	}
	if len(parts) == 0 {
		return "manual"
	}
	return strings.Join(parts, ", ")
}

// ruleActionText is one action in the --action form that gives it, cleaned
// for the terminal. A forward's note is left out.
func ruleActionText(a *chainmailv1.RuleAction) string {
	kind := termText(a.GetKind())
	switch a.GetKind() {
	case "label", "unlabel":
		return kind + "=" + termText(a.GetLabel())
	case "copy", "move":
		return kind + "=" + termText(a.GetFolder())
	case "link_item":
		s := kind + "=" + termText(a.GetItemId())
		if a.GetAnchor() {
			s += ",anchor"
		}
		return s
	case "forward":
		to := make([]string, len(a.GetTo()))
		for i, addr := range a.GetTo() {
			to[i] = termText(addr)
		}
		return kind + "=" + strings.Join(to, ",")
	default:
		return kind
	}
}

// ruleFieldsTable prints a rule's fields, one name and value per line.
// Every value is cleaned for the terminal, the query included.
func ruleFieldsTable(w io.Writer, loc *time.Location, r *chainmailv1.Rule) error {
	tw := newTable(w)
	for _, f := range [][2]string{
		{"id", termText(r.GetId())},
		{"name", termText(r.GetName())},
		{"state", ruleState(r)},
		{"accounts", ruleAccountsText(r)},
		{"query", orDash(termText(r.GetQuery().GetUntrusted()))},
		{"triggers", ruleTriggersText(r)},
		{"priority", strconv.Itoa(int(r.GetPriority()))},
		{"stop", strconv.FormatBool(r.GetStop())},
		{"max per run", strconv.Itoa(int(r.GetMaxPerRun()))},
		{"revision", strconv.FormatInt(r.GetRevision(), 10)},
		{"failed runs", strconv.Itoa(int(r.GetFailedRuns()))},
		{"next run", orDash(shortDate(r.GetNextRunAt(), loc))},
		{"last run", orDash(shortDate(r.GetLastRunAt(), loc))},
		{"created", byText(r.GetCreatedAt(), loc, r.GetCreatedBy())},
		{"updated", byText(r.GetUpdatedAt(), loc, r.GetUpdatedBy())},
		{"deleted", orDash(shortDate(r.GetDeletedAt(), loc))},
	} {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", f[0], f[1])
	}
	return tw.Flush()
}

// ruleViewText prints rule show's view: the fields, the description, the
// actions one per line with a forward's note below it, the totals over
// the runs the daemon keeps, the recent runs, and with --history the
// revisions.
func ruleViewText(w io.Writer, loc *time.Location, r *chainmailv1.GetRuleResponse) error {
	rule := r.GetRule()
	if err := ruleFieldsTable(w, loc, rule); err != nil {
		return err
	}
	var b strings.Builder
	if d := termBlock(rule.GetDescription().GetUntrusted()); strings.TrimSpace(d) != "" {
		b.WriteString("\ndescription:\n")
		b.WriteString(indent(d))
	}
	if actions := rule.GetActions(); len(actions) > 0 {
		b.WriteString("\nactions:\n")
		for _, a := range actions {
			fmt.Fprintf(&b, "  %s\n", ruleActionText(a))
			if note := termText(a.GetNote()); note != "" {
				fmt.Fprintf(&b, "    note: %s\n", note)
			}
		}
	}
	c := r.GetCounts()
	fmt.Fprintf(&b, "\nlast 30 days: %d runs, %d matched, %d applied, %d held, %d failed\n",
		c.GetRuns(), c.GetMatched(), c.GetApplied(), c.GetHeld(), c.GetFailed())
	if err := writeString(w, b.String()); err != nil {
		return err
	}
	if runs := r.GetRuns(); len(runs) > 0 {
		if err := writeString(w, "recent runs:\n"); err != nil {
			return err
		}
		if err := ruleRunsTable(w, loc, runs, false); err != nil {
			return err
		}
	}
	if revs := r.GetRevisions(); len(revs) > 0 {
		if err := writeString(w, "\nrevisions:\n"); err != nil {
			return err
		}
		return ruleRevisionsTable(w, loc, revs)
	}
	return nil
}

// ruleRunsTable prints one line per run, with its rule's ID when withRule
// is set, then the hint of each run that has one.
func ruleRunsTable(w io.Writer, loc *time.Location, runs []*chainmailv1.RuleRun, withRule bool) error {
	header := []string{"ID", "ACCOUNT", "TRIGGER", "STARTED", "OUTCOME", "MATCHED", "APPLIED", "HELD", "SKIPPED", "FAILED", "REASON"}
	if withRule {
		header = slices.Insert(header, 1, "RULE")
	}
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t"))
	var hints strings.Builder
	for _, run := range runs {
		cells := []string{strconv.FormatInt(run.GetId(), 10)}
		if withRule {
			cells = append(cells, orDash(termText(run.GetRuleId())))
		}
		cells = append(cells,
			orDash(termText(run.GetAccount())), orDash(termText(run.GetTrigger())), orDash(shortDate(run.GetStartedAt(), loc)),
			orDash(termText(run.GetOutcome())), strconv.FormatInt(run.GetMatched(), 10), strconv.FormatInt(run.GetApplied(), 10),
			strconv.FormatInt(run.GetHeld(), 10), strconv.FormatInt(run.GetSkipped(), 10), strconv.FormatInt(run.GetFailed(), 10),
			orDash(termText(run.GetReason())))
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
		if hint := termText(run.GetHint()); hint != "" {
			fmt.Fprintf(&hints, "run %d hint: %s\n", run.GetId(), hint)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return writeString(w, hints.String())
}

// ruleRevisionsTable prints one line per revision: its number, the change,
// when and by whom.
func ruleRevisionsTable(w io.Writer, loc *time.Location, revs []*chainmailv1.RuleRevision) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "REVISION\tCHANGE\tAT\tACTOR")
	for _, rv := range revs {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n",
			rv.GetRevision(), orDash(termText(rv.GetChange())), orDash(shortDate(rv.GetAt(), loc)), orDash(termText(rv.GetActor())))
	}
	return tw.Flush()
}
