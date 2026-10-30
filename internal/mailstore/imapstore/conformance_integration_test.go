//go:build integration

package imapstore

import (
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/storetest"
)

// TestConformanceDovecot runs the shared mailstore conformance suite
// against imapstore on Dovecot, each case in a mailbox of its own.
func TestConformanceDovecot(t *testing.T) {
	storetest.Run(t, storetest.Harness{New: func(t *testing.T) (mailstore.Store, storetest.Seeder) {
		user := doveUser(t)
		return doveStore(t, user), func(t *testing.T, msgs ...storetest.Message) {
			t.Helper()
			for _, m := range msgs {
				flags := make([]imap.Flag, len(m.Flags))
				for i, f := range m.Flags {
					flags[i] = imap.Flag(f)
				}
				if _, err := dove().Append(user, m.Folder, string(m.Raw()), m.Date, flags...); err != nil {
					t.Fatalf("APPEND %s to %s: %v", m.Subject, m.Folder, err)
				}
			}
		}
	}})
}
