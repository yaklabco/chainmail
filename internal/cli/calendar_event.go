package cli

import (
	"cmp"
	"context"
	"encoding/base32"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/client"
	"github.com/yaklabco/chainmail/internal/config"
)

const eventHelp = `Show one event, or one instance of a recurring series, by its ce_ ref.
Refs come from agenda, invites and event results; pass them unchanged. A
ref with @<recurrence ID> names one instance, and a plain ref the whole
series.

event create, edit and cancel change calendars under the send policy.
Attendees other than James are told of a change, so they are checked
against the policy. A create whose attendees it does not allow makes a
draft event that notifies no one, and exits 4: send the invitations from
the calendar app. An edit or cancel it does not allow is not applied,
and exits 4. Only the organizer changes an event with other attendees.

` + calendarTextHelp

const eventCreateHelp = `Create an event on an account's calendar, the default one unless
--calendar names another. Each --attendee is invited.

--all-day takes dates: --start is the first day and --end the last, or
--duration a number of days such as 2d; one day by default. The event
takes the dates typed, in any zone: the CLI sends them as 00:00 UTC of
those dates, as the daemon stores all-day events. Otherwise --end or
--duration, such as 30m or 1h30m, is required.

--tz names a timed event's zone as well as the zone times are read in.
Without it the event takes the daemon's [calendar] timezone.

Running the same create again within 7 days returns the event already
created instead of creating it twice: without --idempotency-key, the key
is derived from the account, calendar and every field. The attendees are
sent sorted, and an address given twice in different case once, so their
order does not change the key. To create the same
event again on purpose, pass a new --idempotency-key.

` + calendarTimesHelp

const eventEditHelp = `Change the fields of an event the flags name, and only those. --start
alone moves the event and keeps its length. --duration alone keeps the
start. --attendee replaces the whole attendee list, and --no-attendees
clears it. An empty value, such as --location "", clears that field; an
empty --rrule removes the recurrence.

An all-day event stays all day: its --start and --end take dates, --end
the last day, and --duration whole days such as 2d. To read whether the
event is all day, a change of times reads the event first. --all-day
with dates makes a timed event all day, and --timed with times and
--end or --duration makes an all-day event timed.

The change is refused with EVENT_CONFLICT when the event changed since
the etag was read. Without --etag the CLI reads the event first and uses
the etag it has now.

` + calendarTimesHelp

const eventCancelHelp = `Cancel an event James organizes and tell its attendees, or delete an
event with no other attendees. The cancel is refused with EVENT_CONFLICT
when the event changed since the etag was read. Without --etag the CLI
reads the event first and uses the etag it has now.`

// eventFlags are the field flags event create and event edit share.
type eventFlags struct {
	title, start, end, duration, tz, location, description, rrule string
	attendees                                                     []string
	allDay                                                        bool
	timed                                                         bool // event edit only
}

func addEventFlags(cmd *cobra.Command, ef *eventFlags) {
	f := cmd.Flags()
	f.StringVar(&ef.title, "title", "", "the event's title")
	f.StringVar(&ef.start, "start", "", "when the event starts, or its first day with --all-day")
	f.StringVar(&ef.end, "end", "", "when it ends; a date alone takes in that whole day")
	f.StringVar(&ef.duration, "duration", "", "how long it lasts, such as 30m, 1h30m or 2d, instead of --end")
	f.BoolVar(&ef.allDay, "all-day", false, "an all-day event, with dates for --start and --end")
	f.StringArrayVar(&ef.attendees, "attendee", nil, "an attendee's address; repeat for more (sent sorted, and once when addresses differ only in case)")
	f.StringVar(&ef.location, "location", "", "where it takes place")
	f.StringVar(&ef.description, "description", "", "the event's description")
	f.StringVar(&ef.rrule, "rrule", "", "a recurrence rule, such as FREQ=WEEKLY;BYDAY=MO")
	f.StringVar(&ef.tz, "tz", "", "the event's IANA zone, also used to read times without a zone (default: this machine's zone for reading, the daemon's [calendar] timezone for the event)")
}

// dayRule says whether --start and --end take all-day dates, and if so
// why, for the usage error a time gets. The empty rule takes times.
type dayRule string

const (
	timedTimes dayRule = ""
	allDayFlag dayRule = "--all-day takes dates"
	allDayKept dayRule = "the event is all day, so it takes dates (add --timed to give it times)"
)

// parseStart reads --start. An all-day start must be a date, and is
// 00:00 UTC of that date, as the wire carries all-day dates.
func (ef *eventFlags) parseStart(cmd *cobra.Command, loc *time.Location, rule dayRule) (time.Time, error) {
	allDay := rule != timedTimes
	ct, err := parseCalTime(cmd, "start", ef.start, loc)
	if err != nil {
		return time.Time{}, err
	}
	if allDay && !ct.dateOnly {
		return time.Time{}, usageError{err: errors.New(string(rule) + ": give --start as a date, such as 2026-11-10"), hint: helpHint(cmd)}
	}
	if allDay {
		return ct.date, nil
	}
	return ct.t, nil
}

// parseEnd reads --end or --duration after start. It reports false when
// neither was given.
func (ef *eventFlags) parseEnd(cmd *cobra.Command, start time.Time, loc *time.Location, rule dayRule) (time.Time, bool, error) {
	allDay := rule != timedTimes
	usage := func(msg string) error { return usageError{err: errors.New(msg), hint: helpHint(cmd)} }
	f := cmd.Flags()
	switch {
	case f.Changed("end"):
		ct, err := parseCalTime(cmd, "end", ef.end, loc)
		if err != nil {
			return time.Time{}, false, err
		}
		if allDay && !ct.dateOnly {
			return time.Time{}, false, usage(string(rule) + ": give --end as the last day, such as 2026-11-12")
		}
		end := ct.t
		if ct.dateOnly {
			end = nextDay(ct.t)
		}
		if allDay {
			end = nextDay(ct.date)
		}
		if !end.After(start) {
			return time.Time{}, false, usage("--end must be after --start")
		}
		return end, true, nil
	case f.Changed("duration"):
		d, err := config.ParseCacheDuration(ef.duration)
		if err != nil {
			return time.Time{}, false, usage(fmt.Sprintf("--duration %q: want a duration such as 30m, 1h30m or 2d", ef.duration))
		}
		if d <= 0 {
			return time.Time{}, false, usage("--duration must be positive")
		}
		if allDay {
			const day = 24 * time.Hour
			if d%day != 0 {
				return time.Time{}, false, usage("--duration of an all-day event is whole days, such as 1d or 3d")
			}
			// AddDate keeps midnight across a change of daylight saving time.
			return start.AddDate(0, 0, int(d/day)), true, nil
		}
		return start.Add(d), true, nil
	}
	return time.Time{}, false, nil
}

// endConflict refuses --end with --duration.
func endConflict(cmd *cobra.Command) error {
	if cmd.Flags().Changed("end") && cmd.Flags().Changed("duration") {
		return usageError{err: errors.New("--end and --duration cannot be used together"), hint: helpHint(cmd)}
	}
	return nil
}

func newEventCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "event <ref>",
		Short: "Show an event with its attendees; event create, edit and cancel change one",
		Long:  eventHelp,
		Args:  exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Calendar.GetEvent(ctx, connect.NewRequest(&chainmailv1.GetEventRequest{Ref: args[0]}))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return eventViewText(env.Stdout, env.location(), resp.Msg.GetEvent())
		},
	}
	cmd.AddCommand(newEventCreateCmd(ctx, env, g), newEventEditCmd(ctx, env, g), newEventCancelCmd(ctx, env, g))
	return cmd
}

func newEventCreateCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var (
		ef                     eventFlags
		account, calendar, key string
	)
	cmd := &cobra.Command{
		Use:   "create --title T --start T (--end T | --duration D) [--all-day] [--attendee ADDR]...",
		Short: "Create an event; attendees the policy does not allow make it a draft that invites no one",
		Long:  eventCreateHelp,
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			req, err := ef.createRequest(cmd, env)
			if err != nil {
				return err
			}
			req.Account, req.CalendarId, req.IdempotencyKey = account, calendar, key
			if key == "" {
				req.IdempotencyKey = deriveEventKey(req)
			}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Calendar.CreateEvent(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			if !g.table() {
				if err := writeProto(env.Stdout, resp.Msg); err != nil {
					return err
				}
			} else if err := writeString(env.Stdout, eventCreatedText(env.location(), resp.Msg)); err != nil {
				return err
			}
			return calendarHeldError(heldCreate, resp.Msg.GetHeld())
		},
	}
	addEventFlags(cmd, &ef)
	f := cmd.Flags()
	f.StringVar(&account, "account", "", "the account whose calendar holds the event (default: the daemon's choice)")
	f.StringVar(&calendar, "calendar", "", "the calendar ID (default: the account's default calendar)")
	f.StringVar(&key, "idempotency-key", "", "the key that stops a repeat from creating twice (default: derived from the event)")
	return cmd
}

// createRequest builds a CreateEventRequest from the field flags, without
// the account, calendar and key.
func (ef *eventFlags) createRequest(cmd *cobra.Command, env Env) (*chainmailv1.CreateEventRequest, error) {
	usage := func(msg string) error { return usageError{err: errors.New(msg), hint: helpHint(cmd)} }
	switch {
	case ef.title == "":
		return nil, usage("--title is required")
	case ef.start == "":
		return nil, usage("--start is required")
	}
	if err := endConflict(cmd); err != nil {
		return nil, err
	}
	loc, err := calZone(cmd, env, ef.tz)
	if err != nil {
		return nil, err
	}
	rule := timedTimes
	if ef.allDay {
		rule = allDayFlag
	}
	start, err := ef.parseStart(cmd, loc, rule)
	if err != nil {
		return nil, err
	}
	end, ok, err := ef.parseEnd(cmd, start, loc, rule)
	switch {
	case err != nil:
		return nil, err
	case !ok && ef.allDay:
		end = nextDay(start)
	case !ok:
		return nil, usage("give --end or --duration")
	}
	return &chainmailv1.CreateEventRequest{
		Title: ef.title, Start: timestamppb.New(start), End: timestamppb.New(end), TimeZone: ef.tz, AllDay: ef.allDay,
		Location: ef.location, Description: ef.description, Attendees: canonicalAttendees(ef.attendees), RecurrenceRule: ef.rrule,
	}, nil
}

// deriveEventKey returns "cli-" and the unpadded base32 SHA-256 of every
// field of req but the key. Attendees are in canonical order, as the
// request sends them, so their order does not matter, and the fields are
// labelled so no create shares a key with a send.
func deriveEventKey(req *chainmailv1.CreateEventRequest) string {
	h := newKeyHash()
	h.str("mode", "event-create")
	h.str("account", req.GetAccount())
	h.str("calendar", req.GetCalendarId())
	h.str("title", req.GetTitle())
	h.str("start", req.GetStart().AsTime().UTC().Format(time.RFC3339Nano))
	h.str("end", req.GetEnd().AsTime().UTC().Format(time.RFC3339Nano))
	h.str("time_zone", req.GetTimeZone())
	h.str("all_day", strconv.FormatBool(req.GetAllDay()))
	h.str("location", req.GetLocation())
	h.str("description", req.GetDescription())
	attendees := canonicalAttendees(req.GetAttendees())
	h.str("attendees", strconv.Itoa(len(attendees)))
	for _, a := range attendees {
		h.str("attendee", a)
	}
	h.str("recurrence_rule", req.GetRecurrenceRule())
	sum := h.sum()
	return keyPrefix + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
}

// eventMaskOrder is the order of UpdateEvent's mask paths.
var eventMaskOrder = []string{"title", "start", "end", "time_zone", "all_day", "location", "description", "attendees", "recurrence_rule"}

// eventFlagPaths maps each of event edit's field flags to the mask path it
// changes. --start and --duration may set end as well.
var eventFlagPaths = map[string]string{
	"title": "title", "start": "start", "end": "end", "duration": "end", "tz": "time_zone", "all-day": "all_day", "timed": "all_day",
	"location": "location", "description": "description", "attendee": "attendees", "no-attendees": "attendees", "rrule": "recurrence_rule",
}

// eventReader reads the event an edit or cancel acts on at most once, and
// only when it is needed.
type eventReader struct {
	ctx context.Context
	c   *client.Client
	ref string
	ev  *chainmailv1.CalendarEvent
}

func (r *eventReader) get() (*chainmailv1.CalendarEvent, error) {
	if r.ev != nil {
		return r.ev, nil
	}
	resp, err := r.c.Calendar.GetEvent(r.ctx, connect.NewRequest(&chainmailv1.GetEventRequest{Ref: r.ref}))
	if err != nil {
		return nil, err
	}
	r.ev = resp.Msg.GetEvent()
	return r.ev, nil
}

// etag is etag when given, else the etag the event has now.
func (r *eventReader) etag(etag string) (string, error) {
	if etag != "" {
		return etag, nil
	}
	ev, err := r.get()
	if err != nil {
		return "", err
	}
	return ev.GetEtag(), nil
}

func newEventEditCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var (
		ef          eventFlags
		etag        string
		noAttendees bool
	)
	cmd := &cobra.Command{
		Use:   "edit <ref> [field flags] [--etag ETAG]",
		Short: "Change an event's fields; a change the policy does not allow is not applied",
		Long:  eventEditHelp,
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths, err := ef.editPaths(cmd, noAttendees)
			if err != nil {
				return err
			}
			loc, err := calZone(cmd, env, ef.tz)
			if err != nil {
				return err
			}
			c := g.client(env)
			defer c.Close()
			r := &eventReader{ctx: ctx, c: c, ref: args[0]}
			req := &chainmailv1.UpdateEventRequest{
				Ref: args[0], Title: ef.title, TimeZone: ef.tz, Location: ef.location, Description: ef.description,
				RecurrenceRule: ef.rrule,
			}
			_, req.AllDay = ef.allDayChange(cmd)
			if !noAttendees {
				req.Attendees = canonicalAttendees(ef.attendees)
			}
			if paths, err = ef.editTimes(cmd, r, loc, req, paths); err != nil {
				return err
			}
			if req.Etag, err = r.etag(etag); err != nil {
				return err
			}
			req.UpdateMask = &fieldmaskpb.FieldMask{Paths: paths}
			resp, err := c.Calendar.UpdateEvent(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			if !g.table() {
				if err := writeProto(env.Stdout, resp.Msg); err != nil {
					return err
				}
			} else if err := writeString(env.Stdout, changedText("updated", args[0], resp.Msg.GetHeld())); err != nil {
				return err
			}
			return calendarHeldError(heldChange, resp.Msg.GetHeld())
		},
	}
	addEventFlags(cmd, &ef)
	f := cmd.Flags()
	f.BoolVar(&noAttendees, "no-attendees", false, "remove every attendee")
	f.BoolVar(&ef.timed, "timed", false, "make an all-day event a timed one, with times for --start and --end or --duration")
	f.StringVar(&etag, "etag", "", "refuse the change if the event no longer has this etag (default: the etag it has now)")
	return cmd
}

// editPaths checks event edit's flags and returns the mask paths they
// name, in mask order. Nothing to change is a usage error.
func (ef *eventFlags) editPaths(cmd *cobra.Command, noAttendees bool) ([]string, error) {
	usage := func(msg string) error { return usageError{err: errors.New(msg), hint: helpHint(cmd)} }
	f := cmd.Flags()
	switch {
	case noAttendees && f.Changed("attendee"):
		return nil, usage("--attendee and --no-attendees cannot be used together")
	case f.Changed("all-day") && f.Changed("timed"):
		return nil, usage("--all-day and --timed cannot be used together")
	case f.Changed("all-day") && !f.Changed("start"):
		return nil, usage("--all-day needs --start: give the event's new dates or times")
	case f.Changed("timed") && !f.Changed("start"):
		return nil, usage("--timed needs --start: give the event's new times")
	}
	if err := endConflict(cmd); err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for name, path := range eventFlagPaths {
		if f.Changed(name) && (name != "no-attendees" || noAttendees) {
			set[path] = true
		}
	}
	paths := slices.DeleteFunc(slices.Clone(eventMaskOrder), func(p string) bool { return !set[p] })
	if len(paths) == 0 {
		return nil, usage("nothing to change: give a field flag")
	}
	return paths, nil
}

// allDayChange reports whether event edit's --all-day or --timed sets the
// event's all-day state, and to what.
func (ef *eventFlags) allDayChange(cmd *cobra.Command) (set, allDay bool) {
	f := cmd.Flags()
	switch {
	case f.Changed("all-day"):
		return true, ef.allDay
	case f.Changed("timed"):
		return true, !ef.timed
	}
	return false, false
}

// editTimes sets the start and end of req from --start, --end and
// --duration, and returns paths with end added when the change sets it.
//
// The event keeps its all-day state unless --all-day or --timed changes
// it, so a change of times reads the event to learn that state, unless one
// of those flags gives it. An all-day event takes dates and whole days,
// counted as calendar days and sent as 00:00 UTC of the dates. Without
// --end or --duration, a new --start keeps the event's length.
func (ef *eventFlags) editTimes(cmd *cobra.Command, r *eventReader, loc *time.Location, req *chainmailv1.UpdateEventRequest, paths []string) ([]string, error) {
	f := cmd.Flags()
	if !f.Changed("start") && !f.Changed("end") && !f.Changed("duration") {
		return paths, nil
	}
	set, allDay := ef.allDayChange(cmd)
	toTimed, toAllDay := set && !allDay, set && allDay
	rule := timedTimes
	switch {
	case toAllDay:
		rule = allDayFlag
	case !toTimed:
		ev, err := r.get()
		if err != nil {
			return nil, err
		}
		if ev.GetAllDay() {
			rule = allDayKept
		}
	}
	var start time.Time
	if f.Changed("start") {
		t, err := ef.parseStart(cmd, loc, rule)
		if err != nil {
			return nil, err
		}
		start = t
		req.Start = timestamppb.New(start)
	} else {
		// A new end or duration alone counts from the start the event has.
		ev, err := r.get()
		if err != nil {
			return nil, err
		}
		start = ev.GetStart().AsTime()
	}
	end, ok, err := ef.parseEnd(cmd, start, loc, rule)
	switch {
	case err != nil:
		return nil, err
	case !ok && toTimed:
		return nil, usageError{err: errors.New("give --end or --duration with --timed: the event's new length"), hint: helpHint(cmd)}
	case !ok && toAllDay:
		end = nextDay(start)
	case !ok:
		// --start alone moves the event and keeps its length.
		ev, err := r.get()
		if err != nil {
			return nil, err
		}
		length := ev.GetEnd().AsTime().Sub(ev.GetStart().AsTime())
		end = start.Add(length)
		if rule != timedTimes {
			// All-day dates are UTC midnights, so the length is whole days.
			end = start.AddDate(0, 0, max(1, int(length.Round(24*time.Hour)/(24*time.Hour))))
		}
	}
	req.End = timestamppb.New(end)
	if !slices.Contains(paths, "end") {
		paths = slices.Insert(paths, slices.Index(paths, "start")+1, "end")
	}
	return paths, nil
}

func newEventCancelCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var etag string
	cmd := &cobra.Command{
		Use:   "cancel <ref> [--etag ETAG]",
		Short: "Cancel an event and tell its attendees, or delete one with no other attendees",
		Long:  eventCancelHelp,
		Args:  exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := g.client(env)
			defer c.Close()
			r := &eventReader{ctx: ctx, c: c, ref: args[0]}
			tag, err := r.etag(etag)
			if err != nil {
				return err
			}
			resp, err := c.Calendar.CancelEvent(ctx, connect.NewRequest(&chainmailv1.CancelEventRequest{Ref: args[0], Etag: tag}))
			if err != nil {
				return err
			}
			if !g.table() {
				if err := writeProto(env.Stdout, resp.Msg); err != nil {
					return err
				}
			} else {
				verb := "cancelled"
				if !resp.Msg.GetCancelled() {
					verb = "not cancelled"
				}
				if err := writeString(env.Stdout, changedText(verb, args[0], resp.Msg.GetHeld())); err != nil {
					return err
				}
			}
			return calendarHeldError(heldChange, resp.Msg.GetHeld())
		},
	}
	cmd.Flags().StringVar(&etag, "etag", "", "refuse the cancel if the event no longer has this etag (default: the etag it has now)")
	return cmd
}

// canonicalAttendees is the attendee list a request sends: sorted without
// regard to case, and with addresses that differ only in case kept once,
// as the one that sorts first. Any order of the same addresses gives the
// same list, so a derived key and the request it goes with agree.
func canonicalAttendees(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b string) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a), strings.ToLower(b)), cmp.Compare(a, b))
	})
	return slices.CompactFunc(out, strings.EqualFold)
}
