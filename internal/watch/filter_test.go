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

// TestFilterMatchOptInKinds covers the rule and due kinds beside item and
// item_mail. All six are opt-in: a filter with no kinds and no item filter,
// the cache's reader included, sees none of them. A named kind passes. An
// item filter passes the due kinds of its items and drops rule events. None
// of the four has a folder or an account, so In and Accounts never drop
// them.
func TestFilterMatchOptInKinds(t *testing.T) {
	t.Parallel()
	inbox := Event{Kind: KindNew, Account: "a", Folder: "INBOX", Message: &Message{}}
	status := Event{Kind: KindStatus, Account: "a", Folder: "INBOX", State: StateWatching}
	item := Event{Kind: KindItem, Item: &ItemChange{ItemID: "it_a", Type: "task", Revision: 4, Op: "updated"}}
	itemMail := Event{Kind: KindItemMail, Account: "a", Folder: "INBOX", Ref: "cm:a:INBOX:1:5", ItemID: "it_a", Message: &Message{}}
	rule := Event{Kind: KindRule, Rule: &RuleChange{RuleID: "ru_a", Name: "receipts", Change: "created", Revision: 1, Actor: "1000/cli"}}
	soonA := Event{Kind: KindItemDueSoon, Due: &ItemDue{ItemID: "it_a", Type: "task", DueDate: "2026-11-09", State: "backlog"}}
	dueB := Event{Kind: KindItemDue, Due: &ItemDue{ItemID: "it_b", Type: "epic", DueDate: "2026-11-07", State: "in-progress"}}
	overdueA := Event{Kind: KindItemOverdue, Due: &ItemDue{ItemID: "it_a", Type: "task", DueDate: "2026-11-01", State: "in-review"}}
	dueWithout := Event{Kind: KindItemDue}
	onlyA := map[string]bool{"it_a": true}
	onlyB := map[string]bool{"it_b": true}
	dueKinds := []Kind{KindItemDueSoon, KindItemDue, KindItemOverdue}
	cache := Filter{Accounts: []string{"a"}, Internal: true}

	tests := []struct {
		name string
		f    Filter
		e    Event
		want bool
	}{
		// Empty kinds hide all six opt-in kinds.
		{"item hidden by empty kinds", Filter{}, item, false},
		{"item_mail hidden by empty kinds", Filter{}, itemMail, false},
		{"rule hidden by empty kinds", Filter{}, rule, false},
		{"item_due_soon hidden by empty kinds", Filter{}, soonA, false},
		{"item_due hidden by empty kinds", Filter{}, dueB, false},
		{"item_overdue hidden by empty kinds", Filter{}, overdueA, false},
		{"rule hidden with only accounts", Filter{Accounts: []string{"a"}}, rule, false},
		{"item_due hidden with only in", Filter{In: "INBOX"}, dueB, false},
		{"rule hidden from the cache reader", cache, rule, false},
		{"item_due_soon hidden from the cache reader", cache, soonA, false},
		{"item_due hidden from the cache reader", cache, dueB, false},
		{"item_overdue hidden from the cache reader", cache, overdueA, false},
		{"rule hidden from an internal reader", Filter{Internal: true}, rule, false},
		{"new still passes empty kinds", Filter{}, inbox, true},

		// Each new kind named.
		{"rule named", Filter{Kinds: []Kind{KindRule}}, rule, true},
		{"item_due_soon named", Filter{Kinds: []Kind{KindItemDueSoon}}, soonA, true},
		{"item_due named", Filter{Kinds: []Kind{KindItemDue}}, dueB, true},
		{"item_overdue named", Filter{Kinds: []Kind{KindItemOverdue}}, overdueA, true},
		{"every due kind named", Filter{Kinds: dueKinds}, overdueA, true},
		{"item_due not named beside item_due_soon", Filter{Kinds: []Kind{KindItemDueSoon}}, dueB, false},
		{"rule not named beside item", Filter{Kinds: []Kind{KindItem}}, rule, false},
		{"due not named beside rule", Filter{Kinds: []Kind{KindRule}}, soonA, false},
		{"new not named beside rule", Filter{Kinds: []Kind{KindRule}}, inbox, false},
		{"item not named beside the due kinds", Filter{Kinds: dueKinds}, item, false},
		{"status passes the rule kind", Filter{Kinds: []Kind{KindRule}}, status, true},
		{"status passes the due kinds", Filter{Kinds: dueKinds}, status, true},
		{"rule named for an internal reader", Filter{Internal: true, Kinds: []Kind{KindRule}}, rule, true},

		// The item filter.
		{"item filter passes its item_due_soon", Filter{Items: onlyA}, soonA, true},
		{"item filter passes its item_overdue", Filter{Items: onlyA}, overdueA, true},
		{"item filter drops another item's item_due", Filter{Items: onlyA}, dueB, false},
		{"item filter passes the other item's item_due", Filter{Items: onlyB}, dueB, true},
		{"item filter needs a due payload", Filter{Items: onlyA}, dueWithout, false},
		{"item filter drops rule", Filter{Items: onlyA}, rule, false},
		{"item filter drops rule even when named", Filter{Kinds: []Kind{KindRule}, Items: onlyA}, rule, false},
		{"item filter and kind item_due", Filter{Kinds: []Kind{KindItemDue}, Items: onlyB}, dueB, true},
		{"item filter and kind item_due drops item_due_soon", Filter{Kinds: []Kind{KindItemDue}, Items: onlyA}, soonA, false},
		{"empty item filter drops every due event", Filter{Items: map[string]bool{}}, soonA, false},

		// In and Accounts.
		{"rule passes in", Filter{Kinds: []Kind{KindRule}, In: "INBOX"}, rule, true},
		{"item_due_soon passes in", Filter{Kinds: []Kind{KindItemDueSoon}, In: "Archive"}, soonA, true},
		{"item_due passes in", Filter{Kinds: []Kind{KindItemDue}, In: "inbox"}, dueB, true},
		{"item_overdue passes in with the item filter", Filter{Items: onlyA, In: "Receipts"}, overdueA, true},
		{"rule passes accounts", Filter{Kinds: []Kind{KindRule}, Accounts: []string{"b"}}, rule, true},
		{"item_overdue passes accounts", Filter{Kinds: []Kind{KindItemOverdue}, Accounts: []string{"b"}}, overdueA, true},
		{"item_due_soon passes accounts with the item filter", Filter{Items: onlyA, Accounts: []string{"b"}}, soonA, true},
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

// The four calendar kinds are opt-in and folderless. They carry an
// account, so a filter that names no kinds, the cache builder's reader
// included, must drop them or every mail reader would get them. Named, they
// pass In always and Accounts only for their own account. An item filter
// drops them, as it drops rule events.
func TestFilterMatchCalendarKinds(t *testing.T) {
	t.Parallel()
	ref := "ce_0123456789abcdefghjkmnpqrs"
	invite := Event{Kind: KindInvite, Account: "a", Ref: ref, Calendar: &CalendarChange{Ref: ref, CalendarID: "primary", Organizer: "ann@example.com"}}
	changed := Event{Kind: KindEventChanged, Account: "a", Ref: ref, Calendar: &CalendarChange{Ref: ref, Fields: []string{"start"}}}
	cancelled := Event{Kind: KindEventCancelled, Account: "a", Ref: ref, Calendar: &CalendarChange{Ref: ref}}
	reply := Event{Kind: KindInviteReply, Account: "a", Ref: ref, Calendar: &CalendarChange{Ref: ref, Attendee: "bob@example.com", PartStat: "ACCEPTED"}}
	status := Event{Kind: KindStatus, Account: "a", Folder: "INBOX", State: StateWatching}
	inbox := Event{Kind: KindNew, Account: "a", Folder: "INBOX", Message: &Message{}}
	calKinds := []Kind{KindInvite, KindEventChanged, KindEventCancelled, KindInviteReply}
	cache := Filter{Accounts: []string{"a"}, Internal: true}
	onlyA := map[string]bool{"it_a": true}

	tests := []struct {
		name string
		f    Filter
		e    Event
		want bool
	}{
		// A default filter and the cache builder's filter drop all four.
		{"invite hidden by empty kinds", Filter{}, invite, false},
		{"event_changed hidden by empty kinds", Filter{}, changed, false},
		{"event_cancelled hidden by empty kinds", Filter{}, cancelled, false},
		{"invite_reply hidden by empty kinds", Filter{}, reply, false},
		{"invite hidden with only its account", Filter{Accounts: []string{"a"}}, invite, false},
		{"event_changed hidden with only in", Filter{In: "INBOX"}, changed, false},
		{"invite hidden from the cache reader", cache, invite, false},
		{"event_changed hidden from the cache reader", cache, changed, false},
		{"event_cancelled hidden from the cache reader", cache, cancelled, false},
		{"invite_reply hidden from the cache reader", cache, reply, false},
		{"invite hidden from an internal reader", Filter{Internal: true}, invite, false},
		{"new still passes the cache reader", cache, inbox, true},

		// Delivered when listed.
		{"invite named", Filter{Kinds: []Kind{KindInvite}}, invite, true},
		{"event_changed named", Filter{Kinds: []Kind{KindEventChanged}}, changed, true},
		{"event_cancelled named", Filter{Kinds: []Kind{KindEventCancelled}}, cancelled, true},
		{"invite_reply named", Filter{Kinds: []Kind{KindInviteReply}}, reply, true},
		{"every calendar kind named", Filter{Kinds: calKinds}, reply, true},
		{"event_changed not named beside invite", Filter{Kinds: []Kind{KindInvite}}, changed, false},
		{"invite not named beside the due kinds", Filter{Kinds: []Kind{KindItemDueSoon, KindItemDue, KindItemOverdue}}, invite, false},
		{"new not named beside the calendar kinds", Filter{Kinds: calKinds}, inbox, false},
		{"status passes the calendar kinds", Filter{Kinds: calKinds}, status, true},
		{"invite named for an internal reader", Filter{Internal: true, Kinds: []Kind{KindInvite}}, invite, true},

		// In never drops them; Accounts drops another account's.
		{"invite passes in", Filter{Kinds: []Kind{KindInvite}, In: "INBOX"}, invite, true},
		{"event_changed passes in", Filter{Kinds: []Kind{KindEventChanged}, In: "Archive"}, changed, true},
		{"event_cancelled passes in", Filter{Kinds: []Kind{KindEventCancelled}, In: "sent"}, cancelled, true},
		{"invite_reply passes in", Filter{Kinds: []Kind{KindInviteReply}, In: "inbox"}, reply, true},
		{"invite passes its account", Filter{Kinds: []Kind{KindInvite}, Accounts: []string{"a"}}, invite, true},
		{"invite dropped for another account", Filter{Kinds: []Kind{KindInvite}, Accounts: []string{"b"}}, invite, false},
		{"invite_reply dropped for another account", Filter{Kinds: calKinds, Accounts: []string{"b"}}, reply, false},

		// An item filter drops them, named or not.
		{"item filter drops invite", Filter{Items: onlyA}, invite, false},
		{"item filter drops event_changed even when named", Filter{Kinds: []Kind{KindEventChanged}, Items: onlyA}, changed, false},
		{"item filter drops event_cancelled", Filter{Items: onlyA}, cancelled, false},
		{"item filter drops invite_reply even when named", Filter{Kinds: calKinds, Items: onlyA}, reply, false},
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
