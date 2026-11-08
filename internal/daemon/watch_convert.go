package daemon

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/watch"
)

// eventToProto builds the API form of an event. An item event carries its
// change, never item text; an item_mail event carries its item and
// from_self besides the message row a new event has. A new event's message
// becomes a search row shaped as rowFromSummary shapes one: every string
// from the message is wrapped as untrusted and cleaned, and the row carries
// the event's ref, account and folder. The snippet was rendered by the sync
// engine with snippetOf, so it is only wrapped. A status detail is the
// daemon's own text, never the server's. A rule event carries its change
// and a due event its item, neither ever holding rule or item text. A
// calendar event carries refs, times, field names and addresses, never
// event text; its addresses are wrapped as untrusted.
func eventToProto(e watch.Event) *chainmailv1.WatchEvent {
	out := &chainmailv1.WatchEvent{
		Id: e.ID, At: timestamp(e.At), Kind: string(e.Kind),
		Account: e.Account, Folder: e.Folder, Ref: e.Ref,
		Flags: e.Flags, Labels: e.Labels,
		State: string(e.State), Detail: e.Detail,
		Reason: e.Reason, NextId: e.NextID,
	}
	if m := e.Message; m != nil {
		out.Message = &chainmailv1.MessageRow{
			Ref: e.Ref, Account: e.Account, Folder: e.Folder,
			Date: timestamp(m.Date), InternalDate: timestamp(m.InternalDate),
			From: addressesProto(m.From), To: addressesProto(m.To), Subject: untrusted(m.Subject),
			Flags: m.Flags, Labels: m.Labels, Size: m.Size, HasAttachment: m.HasAttachment, ThreadId: m.ThreadID,
		}
		if m.Snippet != nil {
			out.Message.Snippet = &chainmailv1.Untrusted{Untrusted: *m.Snippet}
		}
	}
	if c := e.Item; c != nil {
		out.Item = &chainmailv1.WorkItemChange{ItemId: c.ItemID, Type: c.Type, Revision: c.Revision, Op: c.Op, Fields: c.Fields}
	}
	if r := e.Rule; r != nil {
		out.Rule = &chainmailv1.RuleChange{RuleId: r.RuleID, Name: r.Name, Change: r.Change, Revision: r.Revision, Actor: r.Actor, Reason: r.Reason}
	}
	if d := e.Due; d != nil {
		out.Due = &chainmailv1.WorkItemDue{ItemId: d.ItemID, Type: d.Type, DueDate: d.DueDate, State: d.State, ParentId: d.ParentID}
	}
	if c := e.Calendar; c != nil {
		out.Calendar = calendarChangeProto(c)
	}
	out.ItemId, out.FromSelf = e.ItemID, e.FromSelf
	return out
}

// folderStatusProto builds the API form of a synced folder's status. The
// checkpoint stays in the daemon. A time never set stays unset.
func folderStatusProto(r watch.FolderRow) *chainmailv1.FolderSyncStatus {
	return &chainmailv1.FolderSyncStatus{
		Account: r.Account, Folder: r.Folder, Mode: string(r.Mode), State: string(r.State), Condstore: r.CondStore,
		LastSyncAt: timestamp(r.LastSyncAt), LastSweepAt: timestamp(r.LastSweepAt), LastError: r.LastError,
	}
}

// groupProto builds the API form of a named group.
func groupProto(g watch.GroupInfo) *chainmailv1.GroupStatus {
	return &chainmailv1.GroupStatus{
		Name: g.Name, AckedId: g.AckedID, Lag: g.Lag,
		CreatedAt: timestamp(g.CreatedAt), LastSeenAt: timestamp(g.LastSeenAt),
	}
}

// calendarChangeProto builds the API form of a calendar event's payload.
// The addresses come from a calendar server or an invite, so they are
// untrusted; no name is ever set. A time that does not parse is left unset.
func calendarChangeProto(c *watch.CalendarChange) *chainmailv1.CalendarChange {
	return &chainmailv1.CalendarChange{
		Ref: c.Ref, MessageRef: c.MessageRef, CalendarId: c.CalendarID,
		Start: rfc3339Proto(c.Start), End: rfc3339Proto(c.End),
		Organizer: emailProto(c.Organizer), Fields: c.Fields,
		Attendee: emailProto(c.Attendee), Partstat: c.PartStat,
	}
}

// rfc3339Proto parses an RFC 3339 time, nil when s is empty or invalid.
func rfc3339Proto(s string) *timestamppb.Timestamp {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return timestamppb.New(t)
}

// emailProto is an address with only its email, nil when addr is empty.
func emailProto(addr string) *chainmailv1.Address {
	if addr == "" {
		return nil
	}
	return &chainmailv1.Address{Email: untrusted(addr)}
}
