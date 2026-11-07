package watch

import (
	"slices"
	"strings"
)

// Filter selects events. Empty fields match everything, with two
// exceptions: the opt-in kinds (item, item_mail, rule, item_due_soon,
// item_due and item_overdue) match only a filter that names them in Kinds
// or sets Items, and unreadable events only a filter with Internal set.
// Status and gap events pass a Kinds filter unless Kinds contains status or
// gap, and they always pass In.
type Filter struct {
	Accounts []string
	Kinds    []Kind
	// In matches the event's folder (case-insensitive), or one of its labels
	// (the message's labels for new and item_mail, Labels for flags).
	// "inbox" also matches the Gmail label \Inbox, and "sent" matches \Sent.
	// Item, rule and due events, which have no folder, always pass it.
	In string
	// Internal also matches unreadable events, which only the daemon's
	// cache reads. API readers leave it false and never see them.
	Internal bool
	// Items, when non-nil, limits item, item_mail and due events to these
	// item IDs and drops new, flags, expunged and rule events. The daemon
	// resolves it (an item and every live item below it) when a read
	// starts. Unreadable events, which describe mail as new events do, are
	// dropped too.
	Items map[string]bool
}

// Gmail system labels the In aliases match.
const (
	labelInbox = `\Inbox`
	labelSent  = `\Sent`
)

// Match reports whether e passes the filter.
//
// An unreadable event passes only a filter with Internal set, and then as
// a new event would.
// An event with no account (an item, rule or due event, or a trimmed gap)
// passes any Accounts filter. A status or gap event passes a Kinds filter
// that names neither status nor gap, and always passes In, since it
// describes a folder or the log rather than a message. The opt-in kinds
// (item, item_mail, rule and the three due kinds) pass a filter with Kinds
// empty only when Items is set. With Items set, an item event passes only
// for a listed Item.ItemID, an item_mail event only for a listed ItemID, a
// due event only for a listed Due.ItemID, status and gap events pass, and
// every other kind, rule included, is dropped. Item, rule and due events
// always pass In; an item_mail event matches In as a new event does. Labels
// compare case-insensitively, as folder names do.
func (f Filter) Match(e Event) bool {
	if e.Kind == KindUnreadable && !f.Internal {
		return false
	}
	if len(f.Accounts) > 0 && e.Account != "" && !slices.Contains(f.Accounts, e.Account) {
		return false
	}
	if f.Items != nil && !f.itemListed(e) {
		return false
	}
	control := e.Kind == KindStatus || e.Kind == KindGap
	if len(f.Kinds) > 0 && !slices.Contains(f.Kinds, e.Kind) {
		if !control || slices.Contains(f.Kinds, KindStatus) || slices.Contains(f.Kinds, KindGap) {
			return false
		}
	}
	if len(f.Kinds) == 0 && f.Items == nil && optInKind(e.Kind) {
		return false
	}
	if f.In == "" || control || folderless(e.Kind) {
		return true
	}
	if strings.EqualFold(e.Folder, f.In) {
		return true
	}
	var labels []string
	switch e.Kind {
	case KindNew, KindItemMail:
		if e.Message != nil {
			labels = e.Message.Labels
		}
	case KindFlags:
		labels = e.Labels
	}
	return slices.ContainsFunc(labels, func(l string) bool { return labelMatches(f.In, l) })
}

// itemListed reports whether e passes a set Items filter: an item event of
// a listed item, an item_mail or due event for a listed item, or a status
// or gap event. Every other kind describes mail, or a rule, that no item
// selects.
func (f Filter) itemListed(e Event) bool {
	switch e.Kind {
	case KindItem:
		return e.Item != nil && f.Items[e.Item.ItemID]
	case KindItemMail:
		return f.Items[e.ItemID]
	case KindItemDueSoon, KindItemDue, KindItemOverdue:
		return e.Due != nil && f.Items[e.Due.ItemID]
	case KindStatus, KindGap:
		return true
	default:
		return false
	}
}

// optInKind reports whether k is a kind a reader opts into: item,
// item_mail, rule, item_due_soon, item_due or item_overdue. A filter that
// names no kinds and sets no items never matches them, so the cache's
// readers and every watcher that asks for all kinds see only mail events.
func optInKind(k Kind) bool {
	switch k {
	case KindItem, KindItemMail, KindRule, KindItemDueSoon, KindItemDue, KindItemOverdue:
		return true
	default:
		return false
	}
}

// folderless reports whether events of kind k have no folder (item, rule
// and the due kinds), so an In filter, which names a folder or a label,
// never drops them.
func folderless(k Kind) bool {
	switch k {
	case KindItem, KindRule, KindItemDueSoon, KindItemDue, KindItemOverdue:
		return true
	default:
		return false
	}
}

// labelMatches reports whether label is the label in names, by name or
// through the inbox and sent aliases.
func labelMatches(in, label string) bool {
	switch {
	case strings.EqualFold(label, in):
		return true
	case strings.EqualFold(in, "inbox"):
		return strings.EqualFold(label, labelInbox)
	case strings.EqualFold(in, "sent"):
		return strings.EqualFold(label, labelSent)
	}
	return false
}
