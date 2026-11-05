package watch

import "testing"

func TestFilterMatch(t *testing.T) {
	t.Parallel()
	inbox := Event{Kind: KindNew, Account: "a", Folder: "INBOX", Message: &Message{}}
	gmailNew := Event{Kind: KindNew, Account: "g", Folder: "[Gmail]/All Mail", Message: &Message{Labels: []string{`\Inbox`, "Receipts"}}}
	gmailSent := Event{Kind: KindNew, Account: "g", Folder: "[Gmail]/All Mail", Message: &Message{Labels: []string{`\Sent`}}}
	gmailFlags := Event{Kind: KindFlags, Account: "g", Folder: "[Gmail]/All Mail", Labels: []string{"Receipts"}}
	gmailArchived := Event{Kind: KindFlags, Account: "g", Folder: "[Gmail]/All Mail"}
	expunged := Event{Kind: KindExpunged, Account: "a", Folder: "Archive"}
	status := Event{Kind: KindStatus, Account: "a", Folder: "Sent", State: StateWatching}
	uidGap := Event{Kind: KindGap, Account: "a", Folder: "INBOX", Reason: GapUIDValidity}
	trimmed := Event{Kind: KindGap, Reason: GapTrimmed, NextID: 9}
	unreadable := Event{Kind: KindUnreadable, Account: "a", Folder: "INBOX"}

	tests := []struct {
		name string
		f    Filter
		e    Event
		want bool
	}{
		{"empty filter", Filter{}, inbox, true},
		{"account listed", Filter{Accounts: []string{"b", "a"}}, inbox, true},
		{"account not listed", Filter{Accounts: []string{"b"}}, inbox, false},
		{"status account not listed", Filter{Accounts: []string{"b"}}, status, false},
		{"trimmed gap passes accounts", Filter{Accounts: []string{"b"}}, trimmed, true},
		{"kind listed", Filter{Kinds: []Kind{KindFlags, KindNew}}, inbox, true},
		{"kind not listed", Filter{Kinds: []Kind{KindFlags}}, inbox, false},
		{"expunged not listed", Filter{Kinds: []Kind{KindNew}}, expunged, false},
		{"status passes unnamed", Filter{Kinds: []Kind{KindNew}}, status, true},
		{"gap passes unnamed", Filter{Kinds: []Kind{KindNew}}, uidGap, true},
		{"trimmed gap passes unnamed", Filter{Kinds: []Kind{KindNew}}, trimmed, true},
		{"status named", Filter{Kinds: []Kind{KindNew, KindStatus}}, status, true},
		{"gap dropped when status named", Filter{Kinds: []Kind{KindNew, KindStatus}}, uidGap, false},
		{"status dropped when gap named", Filter{Kinds: []Kind{KindGap}}, status, false},
		{"gap named", Filter{Kinds: []Kind{KindGap}}, trimmed, true},
		{"in folder", Filter{In: "INBOX"}, inbox, true},
		{"in folder case-insensitive", Filter{In: "inbox"}, inbox, true},
		{"in other folder", Filter{In: "Archive"}, inbox, false},
		{"in folder for expunged", Filter{In: "archive"}, expunged, true},
		{"in label of new", Filter{In: "Receipts"}, gmailNew, true},
		{"in label case-insensitive", Filter{In: "receipts"}, gmailNew, true},
		{"in label of flags", Filter{In: "Receipts"}, gmailFlags, true},
		{"in missing label", Filter{In: "Travel"}, gmailNew, false},
		{"inbox alias", Filter{In: "inbox"}, gmailNew, true},
		{"inbox alias upper", Filter{In: "INBOX"}, gmailNew, true},
		{"inbox alias not sent", Filter{In: "inbox"}, gmailSent, false},
		{"sent alias", Filter{In: "sent"}, gmailSent, true},
		{"sent alias not inbox", Filter{In: "sent"}, gmailNew, false},
		{"archived flags drop out of inbox", Filter{In: "inbox"}, gmailArchived, false},
		{"in all mail folder", Filter{In: "[gmail]/all mail"}, gmailArchived, true},
		{"status passes in", Filter{In: "INBOX"}, status, true},
		{"gap passes in", Filter{In: "Archive"}, uidGap, true},
		{"all fields", Filter{Accounts: []string{"g"}, Kinds: []Kind{KindNew}, In: "inbox"}, gmailNew, true},
		{"all fields one fails", Filter{Accounts: []string{"g"}, Kinds: []Kind{KindFlags}, In: "inbox"}, gmailNew, false},
		{"new without message", Filter{In: "Receipts"}, Event{Kind: KindNew, Folder: "INBOX"}, false},
		{"unreadable hidden from the API", Filter{}, unreadable, false},
		{"unreadable hidden even when named", Filter{Kinds: []Kind{KindUnreadable}}, unreadable, false},
		{"unreadable internal", Filter{Internal: true}, unreadable, true},
		{"unreadable internal account listed", Filter{Internal: true, Accounts: []string{"a"}}, unreadable, true},
		{"unreadable internal account not listed", Filter{Internal: true, Accounts: []string{"b"}}, unreadable, false},
		{"unreadable internal in folder", Filter{Internal: true, In: "inbox"}, unreadable, true},
		{"unreadable internal in other folder", Filter{Internal: true, In: "Archive"}, unreadable, false},
		{"internal still filters kinds", Filter{Internal: true, Kinds: []Kind{KindNew}}, unreadable, false},
		{"internal passes other kinds", Filter{Internal: true}, inbox, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.f.Match(tt.e); got != tt.want {
				t.Errorf("%+v.Match(%+v) = %v, want %v", tt.f, tt.e, got, tt.want)
			}
		})
	}
}
