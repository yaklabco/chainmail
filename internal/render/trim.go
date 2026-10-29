package render

import (
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// attributionRE matches the line a client writes above a quoted reply, in
// the languages James's correspondents use.
var attributionRE = []*regexp.Regexp{
	regexp.MustCompile(`^On .{4,200} wrote:$`),
	regexp.MustCompile(`^Am .{4,200} schrieb .{1,200}:$`),
	regexp.MustCompile(`^Le .{4,200} a écrit ?:$`),
	regexp.MustCompile(`^El .{4,200} escribió:$`),
	regexp.MustCompile(`^Op .{4,200} schreef .{1,200}:$`),
	regexp.MustCompile(`^Il .{4,200} ha scritto:$`),
	regexp.MustCompile(`^Em .{4,200} escreveu:$`),
	regexp.MustCompile(`^Den .{4,200} skrev .{1,200}:$`),
	regexp.MustCompile(`^.{1,200} <[^<>@\s]+@[^<>\s]+> (wrote|schrieb|a écrit|escribió|skrev|schreef|ha scritto|escreveu) ?:$`),
}

var (
	separatorRE = regexp.MustCompile(`^(_{8,}|-{3,} ?(Original Message|Ursprüngliche Nachricht|Message d'origine|Mensaje original|Messaggio originale) ?-{3,})$`)
	fromRE      = regexp.MustCompile(`^(From|Von|De|Da|Van|Från): .+`)
	// sentRE matches the line only Outlook's history header has. dateRE
	// matches the line any header has, so a pasted header block matches it
	// too, and it counts only after a separator line.
	sentRE    = regexp.MustCompile(`^(Sent|Gesendet|Envoyé|Enviado|Inviato|Verzonden|Skickat): .+`)
	dateRE    = regexp.MustCompile(`^(Date|Datum|Data|Fecha): .+`)
	forwardRE = regexp.MustCompile(`(?i)forwarded message|weitergeleiteten? nachricht|message transféré|message réexpédié|mensaje reenviado|messaggio inoltrato|doorgestuurd bericht|mensagem (re)?encaminhada|vidarebefordrat meddelande`)
	// dateEvidenceRE matches what every client attribution names: an
	// address, a year or a time of day.
	dateEvidenceRE = regexp.MustCompile(`@|\b(19|20)\d\d\b|\b\d{1,2}:\d\d|\b\d{1,4}[/.-]\d{1,2}[/.-]\d{1,4}\b`)
)

// trimText removes a trailing signature and then a trailing quoted or
// Outlook history block. fromHTML says s came from HTML, where a signature
// delimiter has lost its trailing space.
func trimText(s string, fromHTML bool) string {
	lines := strings.Split(s, "\n")
	return keptText(lines, trimEnd(lines, nil, fromHTML))
}

// trimHTMLText trims text converted from HTML. quoted[i] says lines[i] was
// inside a blockquote, so a signature delimiter or an Outlook header block
// there belongs to the quoted message and is never a cut point. A
// blockquote is not trimmed as >-quoted text: the parser closes an unclosed
// one at the end of the document, around any reply after it.
func trimHTMLText(lines []string, quoted []bool) string {
	return keptText(lines, trimEnd(lines, quoted, true))
}

// trimEnd returns how many lines to keep. quoted marks the lines that may
// not start a cut, and may be nil.
func trimEnd(lines []string, quoted []bool, fromHTML bool) int {
	end := trimSignature(lines, quoted, fromHTML)
	if q := trimQuoted(lines[:end], quoted); q < end {
		// A signature written above the quoted history is trailing now.
		end = trimSignature(lines[:q], quoted, fromHTML)
	}
	return end
}

func keptText(lines []string, end int) string {
	return strings.TrimRight(strings.Join(lines[:end], "\n"), " \t\n")
}

// inQuote reports whether quoted marks line i.
func inQuote(quoted []bool, i int) bool { return i < len(quoted) && quoted[i] }

// trimSignature returns where the kept text ends: before the last signature
// delimiter, or len(lines) when there is none. A delimiter stays when a
// forward marker follows it, because cutting there would drop the forwarded
// message, and Gmail writes the signature above a forward. It also stays
// when new text follows a quote below it, as in an interleaved reply.
func trimSignature(lines []string, quoted []bool, fromHTML bool) int {
	i := signatureLine(lines, quoted, fromHTML)
	if i < 0 {
		return len(lines)
	}
	afterQuote := false
	for k := i + 1; k < len(lines); k++ {
		l := lines[k]
		if !isQuoted(l) && forwardRE.MatchString(l) {
			return len(lines)
		}
		switch {
		case isQuoted(l) || inQuote(quoted, k):
			afterQuote = true
		case afterQuote && !blankLine(l):
			return len(lines)
		}
	}
	return i
}

func signatureLine(lines []string, quoted []bool, fromHTML bool) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if inQuote(quoted, i) {
			continue
		}
		if lines[i] == "-- " || (fromHTML && strings.TrimSpace(lines[i]) == "--") {
			return i
		}
	}
	return -1
}

// trimQuoted returns where the kept text ends: before a trailing history
// block, or len(lines) when there is none.
func trimQuoted(lines []string, quoted []bool) int {
	end := len(lines)
	for end > 0 && blankLine(lines[end-1]) {
		end--
	}
	if i := outlookHeader(lines[:end], quoted); i >= 0 {
		return i
	}
	q := end
	for q > 0 && (isQuoted(lines[q-1]) || blankLine(lines[q-1])) {
		q--
	}
	for q < end && blankLine(lines[q]) {
		q++
	}
	if q == end {
		return len(lines)
	}
	// Apple Mail quotes the attribution line along with the history.
	if isAttribution(unquote(lines[q])) {
		return q
	}
	if q+1 < end && isWrappedAttribution(unquote(lines[q]), unquote(lines[q+1])) {
		return q
	}
	a := q
	for a > 0 && blankLine(lines[a-1]) {
		a--
	}
	if a > 0 && isAttribution(lines[a-1]) {
		return a - 1
	}
	if a > 1 && isWrappedAttribution(lines[a-2], lines[a-1]) {
		return a - 2
	}
	return len(lines)
}

// outlookHeader returns the line where an Outlook-style history block
// starts, or -1. The history is not >-quoted, so its header block is the
// attribution. After a separator line, From: and then Sent: or Date: start
// the block. After a blank line only From: and Sent: do, since From: and
// Date: are also what a pasted message header looks like. A block whose
// separator or From: line quoted marks is part of a quoted message.
func outlookHeader(lines []string, quoted []bool) int {
	for i := 1; i < len(lines); i++ {
		j := i
		afterSeparator := false
		if separatorRE.MatchString(strings.TrimSpace(lines[i])) {
			if j = nextNonBlank(lines, i+1); j < 0 {
				continue
			}
			afterSeparator = true
		} else if !blankLine(lines[i-1]) {
			continue
		}
		if inQuote(quoted, i) || inQuote(quoted, j) {
			continue
		}
		if !fromRE.MatchString(strings.TrimSpace(lines[j])) || !headerWithin(lines, j+1, 4, afterSeparator) {
			continue
		}
		if p := prevNonBlank(lines, i-1); p >= 0 && forwardRE.MatchString(lines[p]) {
			continue
		}
		return i
	}
	return -1
}

// headerWithin reports whether one of the n lines from from is a Sent:
// line, or with dateOK, a Sent: or Date: line.
func headerWithin(lines []string, from, n int, dateOK bool) bool {
	for k := from; k < len(lines) && k < from+n; k++ {
		l := strings.TrimSpace(lines[k])
		if sentRE.MatchString(l) || dateOK && dateRE.MatchString(l) {
			return true
		}
	}
	return false
}

func nextNonBlank(lines []string, from int) int {
	for k := from; k < len(lines); k++ {
		if !blankLine(lines[k]) {
			return k
		}
	}
	return -1
}

func prevNonBlank(lines []string, from int) int {
	for k := from; k >= 0; k-- {
		if !blankLine(lines[k]) {
			return k
		}
	}
	return -1
}

func blankLine(s string) bool { return strings.TrimSpace(s) == "" }

func isQuoted(s string) bool { return strings.HasPrefix(strings.TrimLeft(s, " "), ">") }

// unquote strips leading quote markers and the spaces around them.
func unquote(s string) string {
	s = strings.TrimLeft(s, " ")
	for strings.HasPrefix(s, ">") {
		s = strings.TrimLeft(strings.TrimPrefix(s, ">"), " ")
	}
	return s
}

// isAttribution reports whether s is an attribution line. Every client names
// an address, a year or a time of day in one, which keeps ordinary
// sentences, such as "On Friday at 3pm, as you wrote:", from matching.
func isAttribution(s string) bool {
	s = strings.TrimSpace(s)
	if !dateEvidenceRE.MatchString(s) {
		return false
	}
	for _, re := range attributionRE {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// isWrappedAttribution reports whether an attribution line was wrapped onto
// first and second. A first line that ends a sentence is the writer's own
// text, not the start of an attribution.
func isWrappedAttribution(first, second string) bool {
	if f := strings.TrimSpace(first); f != "" && strings.ContainsAny(f[len(f)-1:], ".!?") {
		return false
	}
	return isAttribution(first + " " + second)
}

// trimHTML removes a trailing quoted block from an HTML document before it
// is converted, and reports whether it removed anything.
func trimHTML(doc *html.Node) bool {
	// The first marker in document order starts the history. A marker
	// nested inside it belongs to an older message, and a marker inside a
	// blockquote belongs to a quoted message, which a reply may follow.
	n := find(doc, func(n *html.Node) bool {
		return (isOutlookMarker(n) || hasClass(n, "gmail_quote")) && !inBlockquote(n)
	})
	switch {
	case n == nil:
		return false
	case isOutlookMarker(n):
		removeFrom(n)
		return true
	case textAfter(n), unquotedText(n):
		// An interleaved reply keeps its quotes. Gmail writes an inline
		// answer inside the gmail_quote, between the blockquotes.
		return false
	default:
		n.Parent.RemoveChild(n)
		return true
	}
}

// inBlockquote reports whether n has a blockquote ancestor.
func inBlockquote(n *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.DataAtom == atom.Blockquote {
			return true
		}
	}
	return false
}

// unquotedText reports whether n holds visible text outside its
// blockquotes and its gmail_attr attribution line.
func unquotedText(n *html.Node) bool {
	switch {
	case n.Type == html.TextNode:
		return strings.TrimSpace(n.Data) != ""
	case n.Type != html.ElementNode:
		return false
	case n.DataAtom == atom.Blockquote, hasClass(n, "gmail_attr"), skipped[n.DataAtom]:
		return false
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if unquotedText(c) {
			return true
		}
	}
	return false
}

func isOutlookMarker(n *html.Node) bool {
	id := attr(n, "id")
	return id == "divRplyFwdMsg" || id == "OLK_SRC_BODY_SECTION" || hasClass(n, "OLK_SRC_BODY_SECTION")
}

func hasClass(n *html.Node, class string) bool {
	return slices.Contains(strings.Fields(attr(n, "class")), class)
}

// find returns the first element in document order that matches.
func find(n *html.Node, match func(*html.Node) bool) *html.Node {
	if n.Type == html.ElementNode && match(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := find(c, match); f != nil {
			return f
		}
	}
	return nil
}

// removeFrom removes n and everything after it in document order, and a
// horizontal rule directly before it.
func removeFrom(n *html.Node) {
	if prev := prevElement(n); prev != nil && prev.DataAtom == atom.Hr {
		prev.Parent.RemoveChild(prev)
	}
	for cur := n; cur.Parent != nil; cur = cur.Parent {
		for s := cur.NextSibling; s != nil; {
			next := s.NextSibling
			cur.Parent.RemoveChild(s)
			s = next
		}
	}
	n.Parent.RemoveChild(n)
}

func prevElement(n *html.Node) *html.Node {
	for s := n.PrevSibling; s != nil; s = s.PrevSibling {
		if s.Type == html.TextNode && strings.TrimSpace(s.Data) == "" {
			continue
		}
		if s.Type == html.ElementNode {
			return s
		}
		return nil
	}
	return nil
}

// textAfter reports whether any visible text follows n in document order.
func textAfter(n *html.Node) bool {
	for cur := n; cur != nil; cur = cur.Parent {
		for s := cur.NextSibling; s != nil; s = s.NextSibling {
			if hasText(s) {
				return true
			}
		}
	}
	return false
}

func hasText(n *html.Node) bool {
	switch n.Type {
	case html.TextNode:
		return strings.TrimSpace(n.Data) != ""
	case html.ElementNode:
		if skipped[n.DataAtom] {
			return false
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if hasText(c) {
			return true
		}
	}
	return false
}
