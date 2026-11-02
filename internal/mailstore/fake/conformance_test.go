package fake_test

import (
	"cmp"
	"slices"
	"strings"
	"testing"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/mailstore/storetest"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// TestConformance runs the shared mailstore conformance suite against the
// fake, in both modes, so the fake behaves as imapstore does on Dovecot and
// Gmail.
func TestConformance(t *testing.T) {
	t.Parallel()
	for _, gmail := range []bool{false, true} {
		name := map[bool]string{false: "standard", true: "gmail"}[gmail]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			storetest.Run(t, storetest.Harness{Gmail: gmail, Namespaces: true, New: func(_ *testing.T, cfg storetest.Config) (mailstore.Store, storetest.Seeder) {
				s := fake.New("conformance", gmail)
				s.SetArchiveFolder(cfg.ArchiveFolder)
				s.SetNamespace(cfg.Namespace.Prefix, cfg.Namespace.Delim)
				return s, func(t *testing.T, msgs ...storetest.Message) {
					t.Helper()
					for _, m := range msgs {
						seedFake(t, s, gmail, m)
					}
				}
			}})
		})
	}
}

// seedFake adds m to s directly, creating the label folders it names, and returns its ref.
func seedFake(t *testing.T, s *fake.Store, gmail bool, m storetest.Message) ref.Ref {
	t.Helper()
	folder := m.Folder
	if gmail {
		folder = cmp.Or(folder, storetest.GmailAllMail)
		list, err := s.ListFolders(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range m.Labels {
			if !strings.HasPrefix(l, `\`) && !slices.ContainsFunc(list, func(f mailstore.Folder) bool { return strings.EqualFold(f.Name, l) }) {
				s.AddFolder(l, mailstore.RoleNone)
			}
		}
	}
	fm := fake.Message{
		Folder: folder, Date: m.Date, From: []mailstore.Address{{Email: m.From}}, To: []mailstore.Address{{Email: "me@example.test"}},
		Subject: m.Subject, Body: m.Body, Flags: slices.Clone(m.Flags), MessageID: m.MessageID, References: slices.Clone(m.References),
	}
	if gmail {
		fm.Labels = slices.Clone(m.Labels)
	}
	if m.InReplyTo != "" {
		fm.InReplyTo = []string{m.InReplyTo}
	}
	for i, a := range m.Attachments {
		fm.Attachments = append(fm.Attachments, fake.Attachment{Part: storetest.AttachmentPart(i), Filename: a.Filename, ContentType: a.ContentType, Data: a.Data})
	}
	return s.Add(fm)
}

// TestWatchConformance runs the shared watch suite against the fake, on a
// standard account and on Gmail's All Mail, with and without CONDSTORE and
// IDLE, so its passes and wakes match imapstore's on Dovecot and fakegmail.
func TestWatchConformance(t *testing.T) {
	t.Parallel()
	modes := []struct {
		name                   string
		gmail, condstore, idle bool
	}{
		{"standard condstore", false, true, true},
		{"standard plain", false, false, true},
		{"standard plain without idle", false, false, false},
		{"gmail condstore", true, true, true},
		{"gmail plain", true, false, true},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			folder := "INBOX"
			if mode.gmail {
				folder = storetest.GmailAllMail
			}
			storetest.RunWatch(t, storetest.WatchHarness{
				Folder:    folder,
				CondStore: mode.condstore,
				Idle:      mode.idle,
				Gmail:     mode.gmail,
				// The fake renumbers in place, so the watch connection must
				// keep working across it.
				RenumberDropsSessions: false,
				New: func(_ *testing.T) (mailstore.Watcher, storetest.WatchRemote) {
					s := fake.New("conformance", mode.gmail)
					s.SetCondStore(mode.condstore)
					s.SetIdle(mode.idle)
					return s, fakeRemote(s, mode.gmail)
				},
			})
		})
	}
}

// fakeRemote changes s as another mail client would: directly, never
// through the Watcher methods, with no Ops entries or failure hooks.
func fakeRemote(s *fake.Store, gmail bool) storetest.WatchRemote {
	return storetest.WatchRemote{
		Append: func(t *testing.T, folder string, m storetest.Message) uint32 {
			t.Helper()
			m.Folder = folder
			return seedFake(t, s, gmail, m).UID
		},
		SetFlag: func(_ *testing.T, folder string, uid uint32, flag string, on bool) {
			if on {
				s.ExternalFlags(folder, uid, []string{flag}, nil)
				return
			}
			s.ExternalFlags(folder, uid, nil, []string{flag})
		},
		Expunge: func(_ *testing.T, folder string, uid uint32) {
			s.ExternalExpunge(folder, uid)
		},
		NewUIDValidity: func(_ *testing.T, folder string) {
			s.NewUIDValidity(folder)
		},
		Flags: func(_ *testing.T, folder string, uid uint32) []string {
			// Flags finds the message by folder and UID alone.
			return s.Flags(ref.Ref{Folder: folder, UID: uid})
		},
	}
}
