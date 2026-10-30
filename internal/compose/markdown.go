package compose

import (
	"bytes"
	"fmt"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// markdown renders GitHub-flavoured Markdown: tables, strikethrough,
// autolinks and task lists. Raw HTML in the source is left out, as the
// renderer is never given html.WithUnsafe, and so are links with dangerous
// schemes such as javascript:. A goldmark.Markdown is safe for concurrent use.
var markdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

// RenderMarkdown renders src to an HTML fragment.
func RenderMarkdown(src string) (string, error) {
	var buf bytes.Buffer
	if err := markdown.Convert([]byte(src), &buf); err != nil {
		return "", fmt.Errorf("rendering markdown: %w", err)
	}
	return buf.String(), nil
}
