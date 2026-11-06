package watch

import (
	"slices"
	"strings"
)

// Filter selects events. Empty fields match everything, with two
// exceptions: item and item_mail events match only a filter that names
// them in Kinds or sets Items, and unreadable events only a filter with
// Internal set. Status and gap events pass a Kinds filter unless Kinds
// contains status or gap, and they always pass In.
type Filter struct {
	Accounts []string
	Kinds    []Kind
	// In matches the event's folder (case-insensitive), or one of its labels
	// (the message's labels for new and item_mail, Labels for flags).
	// "inbox" also matches the Gmail label \Inbox, and "sent" matches \Sent.
	// Item events always pass it.
	In string
	// Internal also matches unreadable events, which only the daemon's
	// cache reads. API readers leave it false and never see them.
	Internal bool
	// Items, when non-nil, limits item and item_mail events to these item
	// IDs and drops new, flags and expunged events. The daemon resolves it
	// (an item and every live item below it) when a read starts. Unreadable
	// events, which describe mail as new events do, are dropped too.
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
// An event with no account (an item event or a trimmed gap) passes any
// Accounts filter. A status or gap event passes a Kinds filter that names
// neither status nor gap, and always passes In, since it describes a folder
// or the log rather than a message. Item and item_mail events are opt-in:
// with Kinds empty they pass only when Items is set. With Items set, an item
// event passes only for a listed Item.ItemID, an item_mail event only for a
// listed ItemID, status and gap events pass, and every other kind is
// dropped. An item event always passes In; an item_mail event matches In
// as a new event does. Labels compare case-insensitively, as folder names do.
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
	if len(f.Kinds) == 0 && f.Items == nil && isItemKind(e.Kind) {
		return false
	}
	if f.In == "" || control || e.Kind == KindItem {
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
// a listed item, an item_mail event for a listed item, or a status or gap
// event. Every other kind describes mail that no item selects.
func (f Filter) itemListed(e Event) bool {
	switch e.Kind {
	case KindItem:
		return e.Item != nil && f.Items[e.Item.ItemID]
	case KindItemMail:
		return f.Items[e.ItemID]
	case KindStatus, KindGap:
		return true
	default:
		return false
	}
}

// isItemKind reports whether k is one of the item kinds a reader opts into.
func isItemKind(k Kind) bool {
	return k == KindItem || k == KindItemMail
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
