package imapstore

import (
	"context"
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

// isThrottled is true for Gmail's bandwidth text on any command, and for
// the OVERQUOTA and LIMIT codes only on a message download (asDownload):
// elsewhere they are a storage quota or an implementation limit.
func TestIsThrottled(t *testing.T) {
	t.Parallel()
	overQuota := &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeOverQuota, Text: "quota"}
	limit := &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeLimit, Text: "slow down"}
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"OVERQUOTA on a download", asDownload(overQuota), true},
		{"LIMIT on a download", asDownload(limit), true},
		{"OVERQUOTA elsewhere", overQuota, false},
		{"LIMIT elsewhere", limit, false},
		{"Gmail's text without a code", &imap.Error{Type: imap.StatusResponseTypeNo, Text: gmailBandwidthText}, true},
		{"Gmail's text on a download", asDownload(&imap.Error{Type: imap.StatusResponseTypeNo, Text: gmailBandwidthText}), true},
		{"Gmail's text with ALERT", &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlert, Text: gmailBandwidthText}, true},
		{"Gmail's text in a BYE", fmt.Errorf("%w: %w", errConnLost, &imap.Error{Type: imap.StatusResponseTypeBye, Text: gmailBandwidthText}), true},
		{"wrapped download", fmt.Errorf("fetching: %w", asDownload(overQuota)), true},
		{"wrapped elsewhere", fmt.Errorf("fetching: %w", overQuota), false},
		{"another code on a download", asDownload(&imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeServerBug, Text: "fetch failed"}), false},
		{"another text", &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlert, Text: "Too many simultaneous connections. (Failure)"}, false},
		{"not a reply", errors.New(gmailBandwidthText), false},
		{"nil", nil, false},
	} {
		if got := isThrottled(tt.err); got != tt.want {
			t.Errorf("%s: isThrottled(%v) = %t, want %t", tt.name, tt.err, got, tt.want)
		}
	}
}

// asDownload keeps an error's text: only its classification changes.
func TestAsDownloadKeepsTheError(t *testing.T) {
	t.Parallel()
	reply := &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeServerBug, Text: "fetch failed"}
	err := asDownload(reply)
	var got *imap.Error
	if err.Error() != reply.Error() || !errors.As(err, &got) || got != reply {
		t.Errorf("asDownload(%v) = %v, want the same text and reply", reply, err)
	}
	if asDownload(nil) != nil {
		t.Error("asDownload(nil) is not nil")
	}
	var d *downloadError
	if errors.As(asDownload(context.Canceled), &d) {
		t.Error("asDownload marked an error that holds no server reply")
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

// checkUnthrottled fails unless err is the plain ErrUnavailable a server
// refusal always gave: not ErrThrottled, with the server's reply and text.
func checkUnthrottled(t *testing.T, err error, refusal string) {
	t.Helper()
	if !errors.Is(err, mailstore.ErrUnavailable) || errors.Is(err, mailstore.ErrThrottled) {
		t.Fatalf("err = %v, want ErrUnavailable and not ErrThrottled", err)
	}
	var reply *imap.Error
	if !errors.As(err, &reply) {
		t.Errorf("err = %v lost the server's reply", err)
	}
	_, text, _ := strings.Cut(refusal, " ")
	if _, after, ok := strings.Cut(text, "] "); ok {
		text = after
	}
	if !strings.Contains(err.Error(), text) {
		t.Errorf("err = %q lost the server's text %q", err, text)
	}
}

// Gmail's bandwidth text refuses for a limit on any command. The OVERQUOTA
// and LIMIT codes do only on a message download, a FETCH of headers or
// bodies: on a UID SEARCH, LIMIT is an implementation limit such as too
// many results, and keeps the refusal ErrUnavailable with its text. A
// throttle is ErrThrottled, without the server's text. Neither is retried.
func TestServerThrottlingIsThrottled(t *testing.T) {
	t.Parallel()
	for name, refusal := range throttledRefusals {
		gmail := strings.Contains(refusal, gmailBandwidthText)
		for _, tc := range []struct {
			cmd, refuse string
			throttled   bool
		}{
			{cmd: "FetchMessages", refuse: "FETCH", throttled: true},
			{cmd: "FetchHeaders", refuse: "FETCH", throttled: true},
			{cmd: "Search", refuse: "SEARCH", throttled: gmail},
		} {
			t.Run(name+" "+tc.cmd, func(t *testing.T) {
				t.Parallel()
				srv := startScriptServer(t, &scriptServer{n: 1, refuse: tc.refuse, refusal: refusal})
				s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
				var err error
				sent := &srv.fetches
				switch tc.cmd {
				case "Search":
					sent = &srv.searches
					var res []mailstore.SourceResult
					res, err = s.Search(t.Context(), mailstore.SearchRequest{Bounds: map[string]mailstore.Bound{"INBOX": {UIDValidity: scriptUIDValidity}}})
					if err == nil && len(res) == 1 {
						err = res[0].Err
					}
				case "FetchHeaders":
					_, err = s.FetchHeaders(t.Context(), mailstore.Source{Folder: "INBOX", UIDValidity: scriptUIDValidity}, []uint32{1})
				default:
					err = fetchWithin(t, s, scriptRefs(1), 5*time.Second)[0].Err
				}
				if tc.throttled {
					checkThrottled(t, err, refusal)
				} else {
					checkUnthrottled(t, err, refusal)
				}
				if n, c := sent.Load(), srv.conns.Load(); n != 1 || c != 1 {
					t.Errorf("%s sent %d times on %d connections, want once on one", tc.refuse, n, c)
				}
			})
		}
	}
}

// An APPEND refused for a full mailbox, NO [OVERQUOTA], is a storage
// quota, not a download limit: it keeps the ErrUnavailable refusal with
// the server's explanation, as before throttling was classified.
func TestAppendOverQuotaIsNotThrottled(t *testing.T) {
	t.Parallel()
	const refusal = "NO [OVERQUOTA] Mailbox is full"
	srv := startWriteServer(t, &writeServer{
		caps:    "UIDPLUS",
		folders: []*wsFolder{wsMailbox("INBOX", "", 7), wsMailbox("Drafts", "\\Drafts", 40)},
		intercept: func(c wsCmd) (string, bool) {
			if c.kind == "APPEND" {
				return c.tag + " " + refusal + "\r\n", true
			}
			return "", false
		},
	})
	s := writeStore(t, srv, Options{})
	_, err := s.Append(t.Context(), "Drafts", nil, time.Time{}, []byte("Subject: hi\r\n\r\nbody\r\n"))
	checkUnthrottled(t, err, refusal)
	var reply *imap.Error
	if errors.As(err, &reply) && reply.Code != imap.ResponseCodeOverQuota {
		t.Errorf("reply code = %q, want %q", reply.Code, imap.ResponseCodeOverQuota)
	}
	if n := srv.sent("APPEND"); n != 1 {
		t.Errorf("APPENDs = %d, want 1", n)
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
