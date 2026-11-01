package mcpserver

import (
	"slices"
	"testing"
)

func TestFlagNames(t *testing.T) {
	t.Parallel()
	flags, keywords := splitFlags([]string{`\Seen`, `\FLAGGED`, `\Answered`, `\Draft`, `\Deleted`, "$Forwarded", "project-x", `\Recent`})
	if !slices.Equal(flags, []string{"seen", "flagged", "answered", "draft", "deleted"}) {
		t.Errorf("flags = %q", flags)
	}
	want := []untrusted{{Untrusted: "$Forwarded"}, {Untrusted: "project-x"}, {Untrusted: `\Recent`}}
	if !slices.Equal(keywords, want) {
		t.Errorf("keywords = %+v, want %+v", keywords, want)
	}
	if flags, keywords := splitFlags(nil); flags == nil || len(flags) != 0 || keywords != nil {
		t.Errorf("splitFlags(nil) = %#v, %#v; want an empty flags array and no keywords", flags, keywords)
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
}
