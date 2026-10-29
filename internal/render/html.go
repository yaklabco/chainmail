package render

import (
	"bytes"
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
// images are dropped. HTML the tree parser refuses, such as elements nested
// deeper than it allows, is converted tag by tag instead, so the text always
// survives. The only error is a failure to read r.
func HTMLToText(r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("reading HTML: %w", err)
	}
	if s, ok := treeText(b); ok {
		return s, nil
	}
	return tokenText(b), nil
}

// treeText converts b through the tree parser, and reports false when the
// parser refuses it.
func treeText(b []byte) (string, bool) {
	doc, err := html.Parse(bytes.NewReader(b))
	if err != nil {
		return "", false
	}
	return htmlText(doc), true
}

func htmlText(doc *html.Node) string {
	w := &textWriter{atLineStart: true}
	w.node(doc)
	return w.finish()
}

// tokenText converts HTML from its token stream, with no tree. It is the
// fallback for documents html.Parse rejects. Tags need not balance: content
// inside a skipped element is dropped up to its end tag, and every other tag
// formats as it does in htmlText. The head is not skipped as a whole, because
// an unclosed <head> would swallow the body. Its text lives in title, script
// and style, which are skipped on their own. An <svg> ends early at an HTML
// tag that breaks out of foreign content, as in the tree parser, so an
// unclosed <svg> does not swallow the rest of the document.
func tokenText(b []byte) string {
	w := &textWriter{atLineStart: true}
	z := html.NewTokenizer(bytes.NewReader(b))
	var s skip
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break // io.EOF, or input the tokenizer cannot read further
		}
		tok := z.Token()
		a := tok.DataAtom
		if s.drop(z, tt, &tok) {
			continue
		}
		switch tt {
		case html.TextToken:
			w.text(tok.Data)
		case html.StartTagToken, html.SelfClosingTagToken:
			if a != atom.Head && skipped[a] {
				// The tokenizer reads a raw-text element's content as text
				// even when the tag is written <x/>, so that starts a skip too.
				if !voidElements[a] && (tt == html.StartTagToken || rawText[a]) {
					s = skip{el: a, depth: 1}
				}
				continue
			}
			w.open(&html.Node{Type: html.ElementNode, DataAtom: a, Data: tok.Data, Attr: tok.Attr})
			if tt == html.SelfClosingTagToken {
				w.close(a)
			}
		case html.EndTagToken:
			w.close(a)
		default: // comments and doctypes
		}
	}
	return w.finish()
}

// skip tracks a skipped element whose content the tokenizer pass is dropping.
type skip struct {
	el    atom.Atom // the skipped element, while depth > 0
	depth int       // open elements of type el
	// htmlIn counts the open svg integration points (desc, foreignObject and
	// title) inside a skipped <svg>. Their content is HTML, so it neither
	// breaks out of the svg nor escapes raw-text mode.
	htmlIn int
}

// drop reports whether the token belongs to the skipped element and must be
// dropped. A tag that breaks out of an <svg> ends the skip and is not dropped.
func (s *skip) drop(z *html.Tokenizer, tt html.TokenType, tok *html.Token) bool {
	if s.depth == 0 {
		return false
	}
	a := tok.DataAtom
	start := tt == html.StartTagToken || tt == html.SelfClosingTagToken
	if s.el == atom.Svg && s.htmlIn == 0 && start {
		if breaksOut(tok) {
			*s = skip{}
			return false
		}
		// The tree parser reads foreign content with raw-text mode off, so
		// an svg <title> or <script> does not swallow the markup after it.
		z.NextIsNotRawText()
		if tt == html.StartTagToken && svgHTMLPoints[a] {
			s.htmlIn++
		}
	}
	switch {
	case tt == html.StartTagToken && a == s.el:
		s.depth++
	case tt == html.EndTagToken && a == s.el:
		s.depth--
	case tt == html.EndTagToken && s.el == atom.Svg && s.htmlIn > 0 && svgHTMLPoints[a]:
		s.htmlIn--
	}
	if s.depth == 0 {
		*s = skip{}
	}
	return true
}

// breaksOut reports whether a start tag inside <svg> ends the svg, as the
// tree parser decides it for foreign content.
func breaksOut(tok *html.Token) bool {
	if tok.DataAtom != atom.Font {
		return breakout[tok.DataAtom]
	}
	for _, at := range tok.Attr {
		switch at.Key {
		case "color", "face", "size":
			return true
		}
	}
	return false
}

// breakout lists the HTML start tags that end foreign content (WHATWG HTML,
// "parsing tokens in foreign content"). <font> breaks out only with a color,
// face or size attribute.
var breakout = map[atom.Atom]bool{
	atom.B: true, atom.Big: true, atom.Blockquote: true, atom.Body: true, atom.Br: true,
	atom.Center: true, atom.Code: true, atom.Dd: true, atom.Div: true, atom.Dl: true,
	atom.Dt: true, atom.Em: true, atom.Embed: true, atom.H1: true, atom.H2: true,
	atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true, atom.Head: true,
	atom.Hr: true, atom.I: true, atom.Img: true, atom.Li: true, atom.Listing: true,
	atom.Menu: true, atom.Meta: true, atom.Nobr: true, atom.Ol: true, atom.P: true,
	atom.Pre: true, atom.Ruby: true, atom.S: true, atom.Small: true, atom.Span: true,
	atom.Strong: true, atom.Strike: true, atom.Sub: true, atom.Sup: true, atom.Table: true,
	atom.Tt: true, atom.U: true, atom.Ul: true, atom.Var: true,
}

// svgHTMLPoints are the svg elements whose content is HTML. The tokenizer
// lower-cases tag names, so foreignObject arrives as foreignobject.
var svgHTMLPoints = map[atom.Atom]bool{atom.Desc: true, atom.Foreignobject: true, atom.Title: true}

// voidElements in the skipped set have no end tag, so the tokenizer pass must
// not wait for one.
var voidElements = map[atom.Atom]bool{atom.Img: true, atom.Embed: true}

// rawText elements in the skipped set hold raw text up to their end tag, even
// when written self-closing, because the tokenizer reads them that way.
var rawText = map[atom.Atom]bool{
	atom.Title: true, atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Iframe: true,
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
	links        []openLink
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
	if skipped[n.DataAtom] {
		return
	}
	w.open(n)
	w.children(n)
	w.close(n.DataAtom)
}

func (w *textWriter) children(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.node(c)
	}
}

// open writes what comes before the content of element n. Only n's atom and
// attributes are read, so the tokenizer pass can hand it a lone tag.
func (w *textWriter) open(n *html.Node) {
	a := n.DataAtom
	switch a {
	case atom.Br:
		w.write("\n")
		w.pendingSpace = false
	case atom.Hr:
		w.blank()
	case atom.Pre:
		w.blank()
		w.pre++
	case atom.A:
		w.links = append(w.links, openLink{start: w.b.Len(), href: attr(n, "href")})
	case atom.Ul, atom.Ol:
		w.listEdge()
		w.lists++
	case atom.Li:
		w.lineBreak()
		w.prefix(strings.Repeat("  ", max(w.lists-1, 0)) + "- ")
	case atom.Tr:
		w.lineBreak()
		w.cells = 0
	case atom.Td, atom.Th:
		if w.cells > 0 {
			w.prefix("\t")
		}
		w.cells++
	default:
		switch {
		case blankBlocks[a]:
			w.blank()
		case lineBlocks[a]:
			w.lineBreak()
		}
	}
}

// close writes what comes after the content of an element. The counters
// never drop below zero, because the tokenizer pass sees unbalanced end tags.
func (w *textWriter) close(a atom.Atom) {
	switch a {
	case atom.Pre:
		w.pre = max(w.pre-1, 0)
		w.blank()
	case atom.A:
		if len(w.links) > 0 {
			l := w.links[len(w.links)-1]
			w.links = w.links[:len(w.links)-1]
			w.link(l)
		}
	case atom.Ul, atom.Ol:
		w.lists = max(w.lists-1, 0)
		w.listEdge()
	case atom.Li, atom.Tr:
		w.lineBreak()
	default:
		switch {
		case blankBlocks[a]:
			w.blank()
		case lineBlocks[a]:
			w.lineBreak()
		}
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

// openLink is an <a> whose text is being written.
type openLink struct {
	start int // offset in b where the link text begins
	href  string
}

// link follows the text of l with its target in parentheses when the target
// is a web or mail address that differs from the text. The target is cleaned
// as the WHATWG URL parser cleans its input, so a wrapped href stays on one
// line: leading and trailing C0 controls and spaces are trimmed, and tab, LF
// and CR are removed from anywhere. Interior spaces are kept.
func (w *textWriter) link(l openLink) {
	text := strings.TrimSpace(w.b.String()[l.start:])
	href := cleanHref(l.href)
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

// cleanHref trims C0 controls and spaces from both ends of href and removes
// every tab, LF and CR.
func cleanHref(href string) string {
	href = strings.TrimFunc(href, func(r rune) bool { return r <= ' ' })
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, href)
}
