package mcpserver

import (
	"slices"
	"testing"
)

func TestFlagNames(t *testing.T) {
	t.Parallel()
	short := []struct{ in, want string }{
		{`\Seen`, "seen"},
		{`\Flagged`, "flagged"},
		{`\Answered`, "answered"},
		{`\Draft`, "draft"},
		{"$Forwarded", "$Forwarded"},
		{"project-x", "project-x"},
	}
	for _, tt := range short {
		if got := shortFlag(tt.in); got != tt.want {
			t.Errorf("shortFlag(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	imap := []struct{ in, want string }{
		{"seen", `\Seen`},
		{"SEEN", `\Seen`},
		{"Flagged", `\Flagged`},
		{"answered", `\Answered`},
		{"draft", `\Draft`},
		{`\Seen`, `\Seen`},
		{"$Work", "$Work"},
		{"project-x", "project-x"},
	}
	for _, tt := range imap {
		if got := imapFlag(tt.in); got != tt.want {
			t.Errorf("imapFlag(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := shortFlags([]string{`\Seen`, "$Work"}); !slices.Equal(got, []string{"seen", "$Work"}) {
		t.Errorf("shortFlags = %v", got)
	}
}
