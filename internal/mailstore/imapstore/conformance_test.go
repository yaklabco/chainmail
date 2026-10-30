package imapstore

import (
	"cmp"
	"crypto/tls"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/storetest"
	"github.com/jamesainslie/chainmail/internal/testutil/fakegmail"
	"github.com/jamesainslie/chainmail/internal/testutil/tlstest"
)

// TestConformanceFakeGmail runs the shared mailstore conformance suite
// against imapstore on fakegmail. The Dovecot run is in
// conformance_integration_test.go.
func TestConformanceFakeGmail(t *testing.T) {
	t.Parallel()
	storetest.Run(t, storetest.Harness{Gmail: true, New: func(t *testing.T, cfg storetest.Config) (mailstore.Store, storetest.Seeder) {
		ca := tlstest.New(t)
		srv := fakegmail.Start(t, ca, "conformance@example.test", "pw")
		acct := testAccount("gmail", srv.Endpoint, true)
		acct.ArchiveFolder = cfg.ArchiveFolder
		s := New(acct, passwordCreds("conformance@example.test", "pw"), Options{RootCAs: ca.Pool, Backoff: noBackoff})
		t.Cleanup(func() { _ = s.Close() })
		return s, func(t *testing.T, msgs ...storetest.Message) {
			t.Helper()
			seedFakeGmail(t, srv, ca, msgs)
		}
	}})
}

// seedFakeGmail adds msgs to the fake as a mail client would: APPEND to All
// Mail, Trash or Spam, then STORE X-GM-LABELS, which creates any user label
// the account lacks.
func seedFakeGmail(t *testing.T, srv *fakegmail.Server, ca *tlstest.CA, msgs []storetest.Message) {
	t.Helper()
	c, err := imapclient.DialTLS(srv.Endpoint.Addr(), &imapclient.Options{TLSConfig: &tls.Config{RootCAs: ca.Pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Login("conformance@example.test", "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		folder := cmp.Or(m.Folder, storetest.GmailAllMail)
		uid := seedAppend(t, c, folder, m)
		if len(m.Labels) == 0 {
			continue
		}
		if _, err := c.Select(folder, nil).Wait(); err != nil {
			t.Fatal(err)
		}
		store := &imap.StoreGmailLabels{Op: imap.StoreFlagsAdd, Silent: true, Labels: m.Labels}
		if err := c.StoreGmailLabels(imap.UIDSetNum(uid), store, nil).Close(); err != nil {
			t.Fatalf("labelling %s: %v", m.Subject, err)
		}
	}
}

// seedAppend APPENDs m to folder on c and returns its UID there.
func seedAppend(t *testing.T, c *imapclient.Client, folder string, m storetest.Message) imap.UID {
	t.Helper()
	raw := m.Raw()
	flags := make([]imap.Flag, len(m.Flags))
	for i, f := range m.Flags {
		flags[i] = imap.Flag(f)
	}
	cmd := c.Append(folder, int64(len(raw)), &imap.AppendOptions{Flags: flags, Time: m.Date})
	_, werr := cmd.Write(raw)
	cerr := cmd.Close()
	data, err := cmd.Wait()
	if err != nil || werr != nil || cerr != nil {
		t.Fatalf("APPEND %s to %s: %v %v %v", m.Subject, folder, err, werr, cerr)
	}
	return data.UID
}
