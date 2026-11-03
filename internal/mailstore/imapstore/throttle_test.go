package imapstore

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/provider"
)

// gmailBandwidthText is the text Gmail sends when an account has used up
// its IMAP command or download allowance.
const gmailBandwidthText = "Account exceeded command or bandwidth limits. (Failure)"

func TestIsThrottled(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"OVERQUOTA", &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeOverQuota, Text: "quota"}, true},
		{"LIMIT", &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "slow down"}, true},
		{"Gmail's text without a code", &imap.Error{Type: imap.StatusResponseTypeNo, Text: gmailBandwidthText}, true},
		{"Gmail's text with ALERT", &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlert, Text: gmailBandwidthText}, true},
		{"Gmail's text in a BYE", fmt.Errorf("%w: %w", errConnLost, &imap.Error{Type: imap.StatusResponseTypeBye, Text: gmailBandwidthText}), true},
		{"wrapped", fmt.Errorf("fetching: %w", &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeOverQuota}), true},
		{"another code", &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeServerBug, Text: "fetch failed"}, false},
		{"another text", &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlert, Text: "Too many simultaneous connections. (Failure)"}, false},
		{"not a reply", errors.New(gmailBandwidthText), false},
		{"nil", nil, false},
	} {
		if got := isThrottled(tt.err); got != tt.want {
			t.Errorf("%s: isThrottled(%v) = %t, want %t", tt.name, tt.err, got, tt.want)
		}
	}
}

// throttledRefusals are the refusals a server sends for a quota or
// bandwidth limit, as a scripted server sends them.
var throttledRefusals = map[string]string{
	"OVERQUOTA":   "NO [OVERQUOTA] download quota exceeded",
	"LIMIT":       "NO [LIMIT] too much for today",
	"Gmail's NO":  "NO " + gmailBandwidthText,
	"Gmail ALERT": "NO [ALERT] " + gmailBandwidthText,
}

// checkThrottled fails unless err is ErrThrottled, which is also
// ErrUnavailable, and carries none of the server's text.
func checkThrottled(t *testing.T, err error, refusal string) {
	t.Helper()
	if !errors.Is(err, mailstore.ErrThrottled) || !errors.Is(err, mailstore.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrThrottled and ErrUnavailable", err)
	}
	var reply *imap.Error
	if errors.As(err, &reply) {
		t.Errorf("err = %v carries the server's reply", err)
	}
	_, text, _ := strings.Cut(refusal, " ")
	if _, after, ok := strings.Cut(text, "] "); ok {
		text = after
	}
	if strings.Contains(err.Error(), text) {
		t.Errorf("err = %q carries the server's text %q", err, text)
	}
}

// A refusal for a quota or bandwidth limit, of a whole FETCH or SEARCH,
// is ErrThrottled, without the server's text, and is not retried.
func TestServerThrottlingIsThrottled(t *testing.T) {
	t.Parallel()
	for name, refusal := range throttledRefusals {
		for _, cmd := range []string{"FETCH", "SEARCH"} {
			t.Run(name+" "+cmd, func(t *testing.T) {
				t.Parallel()
				srv := startScriptServer(t, &scriptServer{n: 1, refuse: cmd, refusal: refusal})
				s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
				var err error
				sent := &srv.fetches
				if cmd == "SEARCH" {
					sent = &srv.searches
					var res []mailstore.SourceResult
					res, err = s.Search(t.Context(), mailstore.SearchRequest{Bounds: map[string]mailstore.Bound{"INBOX": {UIDValidity: scriptUIDValidity}}})
					if err == nil && len(res) == 1 {
						err = res[0].Err
					}
				} else {
					err = fetchWithin(t, s, scriptRefs(1), 5*time.Second)[0].Err
				}
				checkThrottled(t, err, refusal)
				if n, c := sent.Load(), srv.conns.Load(); n != 1 || c != 1 {
					t.Errorf("%s sent %d times on %d connections, want once on one", cmd, n, c)
				}
			})
		}
	}
}

// A refusal for a quota or bandwidth limit of one message's text fails
// that message as ErrThrottled, not as a message the server cannot read.
func TestFetchMessagesThrottledTextIsThrottled(t *testing.T) {
	t.Parallel()
	for name, refusal := range throttledRefusals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := startScriptServer(t, &scriptServer{n: 3, refuseText: 2, refuseTextWith: refusal})
			s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
			res := fetchWithin(t, s, scriptRefs(srv.n), 5*time.Second)
			checkThrottled(t, res[1].Err, refusal)
			for _, i := range []int{0, 2} {
				if res[i].Err != nil {
					t.Errorf("ref %d: %v", i+1, res[i].Err)
				}
			}
		})
	}
}

// A connection the server closes with Gmail's bandwidth text is
// ErrThrottled at once: a fresh connection would only be refused again.
func TestRetryStopsOnAThrottlingBye(t *testing.T) {
	t.Parallel()
	s := offlineStore(t)
	tries := 0
	err := s.retry(t.Context(), func() error {
		tries++
		return fmt.Errorf("%w: %w", errConnLost, &imap.Error{Type: imap.StatusResponseTypeBye, Text: gmailBandwidthText})
	})
	checkThrottled(t, err, "BYE "+gmailBandwidthText)
	if tries != 1 {
		t.Errorf("tries = %d, want 1", tries)
	}
}

// A connection limit at login stays ErrConnLimit and a rejected login
// stays a rejection, whatever their text: neither is a throttle.
func TestRetryKeepsLoginClassesOverThrottling(t *testing.T) {
	t.Parallel()
	s := offlineStore(t)
	limit := &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "Too many simultaneous connections"}
	connLimit := fmt.Errorf("%w: %w: host refused another connection: %w", mailstore.ErrUnavailable, mailstore.ErrConnLimit, limit)
	if err := s.retry(t.Context(), func() error { return connLimit }); !errors.Is(err, mailstore.ErrConnLimit) || errors.Is(err, mailstore.ErrThrottled) {
		t.Errorf("connection limit: err = %v, want ErrConnLimit and not ErrThrottled", err)
	}
	rejected := &auth.RejectedError{Err: fmt.Errorf("%w: %w", auth.ErrRejected, &imap.Error{Type: imap.StatusResponseTypeNo, Text: gmailBandwidthText})}
	if err := s.retry(t.Context(), func() error { return rejected }); !errors.Is(err, auth.ErrRejected) || errors.Is(err, mailstore.ErrThrottled) {
		t.Errorf("rejected login: err = %v, want auth.ErrRejected and not ErrThrottled", err)
	}
}

// offlineStore is a Store for tests that call its methods without a
// server: nothing it is asked dials.
func offlineStore(t *testing.T) *Store {
	t.Helper()
	ep := provider.Endpoint{Host: "127.0.0.1", Port: 1, Security: provider.SecurityTLS}
	s := New(testAccount("host", ep, false), passwordCreds("host@example.test", "pw"), Options{Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	return s
}
