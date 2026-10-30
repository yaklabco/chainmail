package compose

import (
	"strings"
	"testing"
)

func TestSanitizeHTML(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    []string
		notWant []string
	}{
		{
			name:    "script dropped",
			in:      `<p>hi</p><script>alert(1)</script>`,
			want:    []string{"<p>hi</p>"},
			notWant: []string{"<script", "alert(1)"},
		},
		{
			name:    "style element dropped",
			in:      `<style>p{color:red}</style><p>hi</p>`,
			want:    []string{"<p>hi</p>"},
			notWant: []string{"<style", "color:red"},
		},
		{
			name:    "onclick dropped",
			in:      `<p onclick="alert(1)">hi</p>`,
			want:    []string{"<p>hi</p>"},
			notWant: []string{"onclick"},
		},
		{
			name:    "javascript link dropped",
			in:      `<a href="javascript:alert(1)">x</a>`,
			notWant: []string{"javascript:"},
		},
		{
			name: "styled span kept",
			in:   `<span style="color: red; font-weight: bold">x</span>`,
			want: []string{`<span style="color: red; font-weight: bold">x</span>`},
		},
		{
			name:    "unsafe style property dropped",
			in:      `<span style="position: fixed">x</span>`,
			notWant: []string{"position"},
		},
		{
			name: "cid image kept",
			in:   `<img src="cid:abc@chainmail" alt="chart">`,
			want: []string{`src="cid:abc@chainmail"`},
		},
		{
			name: "https image kept",
			in:   `<img src="https://example.com/a.png">`,
			want: []string{`src="https://example.com/a.png"`},
		},
		{
			name:    "http image source dropped",
			in:      `<img src="http://example.com/a.png">`,
			notWant: []string{"http://example.com/a.png"},
		},
		{
			name:    "data image source dropped",
			in:      `<img src="data:image/png;base64,AAAA">`,
			notWant: []string{"data:"},
		},
		{
			name: "https link kept",
			in:   `<a href="https://example.com/">x</a>`,
			want: []string{`href="https://example.com/"`},
		},
		{
			name: "blockquote kept",
			in:   `<blockquote type="cite"><p>q</p></blockquote>`,
			want: []string{"<blockquote", "<p>q</p></blockquote>"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := SanitizeHTML(tt.in)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("SanitizeHTML(%q) = %q, lacks %q", tt.in, got, w)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(got, w) {
					t.Errorf("SanitizeHTML(%q) = %q, contains %q", tt.in, got, w)
				}
			}
		})
	}
}
