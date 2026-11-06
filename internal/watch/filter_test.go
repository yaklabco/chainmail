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
	item := Event{Kind: KindItem, Item: &ItemChange{ItemID: "it_a", Type: "task", Revision: 4, Op: "updated", Fields: []string{"state"}}}
	itemB := Event{Kind: KindItem, Item: &ItemChange{ItemID: "it_b", Type: "epic", Revision: 5, Op: "created"}}
	itemMail := Event{Kind: KindItemMail, Account: "a", Folder: "INBOX", Ref: "cm:a:INBOX:1:5", ItemID: "it_a", Message: &Message{}}
	itemMailB := Event{Kind: KindItemMail, Account: "a", Folder: "INBOX", Ref: "cm:a:INBOX:1:6", ItemID: "it_b", Message: &Message{}}
	gmailItemMail := Event{Kind: KindItemMail, Account: "g", Folder: "[Gmail]/All Mail", ItemID: "it_a", Message: &Message{Labels: []string{`\Inbox`, "Receipts"}}}
	onlyA := map[string]bool{"it_a": true}

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

		// Item kinds are opt-in: no kinds and no item filter hides them.
		{"item hidden by empty kinds", Filter{}, item, false},
		{"item_mail hidden by empty kinds", Filter{}, itemMail, false},
		{"item hidden with only accounts", Filter{Accounts: []string{"a"}}, item, false},
		{"item hidden with only in", Filter{In: "INBOX"}, item, false},
		{"item hidden from the cache reader", Filter{Accounts: []string{"a"}, Internal: true}, item, false},
		{"item_mail hidden from the cache reader", Filter{Accounts: []string{"a"}, Internal: true}, itemMail, false},
		{"item hidden from an internal reader", Filter{Internal: true}, item, false},

		// Named item kinds.
		{"item named", Filter{Kinds: []Kind{KindItem}}, item, true},
		{"item_mail named", Filter{Kinds: []Kind{KindItemMail}}, itemMail, true},
		{"item_mail not named", Filter{Kinds: []Kind{KindItem}}, itemMail, false},
		{"item not named", Filter{Kinds: []Kind{KindNew}}, item, false},
		{"item_mail not named beside new", Filter{Kinds: []Kind{KindNew, KindFlags}}, itemMail, false},
		{"new not named beside item kinds", Filter{Kinds: []Kind{KindItem, KindItemMail}}, inbox, false},
		{"status passes item kinds", Filter{Kinds: []Kind{KindItem}}, status, true},
		{"gap passes item kinds", Filter{Kinds: []Kind{KindItemMail}}, uidGap, true},
		{"status dropped when gap named beside item", Filter{Kinds: []Kind{KindItem, KindGap}}, status, false},
		{"unreadable hidden beside item kinds", Filter{Kinds: []Kind{KindItem, KindUnreadable}}, unreadable, false},

		// The item filter, with no kinds.
		{"item filter passes its item", Filter{Items: onlyA}, item, true},
		{"item filter drops another item", Filter{Items: onlyA}, itemB, false},
		{"item filter passes its item_mail", Filter{Items: onlyA}, itemMail, true},
		{"item filter drops another item_mail", Filter{Items: onlyA}, itemMailB, false},
		{"item filter drops new", Filter{Items: onlyA}, inbox, false},
		{"item filter drops flags", Filter{Items: onlyA}, gmailFlags, false},
		{"item filter drops expunged", Filter{Items: onlyA}, expunged, false},
		{"item filter passes status", Filter{Items: onlyA}, status, true},
		{"item filter passes gap", Filter{Items: onlyA}, uidGap, true},
		{"item filter passes trimmed gap", Filter{Items: onlyA}, trimmed, true},
		{"item filter needs a change", Filter{Items: onlyA}, Event{Kind: KindItem}, false},
		{"item filter drops unreadable", Filter{Internal: true, Items: onlyA}, unreadable, false},
		{"empty item filter drops every item", Filter{Items: map[string]bool{}}, item, false},
		{"empty item filter passes status", Filter{Items: map[string]bool{}}, status, true},

		// The item filter with kinds.
		{"item filter and kind item", Filter{Kinds: []Kind{KindItem}, Items: onlyA}, item, true},
		{"item filter and kind item drops item_mail", Filter{Kinds: []Kind{KindItem}, Items: onlyA}, itemMail, false},
		{"item filter and both kinds drops another item", Filter{Kinds: []Kind{KindItem, KindItemMail}, Items: onlyA}, itemB, false},
		{"item filter and kind new drops new", Filter{Kinds: []Kind{KindNew}, Items: onlyA}, inbox, false},
		{"item filter and kind new drops item", Filter{Kinds: []Kind{KindNew}, Items: onlyA}, item, false},
		{"item filter and kind item passes status", Filter{Kinds: []Kind{KindItem}, Items: onlyA}, status, true},

		// Accounts and In.
		{"item passes in", Filter{Kinds: []Kind{KindItem}, In: "INBOX"}, item, true},
		{"item passes in with the item filter", Filter{Items: onlyA, In: "Archive"}, item, true},
		{"item passes accounts", Filter{Kinds: []Kind{KindItem}, Accounts: []string{"b"}}, item, true},
		{"item passes accounts with the item filter", Filter{Items: onlyA, Accounts: []string{"b"}}, item, true},
		{"item_mail account listed", Filter{Kinds: []Kind{KindItemMail}, Accounts: []string{"a"}}, itemMail, true},
		{"item_mail account not listed", Filter{Kinds: []Kind{KindItemMail}, Accounts: []string{"b"}}, itemMail, false},
		{"item_mail account not listed with the item filter", Filter{Items: onlyA, Accounts: []string{"b"}}, itemMail, false},
		{"item_mail in folder", Filter{Kinds: []Kind{KindItemMail}, In: "inbox"}, itemMail, true},
		{"item_mail in other folder", Filter{Kinds: []Kind{KindItemMail}, In: "Archive"}, itemMail, false},
		{"item_mail in label", Filter{Kinds: []Kind{KindItemMail}, In: "Receipts"}, gmailItemMail, true},
		{"item_mail inbox alias", Filter{Items: onlyA, In: "inbox"}, gmailItemMail, true},
		{"item_mail sent alias not inbox", Filter{Items: onlyA, In: "sent"}, gmailItemMail, false},
		{"item_mail missing label", Filter{Kinds: []Kind{KindItemMail}, In: "Travel"}, gmailItemMail, false},
		{"item_mail without message", Filter{Kinds: []Kind{KindItemMail}, In: "Receipts"}, Event{Kind: KindItemMail, Folder: "INBOX", ItemID: "it_a"}, false},
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
