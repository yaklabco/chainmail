package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/render"
)

// titleWidth is the most runes of a title a table line shows.
const titleWidth = 60

// termBlock makes multi-line text the daemon returns, such as an item's
// description, safe to print on a terminal: line breaks and tabs stay, and
// every other control or invisible character goes.
func termBlock(s string) string { return render.CleanText(s) }

// indent prefixes each line of s with two spaces and ends it with a
// newline. A blank line stays blank, so no line ends in spaces.
func indent(s string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(strings.TrimRight(s, "\n"), "\n") {
		if line != "" {
			b.WriteString("  ")
			b.WriteString(line)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// inherited shows an item's own area or priority, or else the effective
// one, marked as inherited from a parent or, on an item with no parent, as
// the default. It is "-" when there is neither.
func inherited(own, effective string, root bool) string {
	switch {
	case own != "":
		return termText(own)
	case effective == "":
		return "-"
	case root:
		return termText(effective) + " (default)"
	default:
		return termText(effective) + " (inherited)"
	}
}

// parentText names an item's parent, or says that a task with none is in
// the inbox.
func parentText(it *chainmailv1.WorkItem) string {
	switch {
	case it.GetParentId() != "":
		return termText(it.GetParentId())
	case it.GetType() == "task":
		return "- (inbox)"
	default:
		return "-"
	}
}

// estimateText shows an item's estimate as the shortest decimal that reads
// back as it, or "-" when the item has none.
func estimateText(it *chainmailv1.WorkItem) string {
	if it == nil || it.Estimate == nil {
		return "-"
	}
	return strconv.FormatFloat(*it.Estimate, 'f', -1, 64)
}

// byText shows when a change was made and by whom, as in
// "2026-11-06 09:00 by 1000/cli".
func byText(ts *timestamppb.Timestamp, loc *time.Location, who string) string {
	return orDash(shortDate(ts, loc)) + " by " + orDash(termText(who))
}

// itemFieldsTable prints an item's fields, one name and value per line.
// Every value is cleaned for the terminal, the title included.
func itemFieldsTable(w io.Writer, loc *time.Location, it *chainmailv1.WorkItem) error {
	root := it.GetParentId() == ""
	tw := newTable(w)
	for _, f := range [][2]string{
		{"id", termText(it.GetId())},
		{"type", termText(it.GetType())},
		{"title", termText(it.GetTitle().GetUntrusted())},
		{"state", termText(it.GetState())},
		{"area", inherited(it.GetArea(), it.GetEffectiveArea(), root)},
		{"priority", inherited(it.GetPriority(), it.GetEffectivePriority(), root)},
		{"parent", parentText(it)},
		{"start", orDash(termText(it.GetStartDate()))},
		{"due", orDash(termText(it.GetDueDate()))},
		{"estimate", estimateText(it)},
		{"ongoing", strconv.FormatBool(it.GetOngoing())},
		{"deleted", orDash(shortDate(it.GetDeletedAt(), loc))},
		{"revision", strconv.FormatInt(it.GetRevision(), 10)},
		{"created", byText(it.GetCreatedAt(), loc, it.GetCreatedBy())},
		{"updated", byText(it.GetUpdatedAt(), loc, it.GetUpdatedBy())},
	} {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", f[0], f[1])
	}
	return tw.Flush()
}

// criteriaText prints criteria in order: a heading with the ticked count,
// then a tick box, the ID and the text of each, and the evidence of a
// ticked one on the line below it.
func criteriaText(b *strings.Builder, crit []*chainmailv1.Criterion) {
	ticked := 0
	for _, c := range crit {
		if c.GetTicked() {
			ticked++
		}
	}
	fmt.Fprintf(b, "criteria: %d of %d ticked\n", ticked, len(crit))
	for _, c := range crit {
		box := " "
		if c.GetTicked() {
			box = "x"
		}
		fmt.Fprintf(b, "  [%s] %s %s\n", box, termText(c.GetId()), termText(c.GetText().GetUntrusted()))
		if ev := termText(c.GetEvidence().GetUntrusted()); c.GetTicked() && ev != "" {
			fmt.Fprintf(b, "      evidence: %s\n", ev)
		}
	}
}

// rowTitle is a row's title for a table: cleaned, clipped, and marked when
// the item is ongoing or deleted.
func rowTitle(r *chainmailv1.WorkItemRow) string {
	title := clip(termText(r.GetTitle().GetUntrusted()), titleWidth)
	if r.GetOngoing() {
		title += " (ongoing)"
	}
	if r.GetDeleted() {
		title += " (deleted)"
	}
	return title
}

// countHeading is a section heading with its count, "children: 3", or
// "children: 50 of 72" when the daemon capped the list.
func countHeading(name string, shown int, total int32) string {
	if int(total) > shown {
		return fmt.Sprintf("%s: %d of %d", name, shown, total)
	}
	return fmt.Sprintf("%s: %d", name, shown)
}

// itemRowsSection prints a heading, then one line per row: its ID, type,
// state and title.
func itemRowsSection(w io.Writer, heading string, rows []*chainmailv1.WorkItemRow) error {
	if err := writeString(w, heading+"\n"); err != nil {
		return err
	}
	tw := newTable(w)
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", termText(r.GetId()), termText(r.GetType()), termText(r.GetState()), rowTitle(r))
	}
	return tw.Flush()
}

// threadsSection prints a heading, then one line per linked thread: its
// state, with the reason when it is unavailable, its account and thread
// key, the message count, the newest message's date, subject and ref, and
// the link's note.
func threadsSection(w io.Writer, loc *time.Location, heading string, links []*chainmailv1.ThreadLink) error {
	if err := writeString(w, heading+"\n"); err != nil {
		return err
	}
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "  STATE\tACCOUNT\tTHREAD\tMESSAGES\tLATEST\tSUBJECT\tREF\tNOTE")
	for _, l := range links {
		state := termText(l.GetState())
		if reason := l.GetReason(); reason != "" {
			state += " (" + termText(reason) + ")"
		}
		latest := l.GetLatest()
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			state, termText(l.GetAccount()), termText(l.GetThreadKey()), l.GetMessages(),
			orDash(shortDate(latest.GetDate(), loc)), orDash(clip(termText(latest.GetSubject().GetUntrusted()), 50)),
			orDash(termText(latest.GetRef())), orDash(clip(termText(l.GetNote().GetUntrusted()), 40)))
	}
	return tw.Flush()
}

// itemViewText prints item show's view: the fields, the description, the
// criteria, then each non-empty list of related items and the linked
// threads, separated by blank lines.
func itemViewText(w io.Writer, loc *time.Location, r *chainmailv1.GetItemResponse) error {
	if err := itemFieldsTable(w, loc, r.GetItem()); err != nil {
		return err
	}
	var b strings.Builder
	if d := termBlock(r.GetItem().GetDescription().GetUntrusted()); strings.TrimSpace(d) != "" {
		b.WriteString("\ndescription:\n")
		b.WriteString(indent(d))
	}
	if crit := r.GetCriteria(); len(crit) > 0 {
		b.WriteString("\n")
		criteriaText(&b, crit)
	}
	if err := writeString(w, b.String()); err != nil {
		return err
	}
	for _, s := range []struct {
		heading string
		rows    []*chainmailv1.WorkItemRow
	}{
		{heading: "ancestors:", rows: r.GetAncestors()},
		{heading: countHeading("children", len(r.GetChildren()), r.GetChildrenTotal()), rows: r.GetChildren()},
		{heading: "blocked by:", rows: r.GetBlockedBy()},
		{heading: "blocks:", rows: r.GetBlocks()},
	} {
		if len(s.rows) == 0 {
			continue
		}
		if err := writeString(w, "\n"); err != nil {
			return err
		}
		if err := itemRowsSection(w, s.heading, s.rows); err != nil {
			return err
		}
	}
	links := r.GetThreads()
	if len(links) == 0 {
		return nil
	}
	if err := writeString(w, "\n"); err != nil {
		return err
	}
	return threadsSection(w, loc, countHeading("threads", len(links), r.GetThreadsTotal()), links)
}
