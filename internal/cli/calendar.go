package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
)

// calendarTimesHelp says how every calendar command reads a time.
const calendarTimesHelp = `Times are RFC 3339, such as 2026-11-10T09:00:00Z, or a time without a
zone, 2026-11-10 09:00, or a date, 2026-11-10. A time or date without a
zone is read in this machine's local zone, or in the zone --tz names. A
time the clocks skip when daylight saving time starts is refused, and a
time they show twice when it ends means the first of the two. The
daemon's [calendar] timezone does not apply here: the CLI never reads the
daemon config.`

// calendarTextHelp says that event text is untrusted.
const calendarTextHelp = `Event titles, locations, descriptions, names and comments come from
whoever wrote the event and are untrusted, as mail is. --output json
prints them as {"untrusted": "..."}.`

// calendarRangeHelp says how --from, --to and --days choose a range.
const calendarRangeHelp = `--from defaults to now, and --to to --days after --from, 7 by default. A
date alone in --from starts at the beginning of that day, and in --to
takes in the whole of that day. The range is at most 400 days.`

// The bounds of a calendar range, as the daemon enforces them.
const (
	defaultRangeDays = 7
	maxRangeDays     = 400
)

// rsvpAnswers are the answers rsvp takes, in the order help and completion
// offer them.
var rsvpAnswers = []string{"accept", "decline", "tentative"}

// calTime is a time flag as read: the instant, whether it was a date alone,
// and for a date the calendar date itself, as 00:00 UTC. All-day values
// take date, never t: local midnight does not exist on every date (the
// clocks skip it in some zones), so t can fall on the day before.
type calTime struct {
	t        time.Time
	dateOnly bool
	date     time.Time
}

// localMinuteLayout is a time without a zone, to the minute.
const localMinuteLayout = "2006-01-02 15:04"

// parseCalTime reads the value s of --flag: RFC 3339, a time without a
// zone, or a date, the last two in loc.
func parseCalTime(cmd *cobra.Command, flag, s string, loc *time.Location) (calTime, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return calTime{t: t}, nil
	}
	if w, err := time.Parse(localMinuteLayout, s); err == nil {
		t, ok := wallTime(w, loc)
		if !ok {
			return calTime{}, usageError{
				err:  fmt.Errorf("--%s %q does not exist in %s: the clocks skip it when daylight saving time starts", flag, s, loc),
				hint: helpHint(cmd),
			}
		}
		return calTime{t: t}, nil
	}
	if d, err := time.Parse(time.DateOnly, s); err == nil {
		t, _ := time.ParseInLocation(time.DateOnly, s, loc) // parses: s did
		return calTime{t: t, dateOnly: true, date: d}, nil
	}
	return calTime{}, usageError{
		err:  fmt.Errorf("--%s %q: want RFC 3339 such as 2026-11-10T09:00:00Z, a time such as 2026-11-10 09:00, or a date such as 2026-11-10", flag, s),
		hint: helpHint(cmd),
	}
}

// nextDay is the start of the day after t's, in t's zone.
func nextDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
}

// calZone is the zone times without one are read in: --tz when given,
// else the CLI's own zone.
func calZone(cmd *cobra.Command, env Env, tz string) (*time.Location, error) {
	if tz == "" {
		return env.location(), nil
	}
	loc, err := time.LoadLocation(tz)
	// time.LoadLocation takes "Local" for the machine's zone, which is no
	// zone name the daemon can store.
	if err != nil || tz == "Local" {
		return nil, usageError{err: fmt.Errorf("unknown --tz %q: want an IANA zone such as Europe/London or UTC", tz), hint: helpHint(cmd)}
	}
	return loc, nil
}

// rangeFlags are --from, --to, --days and --tz.
type rangeFlags struct {
	from, to, tz string
	days         int
}

func addRangeFlags(cmd *cobra.Command, rf *rangeFlags) {
	f := cmd.Flags()
	f.StringVar(&rf.from, "from", "", "start of the range (default: now)")
	f.StringVar(&rf.to, "to", "", "end of the range; a date alone takes in that whole day (default: --days after --from)")
	f.IntVar(&rf.days, "days", defaultRangeDays, "length of the range in days, when --to is not given")
	f.StringVar(&rf.tz, "tz", "", "IANA zone to read times without a zone in, and to show times in (default: this machine's zone)")
}

// resolve returns the range the flags name, and the zone to show it in.
func (rf *rangeFlags) resolve(cmd *cobra.Command, env Env) (from, to time.Time, loc *time.Location, err error) {
	usage := func(msg string) error { return usageError{err: errors.New(msg), hint: helpHint(cmd)} }
	f := cmd.Flags()
	switch {
	case f.Changed("days") && f.Changed("to"):
		return from, to, nil, usage("--days and --to cannot be used together")
	case rf.days < 1 || rf.days > maxRangeDays:
		return from, to, nil, usage(fmt.Sprintf("--days must be 1 to %d", maxRangeDays))
	}
	if loc, err = calZone(cmd, env, rf.tz); err != nil {
		return from, to, nil, err
	}
	from = env.now()
	if rf.from != "" {
		ct, err := parseCalTime(cmd, "from", rf.from, loc)
		if err != nil {
			return from, to, nil, err
		}
		from = ct.t
	}
	to = from.AddDate(0, 0, rf.days)
	if rf.to != "" {
		ct, err := parseCalTime(cmd, "to", rf.to, loc)
		if err != nil {
			return from, to, nil, err
		}
		to = ct.t
		if ct.dateOnly {
			to = nextDay(ct.t)
		}
	}
	switch {
	case !to.After(from):
		return from, to, nil, usage("--to must be after --from")
	case to.After(from.AddDate(0, 0, maxRangeDays)):
		return from, to, nil, usage(fmt.Sprintf("--from and --to must be at most %d days apart", maxRangeDays))
	}
	return from, to, loc, nil
}

func newCalendarsCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var accounts []string
	cmd := &cobra.Command{
		Use:   "calendars [--account NAME]...",
		Short: "List each account's calendars, with their access and which is the default",
		Long: `List the calendars of every account with a calendar store, or of the
accounts --account names. access is read or write. Created events go to
the default calendar unless --calendar names another. Calendar names come
from the provider and are untrusted.`,
		Args: exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Calendar.ListCalendars(ctx, connect.NewRequest(&chainmailv1.ListCalendarsRequest{Accounts: accounts}))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return calendarsTable(env.Stdout, resp.Msg)
		},
	}
	cmd.Flags().StringArrayVar(&accounts, "account", nil, "only this account; repeat for more (default: every account with a calendar store)")
	return cmd
}

func newAgendaCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var (
		rf        rangeFlags
		accounts  []string
		calendars []string
		declined  bool
	)
	cmd := &cobra.Command{
		Use:   "agenda [--account NAME]... [--calendar ID]... [--days N | --from T --to T] [--tz ZONE]",
		Short: "List the events in a range of days, in time order, from the calendar cache",
		Long: "List the events and recurring instances that overlap a range, from the\n" +
			"daemon's calendar cache, in time order. Events James declined are left\n" +
			"out unless --include-declined.\n\n" + calendarRangeHelp + "\n\n" + calendarTimesHelp + "\n\n" + calendarTextHelp,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			from, to, loc, err := rf.resolve(cmd, env)
			if err != nil {
				return err
			}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Calendar.Agenda(ctx, connect.NewRequest(&chainmailv1.AgendaRequest{
				Accounts: accounts, Calendars: calendars, From: timestamppb.New(from), To: timestamppb.New(to), IncludeDeclined: declined,
			}))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return agendaTable(env.Stdout, loc, resp.Msg)
		},
	}
	addRangeFlags(cmd, &rf)
	f := cmd.Flags()
	f.StringArrayVar(&accounts, "account", nil, "only this account; repeat for more (default: every account with a calendar store)")
	f.StringArrayVar(&calendars, "calendar", nil, "only this calendar ID; repeat for more (default: every calendar)")
	f.BoolVar(&declined, "include-declined", false, "also list events James declined")
	return cmd
}

func newInvitesCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var accounts []string
	cmd := &cobra.Command{
		Use:   "invites [--account NAME]...",
		Short: "List the invites that wait for a reply, from calendars and from mail",
		Long: `List the invites that wait for James's reply: calendar events where he is
an attendee who has not answered, and invites that came only by mail.
Answer one with chainmail rsvp.

` + calendarTextHelp,
		Args: exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Calendar.ListInvites(ctx, connect.NewRequest(&chainmailv1.ListInvitesRequest{Accounts: accounts}))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return invitesTable(env.Stdout, env.location(), resp.Msg)
		},
	}
	cmd.Flags().StringArrayVar(&accounts, "account", nil, "only this account; repeat for more (default: every account)")
	return cmd
}

func newInviteCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "invite <ref>",
		Short: "Show one invite, with where it came from and its attendees",
		Long:  "Show one invite by its ce_ ref, as invites lists it.\n\n" + calendarTextHelp,
		Args:  exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Calendar.GetInvite(ctx, connect.NewRequest(&chainmailv1.GetInviteRequest{Ref: args[0]}))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return inviteViewText(env.Stdout, env.location(), resp.Msg.GetInvite())
		},
	}
}

func newRsvpCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var comment, key string
	cmd := &cobra.Command{
		Use:   "rsvp <ref> accept|decline|tentative [--comment TEXT]",
		Short: "Answer an invite, through the calendar or by email to the organizer",
		Long: `Answer an invite by its ce_ ref. A ref with @<recurrence ID> answers that
instance only; a plain ref answers the whole series. With a calendar
event on an account with a calendar store, the answer goes through the
calendar, which tells the organizer. An invite that came only by mail is
answered by an iCalendar REPLY email to the organizer.

The organizer is checked against the send policy. A reply it does not
allow is held and the command exits 4: through the calendar nothing is
changed, and by email the reply is saved to Drafts.`,
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			answer := args[1]
			if !slices.Contains(rsvpAnswers, answer) {
				return usageError{err: fmt.Errorf("unknown answer %q: want accept, decline or tentative", answer), hint: helpHint(cmd)}
			}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Calendar.Respond(ctx, connect.NewRequest(&chainmailv1.RespondRequest{
				Ref: args[0], Response: answer, Comment: comment, IdempotencyKey: key,
			}))
			if err != nil {
				return err
			}
			if !g.table() {
				if err := writeProto(env.Stdout, resp.Msg); err != nil {
					return err
				}
			} else if err := writeString(env.Stdout, rsvpText(args[0], answer, resp.Msg)); err != nil {
				return err
			}
			return calendarHeldError(heldRsvp, resp.Msg.GetHeld())
		},
	}
	f := cmd.Flags()
	f.StringVar(&comment, "comment", "", "a note to the organizer, sent with the answer")
	f.StringVar(&key, "idempotency-key", "", "the key that stops a repeat from answering twice")
	return cmd
}

func newFreeBusyCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var (
		rf      rangeFlags
		account string
	)
	cmd := &cobra.Command{
		Use:   "freebusy [--account NAME] [--days N | --from T --to T] [--tz ZONE] [address...]",
		Short: "Show when James is busy across every account, and when other people are",
		Long: "Show James's busy time across every account's calendars, leaving out\n" +
			"events he declined, transparent events and cancelled ones. It names no\n" +
			"event. Each address asks the provider of the account --account names\n" +
			"for that person's busy time. A person the provider cannot see shows as\n" +
			"unknown, never as free.\n\n" + calendarRangeHelp + "\n\n" + calendarTimesHelp,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && account == "" {
				return usageError{err: errors.New("--account is required with addresses: it names whose provider to ask"), hint: helpHint(cmd)}
			}
			from, to, loc, err := rf.resolve(cmd, env)
			if err != nil {
				return err
			}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Calendar.FreeBusy(ctx, connect.NewRequest(&chainmailv1.FreeBusyRequest{
				Account: account, People: args, From: timestamppb.New(from), To: timestamppb.New(to),
			}))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return freeBusyTable(env.Stdout, loc, resp.Msg)
		},
	}
	addRangeFlags(cmd, &rf)
	cmd.Flags().StringVar(&account, "account", "", "the account whose provider is asked about the addresses")
	return cmd
}

// wallTime is the first instant at which the clocks in loc show the wall
// clock w holds (w is in UTC). Around a change of offset the clocks show
// some times twice, and this takes the earlier, which time.Date does not
// promise. It reports false when the clocks skip the time.
func wallTime(w time.Time, loc *time.Location) (time.Time, bool) {
	want := w.Format(localMinuteLayout)
	var first time.Time
	found := false
	// Every offset in force within a day of w, which takes in both sides
	// of any change near it.
	for _, probe := range []time.Time{w.Add(-24 * time.Hour), w, w.Add(24 * time.Hour)} {
		_, offset := probe.In(loc).Zone()
		t := w.Add(-time.Duration(offset) * time.Second)
		if t.In(loc).Format(localMinuteLayout) == want && (!found || t.Before(first)) {
			first, found = t, true
		}
	}
	return first.In(loc), found
}
