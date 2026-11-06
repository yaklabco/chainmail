package fakegmail

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
)

// TestFakeGmailIdlePushesChanges idles on All Mail while another session
// appends and then expunges: the idling client hears EXISTS and EXPUNGE
// without sending a command, as it would from Gmail.
func TestFakeGmailIdlePushesChanges(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := Start(t, ca, "me@gmail.example", "pw", Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}})
	exists := make(chan uint32, 8)
	expunged := make(chan uint32, 8)
	c, err := imapclient.DialTLS(s.Endpoint.Addr(), &imapclient.Options{
		TLSConfig: &tls.Config{RootCAs: ca.Pool, MinVersion: tls.VersionTLS12},
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(d *imapclient.UnilateralDataMailbox) {
				if d.NumMessages != nil {
					exists <- *d.NumMessages
				}
			},
			Expunge: func(seq uint32) { expunged <- seq },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Login("me@gmail.example", "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("[Gmail]/All Mail", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	idle, err := c.Idle()
	if err != nil {
		t.Fatal(err)
	}

	writer := dial(t, s, ca)
	data := appendRaw(t, writer, "[Gmail]/All Mail", "Subject: Pushed\r\n\r\nx\r\n")
	select {
	case n := <-exists:
		if n != 2 {
			t.Errorf("EXISTS during IDLE = %d, want 2", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no EXISTS during IDLE within 2s of another session's APPEND")
	}

	selectRW(t, writer, "[Gmail]/All Mail")
	deleted := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}
	if err := writer.Store(imap.UIDSetNum(data.UID), deleted, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.UIDExpunge(imap.UIDSetNum(data.UID)).Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case seq := <-expunged:
		if seq != 2 {
			t.Errorf("EXPUNGE during IDLE = %d, want 2", seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no EXPUNGE during IDLE within 2s of another session's expunge")
	}

	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := idle.Wait(); err != nil {
		t.Errorf("IDLE ended with %v, want OK", err)
	}
}
