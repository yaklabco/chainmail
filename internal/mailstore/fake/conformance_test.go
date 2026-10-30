package fake_test

import (
	"cmp"
	"slices"
	"strings"
	"testing"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/mailstore/storetest"
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

// seedFake adds m to s directly, creating the label folders it names.
func seedFake(t *testing.T, s *fake.Store, gmail bool, m storetest.Message) {
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
	s.Add(fm)
}
