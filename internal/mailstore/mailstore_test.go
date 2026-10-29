package mailstore

import "testing"

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
