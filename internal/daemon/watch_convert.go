package daemon

import (
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
// and a due event its item, neither ever holding rule or item text.
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
