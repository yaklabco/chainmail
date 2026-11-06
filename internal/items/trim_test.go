package items_test

import (
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/items"
)

func TestTrim(t *testing.T) {
	t.Parallel()
	r := newLinkRig(t)
	r.link(t, r.task(t), "work", "t1")
	acct := items.MailAccount{Name: "work", Address: "me@example.net"}
	old := r.clock.Now()
	r.arrive(t, acct, newMail(mailSpec{account: "work", folder: "INBOX", uid: 1, thread: "t1", msgID: "old@example.com"}))
	r.create(t, items.NewItem{Type: items.TypeTask, Title: "keyed", Area: items.AreaWork, IdempotencyKey: "old-key"})
	r.clock.Advance(8 * 24 * time.Hour)
	recent := r.clock.Now()
	r.arrive(t, acct, newMail(mailSpec{account: "work", folder: "INBOX", uid: 2, thread: "t1", msgID: "new@example.com"}))
	r.create(t, items.NewItem{Type: items.TypeTask, Title: "keyed again", Area: items.AreaWork, IdempotencyKey: "new-key"})
	steps := []struct {
		name               string
		seenBefore         time.Time
		deleted            int64
		seenLeft, keysLeft int
	}{
		// A seen row at the cutoff stays: only older rows go. The request row
		// is 8 days old, past the 7 days kept.
		{"the cutoff is the old seen row's time", old, 1, 2, 1},
		{"the cutoff is now", recent, 1, 1, 1},
		{"nothing left to trim", recent, 0, 1, 1},
	}
	for _, s := range steps {
		n, err := r.store.Trim(t.Context(), s.seenBefore)
		if err != nil || n != s.deleted {
			t.Errorf("%s: Trim = %d, %v; want %d", s.name, n, err, s.deleted)
		}
		if c := r.count(t, "SELECT count(*) FROM item_mail_seen"); c != s.seenLeft {
			t.Errorf("%s: %d seen rows left, want %d", s.name, c, s.seenLeft)
		}
		if c := r.count(t, "SELECT count(*) FROM item_requests"); c != s.keysLeft {
			t.Errorf("%s: %d request rows left, want %d", s.name, c, s.keysLeft)
		}
	}
	if c := r.count(t, "SELECT count(*) FROM item_requests WHERE key = ?", "new-key"); c != 1 {
		t.Error("the recent request row is gone")
	}
}
