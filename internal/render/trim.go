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
	sentRE      = regexp.MustCompile(`^(Sent|Date|Gesendet|Datum|Envoyé|Enviado|Fecha|Inviato|Data|Verzonden|Skickat): .+`)
	forwardRE   = regexp.MustCompile(`(?i)forwarded message|weitergeleiteten? nachricht|message transféré|message réexpédié|mensaje reenviado|messaggio inoltrato|doorgestuurd bericht|mensagem (re)?encaminhada|vidarebefordrat meddelande`)
)

// trimText removes a trailing signature and then a trailing quoted or
// Outlook history block. fromHTML says s came from HTML, where a signature
// delimiter has lost its trailing space.
func trimText(s string, fromHTML bool) string {
	lines := strings.Split(s, "\n")
	end := trimSignature(lines, fromHTML)
	if q := trimQuoted(lines[:end]); q < end {
		// A signature written above the quoted history is trailing now.
		end = trimSignature(lines[:q], fromHTML)
	}
	return strings.TrimRight(strings.Join(lines[:end], "\n"), " \t\n")
}

// trimSignature returns where the kept text ends: before the last signature
// delimiter, or len(lines) when there is none. A delimiter with a forward
// marker after it stays, because cutting there would drop the forwarded
// message. Gmail writes the signature above a forward.
func trimSignature(lines []string, fromHTML bool) int {
	i := signatureLine(lines, fromHTML)
	if i < 0 {
		return len(lines)
	}
	for _, l := range lines[i+1:] {
		if !isQuoted(l) && forwardRE.MatchString(l) {
			return len(lines)
		}
	}
	return i
}

func signatureLine(lines []string, fromHTML bool) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i] == "-- " || (fromHTML && strings.TrimSpace(lines[i]) == "--") {
			return i
		}
	}
	return -1
}

// trimQuoted returns where the kept text ends: before a trailing history
// block, or len(lines) when there is none.
func trimQuoted(lines []string) int {
	end := len(lines)
	for end > 0 && blankLine(lines[end-1]) {
		end--
	}
	if i := outlookHeader(lines[:end]); i >= 0 {
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
	if q+1 < end && isAttribution(unquote(lines[q])+" "+unquote(lines[q+1])) {
		return q
	}
	a := q
	for a > 0 && blankLine(lines[a-1]) {
		a--
	}
	if a > 0 && isAttribution(lines[a-1]) {
		return a - 1
	}
	if a > 1 && isAttribution(lines[a-2]+" "+lines[a-1]) {
		return a - 2
	}
	return len(lines)
}

// outlookHeader returns the line where an Outlook-style history block
// starts, or -1. The history is not >-quoted, so its header block is the
// attribution.
func outlookHeader(lines []string) int {
	for i := 1; i < len(lines); i++ {
		j := i
		if separatorRE.MatchString(strings.TrimSpace(lines[i])) {
			if j = nextNonBlank(lines, i+1); j < 0 {
				continue
			}
		} else if !blankLine(lines[i-1]) {
			continue
		}
		if !fromRE.MatchString(strings.TrimSpace(lines[j])) || !sentWithin(lines, j+1, 4) {
			continue
		}
		if p := prevNonBlank(lines, i-1); p >= 0 && forwardRE.MatchString(lines[p]) {
			continue
		}
		return i
	}
	return -1
}

func sentWithin(lines []string, from, n int) bool {
	for k := from; k < len(lines) && k < from+n; k++ {
		if sentRE.MatchString(strings.TrimSpace(lines[k])) {
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
// a date or an address in one, which keeps ordinary sentences, such as two
// lines that join into "On reflection ... you wrote:", from matching.
func isAttribution(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.ContainsAny(s, "0123456789@") {
		return false
	}
	for _, re := range attributionRE {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// trimHTML removes a trailing quoted block from an HTML document before it
// is converted, and reports whether it removed anything.
func trimHTML(doc *html.Node) bool {
	// The first marker in document order starts the history. A marker
	// nested inside it belongs to an older message.
	n := find(doc, func(n *html.Node) bool { return isOutlookMarker(n) || hasClass(n, "gmail_quote") })
	switch {
	case n == nil:
		return false
	case isOutlookMarker(n):
		removeFrom(n)
		return true
	case textAfter(n):
		return false // an interleaved reply keeps its quotes
	default:
		n.Parent.RemoveChild(n)
		return true
	}
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
