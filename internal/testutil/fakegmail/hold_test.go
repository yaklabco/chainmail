package fakegmail

import (
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
)

// TestFakeGmailHoldReads holds a command the client sent after HoldReads:
// the server answers it only once the hold is released.
func TestFakeGmailHoldReads(t *testing.T) {
	t.Parallel()
	ca := tlstest.New(t)
	s := Start(t, ca, "me@gmail.example", "pw", Message{Raw: raw1, Date: time.Now(), Labels: []string{`\Inbox`}})
	c := dial(t, s, ca)
	held, release := s.HoldReads()
	answered := make(chan error, 1)
	var wg sync.WaitGroup
	// A failed assertion below must not leave the NOOP held: release the
	// hold and close the client, which ends the NOOP either way, before
	// joining its goroutine.
	t.Cleanup(func() {
		release()
		_ = c.Close() // the test is over; only ending the NOOP matters
		wg.Wait()
	})
	wg.Go(func() { answered <- c.Noop().Wait() })
	select {
	case <-held:
	case err := <-answered:
		t.Fatalf("NOOP answered (%v) while reads were held", err)
	case <-time.After(2 * time.Second):
		t.Fatal("the server did not hold the NOOP within 2s")
	}
	select {
	case err := <-answered:
		t.Fatalf("NOOP answered (%v) before the hold was released", err)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	release() // releasing twice is harmless
	select {
	case err := <-answered:
		if err != nil {
			t.Errorf("NOOP after the release = %v, want OK", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("NOOP not answered within 2s of the release")
	}
}
