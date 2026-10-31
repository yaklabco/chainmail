package mailaddr

import (
	"strings"
	"testing"
)

func TestSafe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		addr string
		want bool
	}{
		{"plain", "a@b.co", true},
		{"dots and plus", "first.last+tag@sub.example.org", true},
		{"apostrophe", "o'neil@x.ie", true},
		{"every atext symbol", "!#$%&'*+-/=?^_`{|}~@x.com", true},
		{"single-label domain", "me@localhost", true},
		{"hyphen inside a label", "a@x-y.example", true},
		{"punycode", "a@xn--bcher-kva.example", true},
		{"UTF-8 local part and domain", "用户@例子.广告", true},
		{"64-byte local part", strings.Repeat("a", 64) + "@x.com", true},
		{"254 bytes in all", strings.Repeat("a", 64) + "@" + longDomain(189), true},
		{"63-byte label", "a@" + strings.Repeat("b", 63) + ".com", true},

		{"the review payload, decoded", "victim@blocked.invalid> ORCPT=rfc822;x@example.net", false},
		{"space in the local part", "a b@x.com", false},
		{"two @", "a@b@c", false},
		{"leading dot", ".a@x.com", false},
		{"doubled dot", "a..b@x.com", false},
		{"trailing dot", "a.@x.com", false},
		{"label starts with a hyphen", "a@-x.com", false},
		{"label ends with a hyphen", "a@x-.com", false},
		{"address literal", "a@[1.2.3.4]", false},
		{"quoted local part", `"q"@x.com`, false},
		{"backslash", `a\b@x.com`, false},
		{"opening angle bracket", "a<b@x.com", false},
		{"closing angle bracket", "a@x.com>", false},
		{"65-byte local part", strings.Repeat("a", 65) + "@x.com", false},
		{"255 bytes in all", strings.Repeat("a", 64) + "@" + longDomain(190), false},
		{"64-byte label", "a@" + strings.Repeat("b", 64) + ".com", false},
		{"CR LF", "a@x.com\r\nRSET", false},
		{"NUL", "a\x00b@x.com", false},
		{"DEL", "a\x7fb@x.com", false},
		{"tab", "a\tb@x.com", false},
		{"C1 control", "a\u0085b@x.com", false},
		{"invalid UTF-8 in the local part", "a\xffb@x.com", false},
		{"invalid UTF-8 in the domain", "a@x\xff.com", false},
		{"truncated UTF-8", "a@x\xe4\xbd.com", false},
		{"empty", "", false},
		{"no @", "ab.com", false},
		{"empty local part", "@x.com", false},
		{"empty domain", "a@", false},
		{"empty label", "a@x..com", false},
		{"leading dot in the domain", "a@.x.com", false},
		{"trailing dot in the domain", "a@x.com.", false},
		{"comma", "a,b@x.com", false},
		{"semicolon", "a;b@x.com", false},
		{"colon", "a:b@x.com", false},
		{"parentheses", "a(b)@x.com", false},
		{"double quote", `a"b@x.com`, false},
		{"underscore in the domain", "a@x_y.com", false},
		{"atext symbol in the domain", "a@x+y.com", false},
		{"space in the domain", "a@x .com", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Safe(tt.addr); got != tt.want {
				t.Errorf("Safe(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

// Every byte that is a path or parameter delimiter, whitespace or a control
// character makes an otherwise safe address unsafe, wherever it appears.
func TestSafeRefusesEveryDelimiter(t *testing.T) {
	t.Parallel()
	bad := []byte("<>\"\\ \t,;:()[]@")
	for c := range byte(0x20) {
		bad = append(bad, c)
	}
	bad = append(bad, 0x7f)
	for _, c := range bad {
		for _, addr := range []string{
			"a" + string(c) + "b@x.com",
			"ab@x" + string(c) + "y.com",
			string(c) + "ab@x.com",
			"ab@x.com" + string(c),
		} {
			if Safe(addr) {
				t.Errorf("Safe(%q) = true, want false", addr)
			}
		}
	}
}

// longDomain returns a domain of n bytes made of 63-byte labels.
func longDomain(n int) string {
	var b strings.Builder
	for b.Len() < n {
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strings.Repeat("d", min(63, n-b.Len())))
	}
	return b.String()
}
