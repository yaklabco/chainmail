package allow

import "testing"

func TestValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		entry string
		want  bool
	}{
		{"a@b.com", true},
		{"First.Last+tag@sub.example.org", true},
		{"*@ainslies.us", true},
		{"*@xn--bcher-kva.example", true},
		{"", false},
		{"*", false},
		{"*@*", false},
		{"*@", false},
		{"@example.com", false},
		{"bob@*", false},
		{"*.example.com", false},
		{"Bob <bob@x.com>", false},
		{"<bob@x.com>", false},
		{"a@b@c", false},
		{"*@exa mple.com", false},
		{"*@a*b.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.entry, func(t *testing.T) {
			t.Parallel()
			if got := Valid(tt.entry); got != tt.want {
				t.Errorf("Valid(%q) = %v, want %v", tt.entry, got, tt.want)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		entry, addr string
		want        bool
	}{
		{"*@x.com", "bob@x.com", true},
		{"*@x.com", "bob@sub.x.com", false},
		{"*@x.com", "bob@x.com.evil", false},
		{"bob@x.com", "bob@x.com", true},
		{"bob@x.com", "rob@x.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.entry+"/"+tt.addr, func(t *testing.T) {
			t.Parallel()
			if got := Match(tt.entry, tt.addr); got != tt.want {
				t.Errorf("Match(%q, %q) = %v, want %v", tt.entry, tt.addr, got, tt.want)
			}
		})
	}
}
