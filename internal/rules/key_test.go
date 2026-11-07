package rules

import (
	"strings"
	"testing"

	"github.com/yaklabco/chainmail/internal/idempotency"
)

func TestMessageKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		gmail       bool
		msgID       uint64
		folder      string
		uidValidity uint32
		uid         uint32
		want        string
	}{
		{"google", true, 1781234567890123456, "[Gmail]/All Mail", 7, 42, "g:1781234567890123456"},
		{"google without a message ID falls back", true, 0, "[Gmail]/All Mail", 7, 42, "i:7:42:[Gmail]/All Mail"},
		{"imap", false, 0, "INBOX", 1700000000, 9, "i:1700000000:9:INBOX"},
		{"imap ignores a message ID", false, 55, "INBOX", 1, 2, "i:1:2:INBOX"},
		{"imap folder with colons", false, 0, "a:b", 4294967295, 4294967295, "i:4294967295:4294967295:a:b"},
	}
	for _, tt := range tests {
		if got := MessageKey(tt.gmail, tt.msgID, tt.folder, tt.uidValidity, tt.uid); got != tt.want {
			t.Errorf("%s: MessageKey = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestForwardKey(t *testing.T) {
	t.Parallel()
	const rule = "ru_01jcz8v2kq4f9w3m7xg5t6h8ya"
	key := ForwardKey(rule, "gmail", "g:1781234567890123456")
	// The suffix is the first 16 hex characters of
	// sha256("gmail\x00g:1781234567890123456").
	if want := "rule." + rule + ".4bfa16ce0cff448e"; key != want {
		t.Errorf("ForwardKey = %q, want %q", key, want)
	}
	if again := ForwardKey(rule, "gmail", "g:1781234567890123456"); again != key {
		t.Errorf("ForwardKey is not deterministic: %q then %q", key, again)
	}
	others := []string{
		ForwardKey(rule, "personal", "g:1781234567890123456"),
		ForwardKey(rule, "gmail", "g:1781234567890123457"),
		ForwardKey("ru_other", "gmail", "g:1781234567890123456"),
		// The separator keeps account and key apart.
		ForwardKey(rule, "gmailg", ":1781234567890123456"),
	}
	for _, o := range others {
		if o == key {
			t.Errorf("ForwardKey %q repeats for a different rule, account or message", o)
		}
	}
	longest := ForwardKey("ru_"+strings.Repeat("z", 60), "a", "i:4294967295:4294967295:"+strings.Repeat("f", 255))
	for _, k := range []string{key, longest} {
		if !idempotency.ValidKey(k) {
			t.Errorf("ForwardKey %q is not a valid idempotency key", k)
		}
	}
	if strings.Contains(longest, "fff") {
		t.Errorf("ForwardKey %q holds the folder name", longest)
	}
}
