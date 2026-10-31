package live

// The rules the write tests follow to decide when a send has arrived, when
// cleanup may stop, what a move must leave behind and where an organize
// call left a message. They need no daemon, so this file has no build tag
// and its tests run with the unit suite.

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
)

const (
	// firstPoll is the pause before the first repeat of a search.
	firstPoll = time.Second
	// maxPoll caps the pause between searches while waiting. Gmail's search
	// can trail a change by minutes, so a wait may run long, and the cap
	// keeps it to a few searches a minute.
	maxPoll = 15 * time.Second
	// deliveryTimeout bounds the wait for the delivered copy of a message
	// sent to self: the inbox copy, or on Google the All Mail message. The
	// test finds it by searching, and Gmail's search, served from its
	// search index, trailed real changes by 2 to 4 minutes in live runs.
	deliveryTimeout = 6 * time.Minute
	// searchTimeout bounds the wait for a message to show in a search of a
	// folder after an organize call. The test searches only when it does
	// not know the message's ref, as after a COPY or MOVE without COPYUID.
	// Where it knows the ref, it reads it with Get, which needs no index.
	searchTimeout = 6 * time.Minute
	// settleTimeout is how long cleanup keeps looking for the delivered copy
	// of a send the test never saw arrive, after the last send. It is longer
	// than deliveryTimeout, so a send the test gave up on still gets time.
	settleTimeout = deliveryTimeout + 2*time.Minute
	// cleanupTimeout bounds each cleanup. It outlasts settleTimeout, so a
	// cleanup that waited for a late send still has time to expunge.
	cleanupTimeout = settleTimeout + 5*time.Minute
	// cleanupReserve is the time no wait may use before go test's -timeout
	// deadline. A test the deadline kills runs no cleanup and leaves its
	// mail behind, so the waits stop early enough for the cleanups of the
	// test under way to finish. The organize test runs two, its sweep and
	// its folder cleanup, one after the other, each bounded by
	// cleanupTimeout.
	cleanupReserve = 2*cleanupTimeout + 2*time.Minute
)

// capWait shortens a wait of want so it ends cleanupReserve before the
// test deadline, when there is one. ok is false when the reserve leaves no
// time at all, and the test must stop without starting anything new.
func capWait(want time.Duration, deadline time.Time, hasDeadline bool, now time.Time) (time.Duration, bool) {
	if !hasDeadline {
		return want, true
	}
	left := deadline.Sub(now) - cleanupReserve
	if left <= 0 {
		return 0, false
	}
	return min(want, left), true
}

// backoff returns the pause before repeat n of a search, from 0: firstPoll,
// doubling up to maxPoll.
func backoff(n int) time.Duration {
	d := firstPoll
	for range n {
		d *= 2
		if d >= maxPoll {
			return maxPoll
		}
	}
	return d
}

// deliveredCopy says m is the delivered copy of a message sent to self.
// Elsewhere that is the received copy, in an inbox-role folder. Gmail files
// a message an account sends to its own address once, in Sent, and adds no
// inbox copy, so on Google it is the one All Mail message, which is the
// Sent copy too. A draft that shares the Message-ID, as one being sent
// does, is not it.
func deliveredCopy(m *chainmailv1.Message, google bool, roles map[string]string) bool {
	if slices.Contains(m.GetFlags(), `\Draft`) {
		return false
	}
	if google {
		return roles[m.GetFolder()] == "all"
	}
	return roles[m.GetFolder()] == "inbox"
}

// deliveryScope is the in: scope where the delivered copy of a message
// sent to self is found: the inbox, or on Google anywhere. Gmail keeps a
// self-send as its one Sent message, and a message just sent can take some
// seconds to show in a search of All Mail while in:anywhere already finds
// it, so deliveredCopy takes the one message wherever it is.
func deliveryScope(google bool) string {
	if google {
		return "anywhere"
	}
	return "inbox"
}

// trimID drops the angle brackets around a Message-ID, if any.
func trimID(id string) string { return strings.TrimSuffix(strings.TrimPrefix(id, "<"), ">") }

// deliveries tracks a test's sends against the inbox copies seen of them,
// by Message-ID, so cleanup knows whether a send may still arrive.
type deliveries struct {
	sends int
	// seen holds the Message-IDs, without angle brackets, of the inbox
	// copies seen. Each send has its own Message-ID.
	seen map[string]bool
	// last is when the last send started.
	last time.Time
}

// sending is called before each send attempt.
func (d *deliveries) sending(now time.Time) {
	d.sends++
	d.last = now
}

// delivered records that the inbox copy of the message id was seen.
func (d *deliveries) delivered(id string) {
	if d.seen == nil {
		d.seen = map[string]bool{}
	}
	d.seen[trimID(id)] = true
}

// outstanding is how many sends have no inbox copy seen yet.
func (d *deliveries) outstanding() int { return max(d.sends-len(d.seen), 0) }

// settle is what cleanup does next about the sends it has not seen arrive.
type settle int

const (
	// settleDone: every send was seen in the inbox.
	settleDone settle = iota
	// settleWait: some were not, and one may still arrive.
	settleWait
	// settleUnresolved: some were not, and the wait for them is over.
	settleUnresolved
)

// next says whether cleanup goes on waiting at now, with patience after the
// last send.
func (d *deliveries) next(now time.Time, patience time.Duration) settle {
	switch {
	case d.outstanding() == 0:
		return settleDone
	case now.Sub(d.last) >= patience:
		return settleUnresolved
	}
	return settleWait
}

// originalMustBeGone says whether a move whose outcome gave newRef must
// leave its original ref unreadable. On Google a move edits labels, so the
// ref stays. Elsewhere a new ref comes from MOVE or a confirmed COPYUID,
// and either removes the original. Without one, spec 7.1 lets a move done
// by COPY keep the original, so it may still read.
func originalMustBeGone(google bool, newRef string) bool {
	return !google && newRef != ""
}

// gone says a ref no longer names a message, which cleanup takes as done:
// the test or an earlier sweep moved or removed it.
func gone(e *chainmailv1.ItemError) bool {
	return e.GetReason() == api.ReasonNotFound || e.GetReason() == api.ReasonStaleRef
}

// tracked holds the refs a write test learned for the messages it made, in
// the order it learned them and without repeats. Cleanup removes them by
// ref, because a search for the token can miss them while Gmail's search
// index trails the change.
type tracked struct{ refs []string }

// track records refs. An empty ref, from a call that gave none, is skipped.
func (k *tracked) track(refs ...string) {
	for _, r := range refs {
		if r != "" && !slices.Contains(k.refs, r) {
			k.refs = append(k.refs, r)
		}
	}
}

// inRole says m is in the folder with role, such as "trash". A folder with
// no role is in none.
func inRole(m *chainmailv1.Message, roles map[string]string, role string) bool {
	return role != "" && roles[m.GetFolder()] == role
}

// filedIn says m is filed in folder. On Google a folder is a label and the
// message stays in All Mail, so it has the label. Elsewhere it is in the
// folder.
func filedIn(m *chainmailv1.Message, google bool, folder string) bool {
	if google {
		return slices.Contains(m.GetLabels(), folder)
	}
	return m.GetFolder() == folder
}

// archived says m was archived from folder from. On Google archiving takes
// the \Inbox label away and the message stays in All Mail. Elsewhere it has
// left from for another folder. The account's archive_folder setting can
// name any folder, one with another role such as Sent included, and the
// API does not say which, so only the inbox and Trash are ruled out: no
// archive lands there.
func archived(m *chainmailv1.Message, google bool, roles map[string]string, from string) bool {
	if google {
		return inRole(m, roles, "all") && !slices.Contains(m.GetLabels(), `\Inbox`)
	}
	return m.GetFolder() != from && !inRole(m, roles, "inbox") && !inRole(m, roles, "trash")
}

// isDraft says m is a draft: it has the \Draft flag, or on Google the
// \Draft label, or it is in the folder with the drafts role.
func isDraft(m *chainmailv1.Message, roles map[string]string) bool {
	return slices.Contains(m.GetFlags(), `\Draft`) || slices.Contains(m.GetLabels(), `\Draft`) || inRole(m, roles, "drafts")
}

// nameTaken says a folder called name, or name under a personal namespace
// prefix such as "INBOX.", is among the account's folders, without regard
// to case. The organize test checks its folder name is free with it before
// it creates the folder, whose name the server may prefix.
func nameTaken(roles map[string]string, name string) bool {
	for f := range roles {
		if strings.HasSuffix(strings.ToLower(f), strings.ToLower(name)) {
			return true
		}
	}
	return false
}

// listed says a folder called name is among the account's folders, without
// regard to case, as Gmail matches labels. On a server that tells case
// apart this is stricter than it needs to be, which is the safe side.
func listed(roles map[string]string, name string) bool {
	for f := range roles {
		if strings.EqualFold(f, name) {
			return true
		}
	}
	return false
}

func TestBackoff(t *testing.T) {
	// Waits for Gmail's search index run to minutes, so the pause grows to
	// 15 seconds, not to hammer the server with searches.
	for n, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, maxPoll} {
		if got := backoff(n); got != want {
			t.Errorf("backoff(%d) = %v, want %v", n, got, want)
		}
	}
	if got := backoff(200); got != maxPoll {
		t.Errorf("backoff(200) = %v, want %v", got, maxPoll)
	}
}

func TestDeliveredCopy(t *testing.T) {
	roles := map[string]string{
		"INBOX": "inbox", "Sent": "sent", "Drafts": "drafts", "Archive": "archive", "chainmail-test-x": "",
		"[Gmail]/All Mail": "all", "[Gmail]/Trash": "trash",
	}
	msg := func(folder string, flags, labels []string) *chainmailv1.Message {
		return &chainmailv1.Message{Folder: folder, Flags: flags, Labels: labels}
	}
	for _, tc := range []struct {
		name   string
		m      *chainmailv1.Message
		google bool
		want   bool
	}{
		// Gmail files a message an account sends to its own address once,
		// in Sent, and adds no inbox copy: live, none of three Google
		// accounts gave one \Inbox within 3 minutes. So the one All Mail
		// message, though it is only the Sent copy, is the delivered copy.
		{"google sent copy only", msg("[Gmail]/All Mail", []string{`\Seen`}, []string{`\Sent`}), true, true},
		{"google no labels", msg("[Gmail]/All Mail", nil, nil), true, true},
		{"google received", msg("[Gmail]/All Mail", nil, []string{`\Inbox`, `\Sent`}), true, true},
		{"google draft", msg("[Gmail]/All Mail", []string{`\Draft`}, []string{`\Draft`}), true, false},
		{"google draft in inbox", msg("[Gmail]/All Mail", []string{`\Draft`}, []string{`\Inbox`}), true, false},
		{"google in Trash", msg("[Gmail]/Trash", []string{`\Seen`}, nil), true, false},
		{"standard inbox", msg("INBOX", []string{`\Seen`}, nil), false, true},
		{"standard sent", msg("Sent", []string{`\Seen`}, nil), false, false},
		{"standard drafts", msg("Drafts", nil, nil), false, false},
		{"standard archive", msg("Archive", nil, nil), false, false},
		{"standard other folder", msg("chainmail-test-x", nil, nil), false, false},
		{"standard draft in inbox", msg("INBOX", []string{`\Draft`}, nil), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deliveredCopy(tc.m, tc.google, roles); got != tc.want {
				t.Errorf("deliveredCopy = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeliveryScope(t *testing.T) {
	// Google: anywhere, since a self-send never reaches the inbox there and
	// a just-sent message can take a while to show in All Mail's search.
	// Elsewhere: the inbox.
	if got := deliveryScope(true); got != "anywhere" {
		t.Errorf("deliveryScope(google) = %q, want anywhere", got)
	}
	if got := deliveryScope(false); got != "inbox" {
		t.Errorf("deliveryScope(standard) = %q, want inbox", got)
	}
}

func TestNameTaken(t *testing.T) {
	roles := map[string]string{"INBOX": "inbox", "INBOX.Chainmail-Test-abc": "", "Chainmail-Test-def": ""}
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"chainmail-test-def", true},
		// A server with a personal namespace prefix such as "INBOX." keeps
		// the folder under it, so a name taken there is taken.
		{"chainmail-test-abc", true},
		{"chainmail-test-xyz", false},
	} {
		if got := nameTaken(roles, tc.name); got != tc.want {
			t.Errorf("nameTaken(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDeliveries(t *testing.T) {
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	const patience = time.Minute

	var d deliveries
	if got := d.next(start, patience); got != settleDone {
		t.Errorf("no sends: next = %v, want settleDone", got)
	}
	d.sending(start)
	d.sending(start.Add(10 * time.Second))
	d.delivered("<a@example.com>")
	d.delivered("a@example.com") // the same message seen again
	if got := d.outstanding(); got != 1 {
		t.Fatalf("two sends, one seen twice: outstanding = %d, want 1", got)
	}
	if got := d.next(start.Add(30*time.Second), patience); got != settleWait {
		t.Errorf("one outstanding, before the deadline: next = %v, want settleWait", got)
	}
	if got := d.next(start.Add(70*time.Second), patience); got != settleUnresolved {
		t.Errorf("one outstanding, past the deadline: next = %v, want settleUnresolved", got)
	}
	d.delivered("<b@example.com>")
	if got := d.next(start.Add(70*time.Second), patience); got != settleDone {
		t.Errorf("all seen: next = %v, want settleDone", got)
	}
}

func TestOriginalMustBeGone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		google bool
		newRef string
		want   bool
	}{
		{"google label edit", true, "", false},
		{"google with new ref", true, "work:abc", false},
		{"standard, new ref from MOVE or COPYUID", false, "work:abc", true},
		// Spec 7.1: without COPYUID a move done by COPY keeps the original,
		// and a MOVE without COPYUID removes it, so either is right.
		{"standard, no new ref", false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := originalMustBeGone(tc.google, tc.newRef); got != tc.want {
				t.Errorf("originalMustBeGone = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListed(t *testing.T) {
	roles := map[string]string{"INBOX": "inbox", "Chainmail-Test-abc": ""}
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"Chainmail-Test-abc", true},
		// Gmail matches labels without regard to case, so a name that
		// differs only in case is taken.
		{"chainmail-test-abc", true},
		{"chainmail-test-xyz", false},
	} {
		if got := listed(roles, tc.name); got != tc.want {
			t.Errorf("listed(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    *chainmailv1.ItemError
		want bool
	}{
		// The test or an earlier sweep moved or removed the message, so
		// there is nothing left for cleanup to do with the ref.
		{"not found", &chainmailv1.ItemError{Reason: api.ReasonNotFound}, true},
		{"stale ref", &chainmailv1.ItemError{Reason: api.ReasonStaleRef}, true},
		// The message may still be there.
		{"server unavailable", &chainmailv1.ItemError{Reason: api.ReasonServerUnavailable}, false},
		{"timeout", &chainmailv1.ItemError{Reason: api.ReasonTimeout}, false},
		{"no error", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gone(tc.e); got != tc.want {
				t.Errorf("gone = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTracked(t *testing.T) {
	var k tracked
	k.track("work:a")
	k.track("", "work:b") // a call that gave no ref
	k.track("work:a")     // the same message learned again
	k.track("work:c")
	if want := []string{"work:a", "work:b", "work:c"}; !slices.Equal(k.refs, want) {
		t.Errorf("tracked refs = %q, want %q", k.refs, want)
	}
}

func TestInRole(t *testing.T) {
	roles := map[string]string{"INBOX": "inbox", "Trash": "trash", "[Gmail]/Trash": "trash", "chainmail-test-x": ""}
	for _, tc := range []struct {
		folder, role string
		want         bool
	}{
		{"Trash", "trash", true},
		{"[Gmail]/Trash", "trash", true},
		{"INBOX", "trash", false},
		{"Unlisted", "trash", false},
		// A folder with no role is not in the role "".
		{"chainmail-test-x", "", false},
	} {
		if got := inRole(&chainmailv1.Message{Folder: tc.folder}, roles, tc.role); got != tc.want {
			t.Errorf("inRole(%q, %q) = %v, want %v", tc.folder, tc.role, got, tc.want)
		}
	}
}

func TestFiledIn(t *testing.T) {
	const folder = "INBOX.chainmail-test-x"
	for _, tc := range []struct {
		name   string
		m      *chainmailv1.Message
		google bool
		want   bool
	}{
		{"standard, in the folder", &chainmailv1.Message{Folder: folder}, false, true},
		{"standard, elsewhere", &chainmailv1.Message{Folder: "INBOX", Labels: []string{folder}}, false, false},
		// On Google a folder is a label and the message stays in All Mail.
		{"google, labelled", &chainmailv1.Message{Folder: "[Gmail]/All Mail", Labels: []string{`\Sent`, folder}}, true, true},
		{"google, not labelled", &chainmailv1.Message{Folder: "[Gmail]/All Mail", Labels: []string{`\Sent`}}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := filedIn(tc.m, tc.google, folder); got != tc.want {
				t.Errorf("filedIn = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestArchived(t *testing.T) {
	const from = "INBOX.chainmail-test-x"
	roles := map[string]string{
		"INBOX": "inbox", "INBOX.Archive": "archive", "INBOX.Trash": "trash", "INBOX.Saved": "", "INBOX.Sent": "sent", from: "",
		"[Gmail]/All Mail": "all", "[Gmail]/Trash": "trash",
	}
	for _, tc := range []struct {
		name   string
		m      *chainmailv1.Message
		google bool
		want   bool
	}{
		{"standard, in Archive", &chainmailv1.Message{Folder: "INBOX.Archive"}, false, true},
		// The account's archive_folder setting can name a folder without
		// the archive role.
		{"standard, in another plain folder", &chainmailv1.Message{Folder: "INBOX.Saved"}, false, true},
		// It can also name a folder that has another role, such as Sent.
		{"standard, in a folder with another role", &chainmailv1.Message{Folder: "INBOX.Sent"}, false, true},
		{"standard, still where it was", &chainmailv1.Message{Folder: from}, false, false},
		{"standard, in the inbox", &chainmailv1.Message{Folder: "INBOX"}, false, false},
		{"standard, in Trash", &chainmailv1.Message{Folder: "INBOX.Trash"}, false, false},
		// Archiving on Google takes the \Inbox label away, and the message
		// stays in All Mail.
		{"google, out of the inbox", &chainmailv1.Message{Folder: "[Gmail]/All Mail", Labels: []string{`\Sent`, from}}, true, true},
		{"google, still in the inbox", &chainmailv1.Message{Folder: "[Gmail]/All Mail", Labels: []string{`\Inbox`}}, true, false},
		{"google, in Trash", &chainmailv1.Message{Folder: "[Gmail]/Trash"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := archived(tc.m, tc.google, roles, from); got != tc.want {
				t.Errorf("archived = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsDraft(t *testing.T) {
	roles := map[string]string{"INBOX": "inbox", "Drafts": "drafts", "Sent": "sent", "[Gmail]/All Mail": "all"}
	for _, tc := range []struct {
		name string
		m    *chainmailv1.Message
		want bool
	}{
		{"standard, in Drafts", &chainmailv1.Message{Folder: "Drafts"}, true},
		{"standard, flagged elsewhere", &chainmailv1.Message{Folder: "INBOX", Flags: []string{`\Draft`}}, true},
		{"standard, the sent copy", &chainmailv1.Message{Folder: "Sent", Flags: []string{`\Seen`}}, false},
		{"google, a draft", &chainmailv1.Message{Folder: "[Gmail]/All Mail", Flags: []string{`\Draft`}, Labels: []string{`\Draft`}}, true},
		{"google, labelled only", &chainmailv1.Message{Folder: "[Gmail]/All Mail", Labels: []string{`\Draft`}}, true},
		// The message a sent draft became shares its Message-ID but is no
		// draft.
		{"google, the sent message", &chainmailv1.Message{Folder: "[Gmail]/All Mail", Flags: []string{`\Seen`}, Labels: []string{`\Sent`}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDraft(tc.m, roles); got != tc.want {
				t.Errorf("isDraft = %v, want %v", got, tc.want)
			}
		})
	}
}

// String names a settle value in test failures.
func (s settle) String() string {
	switch s {
	case settleDone:
		return "settleDone"
	case settleWait:
		return "settleWait"
	case settleUnresolved:
		return "settleUnresolved"
	}
	return fmt.Sprintf("settle(%d)", int(s))
}

func TestCapWait(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		want        time.Duration
		deadline    time.Time
		hasDeadline bool
		wantWait    time.Duration
		wantOK      bool
	}{
		{"no deadline", 6 * time.Minute, time.Time{}, false, 6 * time.Minute, true},
		{"plenty of time", 6 * time.Minute, now.Add(cleanupReserve + time.Hour), true, 6 * time.Minute, true},
		{"cut short by the reserve", 6 * time.Minute, now.Add(cleanupReserve + 2*time.Minute), true, 2 * time.Minute, true},
		{"only the reserve left", 6 * time.Minute, now.Add(cleanupReserve), true, 0, false},
		{"past the deadline", 6 * time.Minute, now.Add(-time.Minute), true, 0, false},
		{"a zero wait with time left", 0, now.Add(cleanupReserve + time.Minute), true, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := capWait(tc.want, tc.deadline, tc.hasDeadline, now)
			if got != tc.wantWait || ok != tc.wantOK {
				t.Errorf("capWait = %v, %v, want %v, %v", got, ok, tc.wantWait, tc.wantOK)
			}
		})
	}
}
