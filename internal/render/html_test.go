package render

import (
	"strings"
	"testing"
)

func TestHTMLToText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want string
	}{
		{"paragraphs", "<p>One</p><p>Two</p>", "One\n\nTwo"},
		{"line break", "Line one<br>Line two", "Line one\nLine two"},
		{"list", "<ul><li>One</li><li>Two</li></ul>", "- One\n- Two"},
		{"nested list", "<ul><li>a<ul><li>b</li></ul></li></ul>", "- a\n  - b"},
		{"ordered list", "<ol><li>First</li><li>Second</li></ol>", "- First\n- Second"},
		{"link", `<p>See <a href="https://example.com/agenda">the agenda</a>.</p>`, "See the agenda (https://example.com/agenda)."},
		{"link same as text", `<a href="https://x.test">https://x.test</a>`, "https://x.test"},
		{"mailto same as text", `<a href="mailto:a@x.test">a@x.test</a>`, "a@x.test"},
		{"javascript link", `<a href="javascript:alert(1)">click</a>`, "click"},
		{"image link", `<a href="https://x.test"><img src="logo.png"></a>`, ""},
		{"table", "<table><tr><td>Name</td><td>Qty</td></tr><tr><td>Widget</td><td>2</td></tr></table>", "Name\tQty\nWidget\t2"},
		{"dropped elements", "<head><title>T</title><style>p{}</style></head><body><p>Hi<script>alert(1)</script></p></body>", "Hi"},
		{"image", `<p>Logo <img src="x.png" alt="Logo"> here</p>`, "Logo here"},
		{"pre", "<pre>  x\n  y</pre>", "  x\n  y"},
		{"entities", "Fish &amp; chips&nbsp;today", "Fish & chips today"},
		{"heading", "<h1>Title</h1><p>Body</p>", "Title\n\nBody"},
		{"divs", "<div>a</div><div>b</div>", "a\nb"},
		{"blank runs collapse", "<p>a</p><br><br><br><br><p>b</p>", "a\n\nb"},
		{"whitespace collapses", "<p>  lots   of\n\n spaces  </p>", "lots of spaces"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := HTMLToText(strings.NewReader(tt.in))
			if err != nil {
				t.Fatalf("HTMLToText: %v", err)
			}
			if got != tt.want {
				t.Errorf("HTMLToText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
