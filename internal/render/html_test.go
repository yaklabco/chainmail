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

func TestHTMLToTextDeepNesting(t *testing.T) {
	t.Parallel()
	for _, tag := range []string{"div", "b", "span"} {
		t.Run(tag, func(t *testing.T) {
			t.Parallel()
			in := strings.Repeat("<"+tag+">", 600) + "The sentence survives." + strings.Repeat("</"+tag+">", 600)
			got, err := HTMLToText(strings.NewReader(in))
			if err != nil {
				t.Fatalf("HTMLToText: %v", err)
			}
			if got != "The sentence survives." {
				t.Errorf("HTMLToText = %q, want %q", got, "The sentence survives.")
			}
		})
	}
}

func TestHTMLToTextDeepNestingFormats(t *testing.T) {
	t.Parallel()
	deep := strings.Repeat("<div>", 600)
	tests := []struct {
		name, in, want string
	}{
		{"dropped elements", deep + "<head><title>T</title><style>p{}</style></head><p>Hi<script>alert(1)</script></p>", "Hi"},
		{"head without end tag", deep + "<head><title>T</title><body><p>Hi</p>", "Hi"},
		{"blocks", deep + "<p>One</p><p>Two</p><div>a</div><div>b</div>", "One\n\nTwo\n\na\nb"},
		{"line break", deep + "Line one<br>Line two", "Line one\nLine two"},
		{"list", deep + "<ul><li>One</li><li>Two</li></ul>", "- One\n- Two"},
		{"table", deep + "<table><tr><td>Name</td><td>Qty</td></tr></table>", "Name\tQty"},
		{"link", deep + `<p>See <a href="https://example.com/agenda">the agenda</a>.</p>`, "See the agenda (https://example.com/agenda)."},
		{"image", deep + `<p>Logo <img src="x.png" alt="Logo"> here <svg/>now</p>`, "Logo here now"},
		{"entities", deep + "Fish &amp; chips", "Fish & chips"},
		{"self-closing script", deep + "<p>Hi<script/>alert(1)</script> there</p>", "Hi there"},
		{"self-closing style", deep + "<p>Hi<style/>p{color:red}</style> there</p>", "Hi there"},
		{"self-closing title", deep + "<title/>Secret</title><p>Hi there</p>", "Hi there"},
		{"self-closing noscript", deep + "<p>Hi<noscript/>no js</noscript> there</p>", "Hi there"},
		{"self-closing iframe", deep + "<p>Hi<iframe/>framed</iframe> there</p>", "Hi there"},
		{"unclosed svg", deep + "<p>Hi</p><svg><p>Body after unclosed svg</p>", "Hi\n\nBody after unclosed svg"},
		{"svg br breaks out", deep + "<svg><text>drawn</text><br/>After</svg>", "After"},
		{"svg font with color breaks out", deep + `<svg><font color="red">Shown</font>`, "Shown"},
		{"svg font without attributes stays", deep + "<svg><font>drawn</font></svg>After", "After"},
		{"svg foreignObject keeps html inside", deep + "<svg><foreignObject><p>drawn</p></foreignObject></svg><p>After</p>", "After"},
		{"svg title keeps html inside", deep + "<svg><title><p>drawn</p></title></svg><p>After</p>", "After"},
		{"svg script is not raw text", deep + "<svg><script/><p>After</p>", "After"},
		{"svg nested title in foreignObject hides script", deep + `<svg><foreignObject><title>t</title><script>x="<p>secret-script";</script></foreignObject></svg><p>After</p>`, "After"},
		{"svg nested title hides style", deep + `<svg><title><title>t</title><style>a{}<p>secret-style</style></title></svg><p>After</p>`, "After"},
		{"svg nested desc hides script", deep + `<svg><desc><desc></desc><script><b>secret-desc</script></desc></svg><p>After</p>`, "After"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := HTMLToText(strings.NewReader(tt.in))
			if err != nil {
				t.Fatalf("HTMLToText: %v", err)
			}
			if got != tt.want {
				t.Errorf("HTMLToText = %q, want %q", got, tt.want)
			}
			// The fallback must format like the tree path does.
			tree, err := HTMLToText(strings.NewReader(strings.TrimPrefix(tt.in, deep)))
			if err != nil || tree != tt.want {
				t.Errorf("tree path = %q, %v; want %q", tree, err, tt.want)
			}
		})
	}
}

func TestHTMLToTextLinkTargetWhitespace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want string
	}{
		{"embedded newline", "<a href=\"https://x.test/a\n/b\r\n\">go</a>", "go (https://x.test/a/b)"},
		{"tab and leading space", "<a href=\" \thttps://x.test/a\t/b \">go</a>", "go (https://x.test/a/b)"},
		{"interior space", `<a href="mailto:a@b.test?subject=Meeting notes">write</a>`, "write (mailto:a@b.test?subject=Meeting notes)"},
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
