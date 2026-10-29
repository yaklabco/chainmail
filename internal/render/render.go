package render

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-message"
	"golang.org/x/net/html"
)

// SnippetLength is the length of a snippet, in characters.
const SnippetLength = 200

// Options control Render.
type Options struct {
	// Limit truncates the body at this many bytes. Zero means no limit.
	Limit int
	// Full skips trimming and truncation.
	Full bool
}

// Body is a rendered message body.
type Body struct {
	Text string
	// Truncated says Text is incomplete: it was cut at Options.Limit, or the
	// part was too large or failed to decode partway.
	Truncated bool
	// QuotedTrimmed says a trailing quote or signature was removed, and
	// TrimmedBytes how many bytes of text that was.
	QuotedTrimmed bool
	TrimmedBytes  int
}

// Render picks the text part of raw, converts it to text, trims it and
// truncates it. A message without a text part renders as an empty Body.
// A forwarded message is never trimmed, since the forwarded text is what
// its sender meant to send. The text is not cleaned: pass it through
// CleanText before it goes into the untrusted envelope.
func Render(raw []byte, opts Options) (Body, error) {
	tp, err := pickText(raw)
	if err != nil {
		return Body{}, err
	}
	if tp == nil {
		return Body{}, nil
	}
	trim := !opts.Full && !forwarded(raw)
	var full, text string
	if tp.html {
		full, text = htmlBody(tp.text, trim)
	} else {
		full = strings.TrimRight(tp.text, " \t\n")
		text = full
		if trim {
			text = trimText(text, false)
		}
	}
	b := Body{Text: text, Truncated: tp.clipped}
	if n := len(full) - len(text); n > 0 {
		b.QuotedTrimmed = true
		b.TrimmedBytes = n
	}
	if !opts.Full {
		var cut bool
		b.Text, cut = truncate(b.Text, opts.Limit)
		b.Truncated = b.Truncated || cut
	}
	return b, nil
}

// htmlBody converts an HTML part to text, and to trimmed text when trim is
// set. HTML the tree parser refuses has no tree to trim, so it is converted
// tag by tag, as HTMLToText does, and only its text is trimmed.
func htmlBody(src string, trim bool) (full, text string) {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		full = tokenText([]byte(src))
		if !trim {
			return full, full
		}
		return full, trimText(full, true)
	}
	full = htmlText(doc)
	if !trim {
		return full, full
	}
	text = full
	if trimHTML(doc) {
		text = htmlText(doc)
	}
	return full, trimText(text, true)
}

// forwardSubjectRE matches the subject prefix a client writes on a forward,
// in the languages of attributionRE, after any tags such as "[EXTERNAL]"
// that a list or mail gateway put in front of it.
var forwardSubjectRE = regexp.MustCompile(`(?i)^\s*(\[[^\]]*\]\s*)*\[?\s*(fwd?|wg|tr|rv|enc|i|doorst|vb|vs)\s*:`)

// forwarded reports whether the subject of raw marks it as a forward.
// Outlook writes a forwarded message's header block exactly as it writes a
// reply's history, in plain text and in HTML, so only the subject tells a
// forward apart.
func forwarded(raw []byte) bool {
	e, _ := message.Read(bytes.NewReader(raw)) // an unknown charset still yields the header
	if e == nil {
		return false
	}
	s, err := e.Header.Text("Subject")
	if err != nil {
		s = e.Header.Get("Subject")
	}
	return forwardSubjectRE.MatchString(s)
}

// truncate cuts s to at most limit bytes, on a UTF-8 boundary.
func truncate(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// Snippet renders up to SnippetLength characters from the first bytes of a
// text part, as a store fetched them: still transfer-encoded and in their
// own charset. A sample cut in the middle of an encoded unit keeps what
// decoded before the cut.
func Snippet(mediaType, charset, encoding string, data []byte) string {
	var h message.Header
	params := map[string]string{}
	if charset != "" {
		params["charset"] = charset
	}
	h.SetContentType(mediaType, params)
	if encoding != "" {
		h.Set("Content-Transfer-Encoding", encoding)
	}
	e, _ := message.New(h, bytes.NewReader(data)) // an unknown charset or encoding still yields readable bytes
	b, _ := io.ReadAll(e.Body)                    // see the doc comment on cut samples
	text := toText(b)
	if strings.EqualFold(mediaType, "text/html") {
		if converted, err := HTMLToText(strings.NewReader(text)); err == nil {
			text = converted
		}
	}
	return Clip(text)
}

// Clip cleans s, collapses its whitespace and cuts it to SnippetLength characters.
func Clip(s string) string {
	s = strings.Join(strings.FieldsFunc(CleanText(s), unicode.IsSpace), " ")
	if utf8.RuneCountInString(s) <= SnippetLength {
		return s
	}
	return string([]rune(s)[:SnippetLength])
}
