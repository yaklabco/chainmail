package mailstore

import (
	"slices"
	"testing"
)

func TestRootMessageID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		references []string
		inReplyTo  []string
		messageID  string
		want       string
	}{
		{"references first", []string{"<root@x>", "<mid@x>"}, []string{"mid@x"}, "me@x", "root@x"},
		{"in-reply-to", nil, []string{"<parent@x>"}, "me@x", "parent@x"},
		{"own id", nil, nil, "<me@x>", "me@x"},
		{"blank entries skipped", []string{" ", "<r@x>"}, nil, "me@x", "r@x"},
		{"none", nil, nil, "", ""},
		{"raw References header", []string{"<root@x> <mid@x>"}, nil, "me@x", "root@x"},
		{"folded References header", []string{"\r\n\t<root@x>\r\n <mid@x>"}, nil, "me@x", "root@x"},
		{"raw In-Reply-To header", nil, []string{" <parent@x> <other@x>"}, "me@x", "parent@x"},
		{"bare brackets skipped", []string{"<>", "<r@x>"}, nil, "me@x", "r@x"},
		{"adjacent ids", []string{"<root@x><mid@x>"}, nil, "me@x", "root@x"},
		{"comment before the id", []string{"(old) <root@x>"}, nil, "me@x", "root@x"},
		{"phrase In-Reply-To", nil, []string{"Your message of Monday <parent@x>"}, "me@x", "parent@x"},
	}
	for _, tt := range tests {
		if got := RootMessageID(tt.references, tt.inReplyTo, tt.messageID); got != tt.want {
			t.Errorf("%s: RootMessageID = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestThreadKey(t *testing.T) {
	t.Parallel()
	a := ThreadKey("root@example.com")
	if len(a) != 16 || a != ThreadKey("root@example.com") || a == ThreadKey("other@example.com") {
		t.Errorf("ThreadKey = %q", a)
	}
	if ThreadKey("") != "" {
		t.Error("ThreadKey of no root should be empty")
	}
}

func TestMessageIDs(t *testing.T) {
	t.Parallel()
	got := MessageIDs([]string{"<a@x> <b@x>", "c@x", "(note) <d@x><e@x>", " ", "<>"})
	if want := []string{"a@x", "b@x", "c@x", "d@x", "e@x"}; !slices.Equal(got, want) {
		t.Errorf("MessageIDs = %q, want %q", got, want)
	}
}
