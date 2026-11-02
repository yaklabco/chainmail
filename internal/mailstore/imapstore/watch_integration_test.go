//go:build integration

package imapstore

import (
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/storetest"
)

// TestWatchDovecot runs the shared watch suite against imapstore on
// Dovecot. Dovecot advertises IDLE and CONDSTORE, so this is the CONDSTORE
// path: CHANGEDSINCE flag fetches and UID SEARCH ALL sweeps. The fakegmail
// run covers the path without CONDSTORE. Each case has a mailbox of its
// own, changed from Dovecot sessions other than the Watcher's.
func TestWatchDovecot(t *testing.T) {
	storetest.RunWatch(t, storetest.WatchHarness{
		Folder:    "INBOX",
		CondStore: true,
		Idle:      true,
		New: func(t *testing.T) (mailstore.Watcher, storetest.WatchRemote) {
			if t.Name() == "TestWatchDovecot/watch/uidvalidity_change" {
				// A session open across a doveadm UIDVALIDITY change sees the
				// new value without a HIGHESTMODSEQ, so NewUIDValidity kicks
				// the user's sessions, as SetUIDValidity does, the Watcher's
				// included. WatchConn.Sync documents that a
				// dropped connection is ErrUnavailable and the caller dials
				// again, but this storetest case keeps one WatchConn across
				// the change. The pooled run of the case redials and passes.
				t.Skip("storetest defect: the watch path of the uidvalidity case cannot redial after NewUIDValidity drops the WatchConn")
			}
			user := doveUser(t)
			return doveStore(t, user), doveRemote(user)
		},
	})
}

// doveRemote changes user's mailbox as another mail client would, each
// call on a Dovecot session of its own.
func doveRemote(user string) storetest.WatchRemote {
	return storetest.WatchRemote{
		Append: func(t *testing.T, folder string, m storetest.Message) uint32 {
			t.Helper()
			uid, err := dove().Append(user, folder, string(m.Raw()), m.Date, doveFlags(m.Flags)...)
			if err != nil {
				t.Fatalf("APPEND %s to %s: %v", m.Subject, folder, err)
			}
			return uint32(uid)
		},
		SetFlag: func(t *testing.T, folder string, uid uint32, flag string, on bool) {
			t.Helper()
			if err := dove().SetFlag(user, folder, imap.UID(uid), imap.Flag(flag), on); err != nil {
				t.Fatalf("STORE %s (on %v) on %s/%d: %v", flag, on, folder, uid, err)
			}
		},
		Expunge: func(t *testing.T, folder string, uid uint32) {
			t.Helper()
			if err := dove().Expunge(user, folder, imap.UID(uid)); err != nil {
				t.Fatalf("expunging %s/%d: %v", folder, uid, err)
			}
		},
		NewUIDValidity: func(t *testing.T, folder string) {
			t.Helper()
			v, err := dove().UIDValidity(user, folder)
			if err != nil {
				t.Fatal(err)
			}
			if err := dove().SetUIDValidity(user, folder, v+1000); err != nil {
				t.Fatalf("changing the UIDVALIDITY of %s: %v", folder, err)
			}
		},
		Flags: func(t *testing.T, folder string, uid uint32) []string {
			t.Helper()
			flags, err := dove().Flags(user, folder, imap.UID(uid))
			if err != nil {
				t.Fatalf("reading the flags of %s/%d: %v", folder, uid, err)
			}
			return flagStrings(flags)
		},
	}
}

// doveFlags converts flag names to go-imap flags without the validation
// imapFlags (write.go) does: a seeded message's flags are the test's own.
func doveFlags(names []string) []imap.Flag {
	flags := make([]imap.Flag, len(names))
	for i, f := range names {
		flags[i] = imap.Flag(f)
	}
	return flags
}
