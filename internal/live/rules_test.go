package live

// The rules the write tests follow to decide when a send has arrived, when
// cleanup may stop and what a move must leave behind. They need no daemon,
// so this file has no build tag and its tests run with the unit suite.

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
)

const (
	// firstPoll is the pause before the first repeat of a search.
	firstPoll = time.Second
	// maxPoll caps the pause between searches while waiting.
	maxPoll = 5 * time.Second
)

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
	for n, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, maxPoll, maxPoll} {
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
