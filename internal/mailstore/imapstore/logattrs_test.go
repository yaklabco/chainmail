package imapstore

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/yaklabco/chainmail/internal/mailstore"
)

// TestLogAttrsKeepServerTextOut logs errors from partial writes, dropped
// connections and refused commands whose text holds a private marker, as a
// server echoing what it was sent would. The attributes give the reason
// and, for a tagged reply, its status and a known response code, never the
// text.
func TestLogAttrsKeepServerTextOut(t *testing.T) {
	t.Parallel()
	const private = "PRIVATE-SERVER-TEXT"
	reply := &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAuthenticationFailed, Text: private}
	tests := []struct {
		name string
		err  error
		want []string
	}{
		{"refused", fmt.Errorf("storing: %w", reply), []string{"reason=refused", "status=NO", "code=AUTHENTICATIONFAILED"}},
		{"unknown code", &imap.Error{Type: imap.StatusResponseTypeBad, Code: imap.ResponseCode(private), Text: private}, []string{"reason=refused", "status=BAD"}},
		{"connection lost", fmt.Errorf("%w: %w", errConnLost, &imap.Error{Type: imap.StatusResponseTypeBye, Text: private}), []string{"reason=connection_lost", "status=BYE"}},
		{"command timeout", fmt.Errorf("%w: %w within 1s", errConnLost, errCommandTimeout), []string{"reason=command_timeout"}},
		{"unavailable", fmt.Errorf("%w: %s", mailstore.ErrUnavailable, private), []string{"reason=unavailable"}},
		{"malformed", fmt.Errorf("%w: %s", mailstore.ErrMalformed, private), []string{"reason=malformed"}},
		{"stale", fmt.Errorf("%w: %s", mailstore.ErrStaleRef, private), []string{"reason=stale_ref"}},
		{"cancelled", fmt.Errorf("%s: %w", private, context.Canceled), []string{"reason=canceled"}},
		{"other", fmt.Errorf("%s", private), []string{"reason=other"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			slog.New(slog.NewTextHandler(&buf, nil)).Info("a write failed", logAttrs(tt.err, "account", "a")...)
			out := buf.String()
			if !strings.Contains(out, "account=a") {
				t.Errorf("log line %q lost the attributes before the error's", out)
			}
			if strings.Contains(out, private) {
				t.Errorf("log line holds the server's text: %s", out)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("log line %q lacks %q", out, w)
				}
			}
		})
	}
}
