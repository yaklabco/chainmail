package render

import (
	"fmt"
	"io"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// HTMLToText converts an HTML document to plain text. Block elements become
// line breaks, list items become "- " lines, links become "text (url)",
// table rows become tab-separated lines, and scripts, styles, the head and
// images are dropped.
func HTMLToText(r io.Reader) (string, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return "", fmt.Errorf("parsing HTML: %w", err)
	}
	return htmlText(doc), nil
}

func htmlText(doc *html.Node) string {
	w := &textWriter{atLineStart: true}
	w.node(doc)
	return w.finish()
}

// skipped elements contribute no text.
var skipped = map[atom.Atom]bool{
	atom.Head: true, atom.Title: true, atom.Script: true, atom.Style: true, atom.Noscript: true,
	atom.Template: true, atom.Img: true, atom.Svg: true, atom.Object: true, atom.Iframe: true,
	atom.Embed: true, atom.Picture: true, atom.Video: true, atom.Audio: true, atom.Canvas: true,
}

// blankBlocks are set off by a blank line, lineBlocks by a line break.
var (
	blankBlocks = map[atom.Atom]bool{
		atom.P: true, atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
		atom.Blockquote: true, atom.Table: true, atom.Dl: true, atom.Figure: true, atom.Address: true,
	}
	lineBlocks = map[atom.Atom]bool{
		atom.Div: true, atom.Section: true, atom.Article: true, atom.Header: true, atom.Footer: true,
		atom.Nav: true, atom.Aside: true, atom.Main: true, atom.Center: true, atom.Figcaption: true,
		atom.Dt: true, atom.Dd: true, atom.Caption: true, atom.Form: true, atom.Fieldset: true,
	}
)

// textWriter accumulates text, collapsing whitespace outside <pre>.
type textWriter struct {
	b            strings.Builder
	atLineStart  bool
	newlines     int // trailing newlines in b
	pendingSpace bool
	pre          int
	lists        int
	cells        int
}

func (w *textWriter) node(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		w.text(n.Data)
		return
	case html.DocumentNode:
		w.children(n)
		return
	case html.ElementNode:
	default:
		return // comments and doctypes
	}
	a := n.DataAtom
	if skipped[a] {
		return
	}
	switch a {
	case atom.Br:
		w.write("\n")
		w.pendingSpace = false
	case atom.Hr:
		w.blank()
	case atom.Pre:
		w.blank()
		w.pre++
		w.children(n)
		w.pre--
		w.blank()
	case atom.A:
		w.link(n)
	case atom.Ul, atom.Ol:
		w.listEdge()
		w.lists++
		w.children(n)
		w.lists--
		w.listEdge()
	case atom.Li:
		w.lineBreak()
		w.prefix(strings.Repeat("  ", max(w.lists-1, 0)) + "- ")
		w.children(n)
		w.lineBreak()
	case atom.Tr:
		w.lineBreak()
		w.cells = 0
		w.children(n)
		w.lineBreak()
	case atom.Td, atom.Th:
		if w.cells > 0 {
			w.prefix("\t")
		}
		w.cells++
		w.children(n)
	default:
		switch {
		case blankBlocks[a]:
			w.blank()
			w.children(n)
			w.blank()
		case lineBlocks[a]:
			w.lineBreak()
			w.children(n)
			w.lineBreak()
		default:
			w.children(n)
		}
	}
}

func (w *textWriter) children(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.node(c)
	}
}

// listEdge separates a top-level list by a blank line and a nested one by a line break.
func (w *textWriter) listEdge() {
	if w.lists == 0 {
		w.blank()
		return
	}
	w.lineBreak()
}

// link writes the link text, then the target in parentheses when it is a
// web or mail address that differs from the text.
func (w *textWriter) link(n *html.Node) {
	start := w.b.Len()
	w.children(n)
	text := strings.TrimSpace(w.b.String()[start:])
	href := strings.TrimSpace(attr(n, "href"))
	if text == "" || !showableLink(href) || href == text || strings.TrimPrefix(href, "mailto:") == text {
		return
	}
	w.pendingSpace = true
	w.word("(" + href + ")")
}

func showableLink(href string) bool {
	lower := strings.ToLower(href)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "mailto:")
}

func (w *textWriter) text(s string) {
	if w.pre > 0 {
		w.write(strings.ReplaceAll(s, "\r\n", "\n"))
		w.pendingSpace = false
		return
	}
	words := strings.FieldsFunc(s, unicode.IsSpace)
	if len(words) == 0 {
		if s != "" {
			w.pendingSpace = true
		}
		return
	}
	if r := []rune(s); unicode.IsSpace(r[0]) {
		w.pendingSpace = true
	}
	for i, word := range words {
		if i > 0 {
			w.pendingSpace = true
		}
		w.word(word)
	}
	if r := []rune(s); unicode.IsSpace(r[len(r)-1]) {
		w.pendingSpace = true
	}
}

func (w *textWriter) word(s string) {
	if w.pendingSpace && !w.atLineStart {
		w.write(" ")
	}
	w.pendingSpace = false
	w.write(s)
}

// prefix writes s, such as a list marker or a cell separator, with no space before it.
func (w *textWriter) prefix(s string) {
	w.pendingSpace = false
	w.write(s)
}

func (w *textWriter) write(s string) {
	if s == "" {
		return
	}
	w.b.WriteString(s)
	trailing := len(s) - len(strings.TrimRight(s, "\n"))
	if trailing == len(s) {
		w.newlines += trailing
	} else {
		w.newlines = trailing
	}
	w.atLineStart = trailing > 0
}

func (w *textWriter) lineBreak() {
	if !w.atLineStart {
		w.write("\n")
	}
	w.pendingSpace = false
}

func (w *textWriter) blank() {
	w.lineBreak()
	if w.b.Len() > 0 && w.newlines < 2 {
		w.write("\n")
	}
}

// finish trims line ends, collapses runs of blank lines to one, and trims
// blank lines at both ends.
func (w *textWriter) finish() string {
	lines := strings.Split(w.b.String(), "\n")
	out := lines[:0]
	blanks := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			blanks++
			if blanks > 1 {
				continue
			}
		} else {
			blanks = 0
		}
		out = append(out, l)
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
