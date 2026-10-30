package compose

import (
	"strings"
	"testing"
)

func TestRenderMarkdown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		src     string
		want    []string
		notWant []string
	}{
		{
			name: "table",
			src:  "| a | b |\n|---|---|\n| 1 | 2 |\n",
			want: []string{"<table>", "<th>a</th>", "<td>2</td>"},
		},
		{
			name: "link",
			src:  "see [chainmail](https://ainslies.us/chainmail)",
			want: []string{`<a href="https://ainslies.us/chainmail">chainmail</a>`},
		},
		{
			name: "autolink",
			src:  "see https://ainslies.us today",
			want: []string{`<a href="https://ainslies.us">https://ainslies.us</a>`},
		},
		{
			name: "strikethrough",
			src:  "~~gone~~",
			want: []string{"<del>gone</del>"},
		},
		{
			name: "task list",
			src:  "- [x] done\n- [ ] todo\n",
			want: []string{`<input checked="" disabled="" type="checkbox"`, `<input disabled="" type="checkbox"`},
		},
		{
			name:    "raw html block is not passed through",
			src:     "<script>alert(1)</script>\n",
			notWant: []string{"<script>"},
		},
		{
			name:    "inline raw html is not passed through",
			src:     "hello <script>alert(1)</script> there",
			notWant: []string{"<script>"},
		},
		{
			name:    "javascript link is not a link",
			src:     "[x](javascript:alert(1))",
			notWant: []string{`href="javascript:`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := RenderMarkdown(tt.src)
			if err != nil {
				t.Fatalf("RenderMarkdown: %v", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("output %q lacks %q", got, w)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(got, w) {
					t.Errorf("output %q contains %q", got, w)
				}
			}
		})
	}
}

func TestRenderMarkdownEscapesScriptText(t *testing.T) {
	t.Parallel()
	got, err := RenderMarkdown("a `<script>` b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "<code>&lt;script&gt;</code>") {
		t.Errorf("output %q, want the script tag escaped in code", got)
	}
}
