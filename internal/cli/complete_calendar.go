package cli

import (
	"context"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/client"
)

// completionAgendaDays is how far ahead event ref completion looks.
const completionAgendaDays = 14

// flagValues is the value of cmd's flag name as a list: each value of a
// repeatable flag once, or the one value of another, or none when it is
// empty or not a flag of cmd. cobra may parse a completion request's flags
// more than once, which repeats a repeatable flag's values.
func flagValues(cmd *cobra.Command, name string) []string {
	f := cmd.Flags().Lookup(name)
	if f == nil {
		return nil
	}
	if s, ok := f.Value.(pflag.SliceValue); ok {
		var out []string
		for _, v := range s.GetSlice() {
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
		return out
	}
	if v := f.Value.String(); v != "" {
		return []string{v}
	}
	return nil
}

// calendarIDs completes --calendar with the calendars of the accounts
// --account names, or of every account, each described by its account and
// name.
func (cp *completer) calendarIDs(cmd *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	var list []*chainmailv1.CalendarInfo
	ok := cp.call(cmd, func(ctx context.Context, c *client.Client) error {
		resp, err := c.Calendar.ListCalendars(ctx, connect.NewRequest(&chainmailv1.ListCalendarsRequest{Accounts: flagValues(cmd, "account")}))
		if err != nil {
			return err
		}
		list = resp.Msg.GetCalendars()
		return nil
	})
	if !ok {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []cobra.Completion
	for _, c := range list {
		id := c.GetId()
		if !safeValue(id) || !strings.HasPrefix(id, toComplete) {
			continue
		}
		out = append(out, cobra.CompletionWithDesc(id, completionText(c.GetAccount()+" "+c.GetName().GetUntrusted())))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// refCandidates offers each event's ref once, described by when it starts
// and its title, as up to maxArgs positional arguments.
func (cp *completer) refCandidates(events []*chainmailv1.CalendarEvent, args []string, toComplete string) []cobra.Completion {
	loc := cp.env.location()
	seen := map[string]bool{}
	var out []cobra.Completion
	for _, e := range events {
		ref := e.GetRef()
		if seen[ref] || !safeValue(ref) || !strings.HasPrefix(ref, toComplete) || slices.Contains(args, ref) {
			continue
		}
		seen[ref] = true
		out = append(out, cobra.CompletionWithDesc(ref, completionText(whenShort(e, loc)+" "+e.GetSummary().GetUntrusted())))
	}
	return out
}

// eventRefs completes the refs of the events in the next
// completionAgendaDays, as up to maxArgs positional arguments.
func (cp *completer) eventRefs(maxArgs int) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) >= maxArgs {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var events []*chainmailv1.CalendarEvent
		ok := cp.call(cmd, func(ctx context.Context, c *client.Client) error {
			now := cp.env.now()
			resp, err := c.Calendar.Agenda(ctx, connect.NewRequest(&chainmailv1.AgendaRequest{
				From: timestamppb.New(now), To: timestamppb.New(now.AddDate(0, 0, completionAgendaDays)),
			}))
			if err != nil {
				return err
			}
			events = resp.Msg.GetEvents()
			return nil
		})
		if !ok {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return cp.refCandidates(events, args, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

// inviteRefs completes the refs of the invites that wait for a reply, as
// up to maxArgs positional arguments.
func (cp *completer) inviteRefs(maxArgs int) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) >= maxArgs {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var events []*chainmailv1.CalendarEvent
		ok := cp.call(cmd, func(ctx context.Context, c *client.Client) error {
			resp, err := c.Calendar.ListInvites(ctx, connect.NewRequest(&chainmailv1.ListInvitesRequest{}))
			if err != nil {
				return err
			}
			for _, inv := range resp.Msg.GetInvites() {
				events = append(events, inv.GetEvent())
			}
			return nil
		})
		if !ok {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return cp.refCandidates(events, args, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

// rsvpArgs completes rsvp: an invite ref, then the answer.
func (cp *completer) rsvpArgs(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		return cp.inviteRefs(1)(cmd, args, toComplete)
	case 1:
		return fixed(rsvpAnswers...)(cmd, args, toComplete)
	default:
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

// rangeFlagSpecs completes addRangeFlags' flags.
func rangeFlagSpecs() map[string]cobra.CompletionFunc {
	return map[string]cobra.CompletionFunc{"from": noComplete, "to": noComplete, "days": fixed("1", "7", "14", "30"), "tz": noComplete}
}

// eventFieldSpecs completes addEventFlags' flags.
func eventFieldSpecs() map[string]cobra.CompletionFunc {
	return map[string]cobra.CompletionFunc{
		"title": noComplete, "start": noComplete, "end": noComplete, "duration": fixed("15m", "30m", "1h", "1d"),
		"attendee": noComplete, "location": noComplete, "description": noComplete, "rrule": noComplete, "tz": noComplete,
	}
}

// calendarSpecs is how the calendar commands complete, by command path.
// specs merges it in.
func (cp *completer) calendarSpecs() map[string]compSpec {
	allAccounts := cp.accounts(nil, 0)
	accountOnly := map[string]cobra.CompletionFunc{"account": allAccounts}
	return map[string]compSpec{
		"chainmail calendars": {args: noComplete, flags: accountOnly},
		"chainmail agenda": {args: noComplete, flags: mergeFlagCompletions(rangeFlagSpecs(), map[string]cobra.CompletionFunc{
			"account": allAccounts, "calendar": cp.calendarIDs,
		})},
		"chainmail event": {args: cp.eventRefs(1)},
		"chainmail event create": {args: noComplete, flags: mergeFlagCompletions(eventFieldSpecs(), map[string]cobra.CompletionFunc{
			"account": allAccounts, "calendar": cp.calendarIDs, "idempotency-key": noComplete,
		})},
		"chainmail event edit":   {args: cp.eventRefs(1), flags: mergeFlagCompletions(eventFieldSpecs(), map[string]cobra.CompletionFunc{"etag": noComplete})},
		"chainmail event cancel": {args: cp.eventRefs(1), flags: map[string]cobra.CompletionFunc{"etag": noComplete}},
		"chainmail invites":      {args: noComplete, flags: accountOnly},
		"chainmail invite":       {args: cp.inviteRefs(1)},
		"chainmail rsvp":         {args: cp.rsvpArgs, flags: map[string]cobra.CompletionFunc{"comment": noComplete, "idempotency-key": noComplete}},
		"chainmail freebusy":     {args: noComplete, flags: mergeFlagCompletions(rangeFlagSpecs(), accountOnly)},
	}
}
