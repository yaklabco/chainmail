package watch

import (
	"slices"
	"strings"
)

// Filter selects events. Empty fields match everything. Status and gap
// events pass a Kinds filter unless Kinds contains status or gap, and they
// always pass In.
type Filter struct {
	Accounts []string
	Kinds    []Kind
	// In matches the event's folder (case-insensitive), or one of its labels
	// (the message's labels for new, Labels for flags). "inbox" also matches
	// the Gmail label \Inbox, and "sent" matches \Sent.
	In string
}

// Gmail system labels the In aliases match.
const (
	labelInbox = `\Inbox`
	labelSent  = `\Sent`
)

// Match reports whether e passes the filter.
//
// An event with no account (a trimmed gap) passes any Accounts filter. A
// status or gap event passes a Kinds filter that names neither status nor
// gap, and always passes In, since it describes a folder or the log rather
// than a message. Labels compare case-insensitively, as folder names do.
func (f Filter) Match(e Event) bool {
	if len(f.Accounts) > 0 && e.Account != "" && !slices.Contains(f.Accounts, e.Account) {
		return false
	}
	control := e.Kind == KindStatus || e.Kind == KindGap
	if len(f.Kinds) > 0 && !slices.Contains(f.Kinds, e.Kind) {
		if !control || slices.Contains(f.Kinds, KindStatus) || slices.Contains(f.Kinds, KindGap) {
			return false
		}
	}
	if f.In == "" || control {
		return true
	}
	if strings.EqualFold(e.Folder, f.In) {
		return true
	}
	var labels []string
	switch e.Kind {
	case KindNew:
		if e.Message != nil {
			labels = e.Message.Labels
		}
	case KindFlags:
		labels = e.Labels
	}
	return slices.ContainsFunc(labels, func(l string) bool { return labelMatches(f.In, l) })
}

// labelMatches reports whether the label is the one in names, either by
// name or through the inbox and sent aliases.
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
