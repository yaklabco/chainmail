// Package render turns MIME messages into text an agent can read. It picks
// the text part, decodes charsets, converts HTML, trims trailing quotes and
// signatures, truncates, and cleans message-derived strings for the
// untrusted envelope. It has no IMAP knowledge.
package render

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // registers charset decoders with go-message
)

const (
	// maxPartBytes caps how much of one text part is read. The IMAP store
	// fetches at most 4 MiB of a part, so this only bites on other input.
	maxPartBytes = 8 << 20
	// maxDepth bounds the MIME walk.
	maxDepth = 16
)

// textPart is the part chosen for display, decoded to UTF-8 with LF line ends.
type textPart struct {
	html    bool
	text    string
	clipped bool
}

// pickText parses raw as a MIME message and returns the part to show, or nil
// when the message has no text part.
func pickText(raw []byte) (*textPart, error) {
	e, err := message.Read(bytes.NewReader(raw))
	if e == nil {
		return nil, fmt.Errorf("parsing message: %w", err)
	}
	// An unknown charset or transfer encoding still leaves a readable
	// entity, whose bytes become U+FFFD where they do not decode.
	return walk(e, 0), nil
}

func walk(e *message.Entity, depth int) *textPart {
	if depth > maxDepth {
		return nil
	}
	if headerType(e.Header.Get("Content-Disposition")) == "attachment" {
		return nil
	}
	mediaType := headerType(e.Header.Get("Content-Type"))
	if mediaType == "" {
		mediaType = "text/plain"
	}
	if mr := e.MultipartReader(); mr != nil {
		return walkMultipart(mr, mediaType == "multipart/alternative", depth)
	}
	switch mediaType {
	case "text/plain":
		return readPart(e, false)
	case "text/html":
		return readPart(e, true)
	default:
		return nil
	}
}

func walkMultipart(mr message.MultipartReader, alternative bool, depth int) *textPart {
	var plain, html *textPart
	for {
		part, _ := mr.NextPart() // a part with an unknown charset comes with an error but is still readable
		if part == nil {
			break // io.EOF, or a malformed body ends the walk
		}
		tp := walk(part, depth+1)
		if tp == nil {
			continue
		}
		if !alternative {
			return tp
		}
		switch {
		case !tp.html && plain == nil:
			plain = tp
		case tp.html && html == nil:
			html = tp
		}
	}
	if plain != nil {
		return plain
	}
	return html
}

// headerType returns the lower-case type of a Content-Type or
// Content-Disposition value, or "" when the type itself is unreadable.
// Hostile mail often breaks the parameters (an unquoted filename with a
// space, an unterminated quote, a repeated name), and go-message then reports
// an error for the whole field. Only the part before the first ";" is parsed
// here, so broken parameters never hide the type.
func headerType(v string) string {
	base, _, _ := strings.Cut(v, ";")
	t, _, err := mime.ParseMediaType(base)
	if err != nil {
		return ""
	}
	return t
}

func readPart(e *message.Entity, html bool) *textPart {
	b, err := io.ReadAll(io.LimitReader(e.Body, maxPartBytes+1))
	// A decoding error midway keeps what was decoded before it, marked as
	// incomplete.
	clipped := err != nil && !errors.Is(err, io.EOF)
	if len(b) > maxPartBytes {
		b = b[:runeBoundary(b, maxPartBytes)]
		clipped = true
	}
	return &textPart{html: html, text: toText(b), clipped: clipped}
}

// runeBoundary backs n off to the start of the UTF-8 sequence that b[n]
// belongs to, so clipping b at the result splits no rune. It moves at most
// utf8.UTFMax-1 bytes, which leaves invalid input clipped at n. b must be
// longer than n.
func runeBoundary(b []byte, n int) int {
	for i := n; i > n-utf8.UTFMax && i > 0; i-- {
		if utf8.RuneStart(b[i]) {
			return i
		}
	}
	return n
}

// toText makes b valid UTF-8 with LF line ends.
func toText(b []byte) string {
	s := strings.ToValidUTF8(string(b), "\ufffd")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}
