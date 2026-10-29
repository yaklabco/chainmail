// Package render turns MIME messages into text an agent can read. It picks
// the text part, decodes charsets, converts HTML, trims trailing quotes and
// signatures, truncates, and cleans message-derived strings for the
// untrusted envelope. It has no IMAP knowledge.
package render

import (
	"bytes"
	"fmt"
	"io"
	"strings"

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
	if disp, _, err := e.Header.ContentDisposition(); err == nil && strings.EqualFold(disp, "attachment") {
		return nil
	}
	mediaType, _, err := e.Header.ContentType()
	if err != nil || mediaType == "" {
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

func readPart(e *message.Entity, html bool) *textPart {
	b, _ := io.ReadAll(io.LimitReader(e.Body, maxPartBytes+1)) // a decoding error keeps what was read before it
	clipped := len(b) > maxPartBytes
	if clipped {
		b = b[:maxPartBytes]
	}
	return &textPart{html: html, text: toText(b), clipped: clipped}
}

// toText makes b valid UTF-8 with LF line ends.
func toText(b []byte) string {
	s := strings.ToValidUTF8(string(b), "\ufffd")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}
