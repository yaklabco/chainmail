package fakegmail

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
)

// TestFakeGmailIdlePollsOnInjectedTicks drives an idling session's polling
// through Options.IdleTicks: a change another session makes is announced at
// the next tick, and not before.
func TestFakeGmailIdlePollsOnInjectedTicks(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	ticks := make(chan time.Time)
	started := make(chan struct{}, 1)
	opts := Options{IdleTicks: func() (<-chan time.Time, func()) {
		started <- struct{}{}
		return ticks, func() {}
	}}
	s := StartWith(t, ca, opts, "me@gmail.example", "pw", Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}})
	exists := make(chan uint32, 8)
	c, err := imapclient.DialTLS(s.Endpoint.Addr(), &imapclient.Options{
		TLSConfig: &tls.Config{RootCAs: ca.Pool, MinVersion: tls.VersionTLS12},
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(d *imapclient.UnilateralDataMailbox) {
				if d.NumMessages != nil {
					exists <- *d.NumMessages
				}
			},
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
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the idling session did not ask Options.IdleTicks for its ticks within 2s")
	}

	writer := dial(t, s, ca)
	appendRaw(t, writer, "[Gmail]/All Mail", "Subject: Pushed\r\n\r\nx\r\n")
	select {
	case n := <-exists:
		t.Fatalf("EXISTS %d during IDLE before a tick; want it only at the next tick", n)
	case <-time.After(30 * time.Millisecond):
	}
	ticks <- time.Now()
	select {
	case n := <-exists:
		if n != 2 {
			t.Errorf("EXISTS after the tick = %d, want 2", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no EXISTS within 2s of a tick after another session's APPEND")
	}
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := idle.Wait(); err != nil {
		t.Errorf("IDLE ended with %v, want OK", err)
	}
}
