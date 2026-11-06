package imapstore

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"fmt"
	"maps"
	"mime"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	"github.com/yaklabco/chainmail/internal/mailstore"
)

var (
	// headerSection is the whole top-level header, without setting \Seen.
	headerSection = &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
	// referencesSection is the References header alone, for thread IDs on
	// servers without X-GM-THRID.
	referencesSection = &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"References"}, Peek: true}

	wordDecoder = &mime.WordDecoder{CharsetReader: charset.Reader}
	partRE      = regexp.MustCompile(`^[1-9][0-9]{0,3}(\.[1-9][0-9]{0,3}){0,15}$`)
)

// textSection is the body of a single-part message, capped.
func textSection() *imap.FetchItemBodySection {
	return &imap.FetchItemBodySection{Specifier: imap.PartSpecifierText, Peek: true, Partial: &imap.SectionPartial{Size: mailstore.TextPartCap}}
}

func mimeSection(path []int) *imap.FetchItemBodySection {
	return &imap.FetchItemBodySection{Part: path, Specifier: imap.PartSpecifierMIME, Peek: true}
}

func bodySection(path []int) *imap.FetchItemBodySection {
	return &imap.FetchItemBodySection{Part: path, Peek: true, Partial: &imap.SectionPartial{Size: mailstore.TextPartCap}}
}

func isAttachment(sp *imap.BodyStructureSinglePart) bool {
	if d := sp.Disposition(); d != nil && strings.EqualFold(d.Value, "attachment") {
		return true
	}
	return sp.Filename() != "" || sp.MediaType() == "message/rfc822"
}

func isTextLeaf(sp *imap.BodyStructureSinglePart) bool {
	mt := sp.MediaType()
	return (mt == "text/plain" || mt == "text/html") && !isAttachment(sp)
}

func partString(path []int) string {
	s := make([]string, len(path))
	for i, n := range path {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ".")
}

// parsePart parses an IMAP part number such as 2 or 1.2.
func parsePart(s string) ([]int, bool) {
	if !partRE.MatchString(s) {
		return nil, false
	}
	var path []int
	for _, f := range strings.Split(s, ".") {
		n, _ := strconv.Atoi(f) // the pattern allows only small positive numbers
		path = append(path, n)
	}
	return path, true
}

func decodeWord(s string) string {
	if d, err := wordDecoder.DecodeHeader(s); err == nil {
		return d
	}
	return s
}

func param(params map[string]string, key string) string {
	for k, v := range params {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// isShown reports whether sp is a part the body shows by its Content-ID,
// such as an unnamed inline image, that is neither an attachment nor the
// body text itself.
func isShown(sp *imap.BodyStructureSinglePart) bool {
	return contentID(sp) != "" && !isAttachment(sp) && !isTextLeaf(sp)
}

// contentID is sp's Content-ID without angle brackets.
func contentID(sp *imap.BodyStructureSinglePart) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(sp.ID), "<"), ">")
}

// partInfo describes sp for streaming: its filename, media type and
// parameters, Content-ID and whether the body shows it.
func partInfo(sp *imap.BodyStructureSinglePart) mailstore.PartInfo {
	info := mailstore.PartInfo{Filename: decodeWord(sp.Filename()), ContentType: sp.MediaType(), ContentID: contentID(sp), Size: int64(sp.Size)}
	if len(sp.Params) > 0 {
		info.Params = maps.Clone(sp.Params)
	}
	if d := sp.Disposition(); d != nil {
		info.Inline = strings.EqualFold(d.Value, "inline")
	} else {
		info.Inline = info.ContentID != ""
	}
	return info
}

// hasAttachment reports whether a body structure has an attachment part. A
// part only the body shows, such as an unnamed inline image, is not one.
func hasAttachment(bs imap.BodyStructure) bool {
	found := false
	if bs == nil {
		return false
	}
	bs.Walk(func(_ []int, part imap.BodyStructure) bool {
		if sp, ok := part.(*imap.BodyStructureSinglePart); ok && isAttachment(sp) {
			found = true
		}
		return !found
	})
	return found
}

// attachmentsOf lists the attachment parts in a body structure, and the
// parts without a filename the body shows by Content-ID.
func attachmentsOf(bs imap.BodyStructure) []mailstore.Attachment {
	var out []mailstore.Attachment
	if bs == nil {
		return nil
	}
	bs.Walk(func(path []int, part imap.BodyStructure) bool {
		sp, ok := part.(*imap.BodyStructureSinglePart)
		if !ok || !isAttachment(sp) && !isShown(sp) {
			return true
		}
		name := decodeWord(sp.Filename())
		if name == "" && sp.MediaType() == "message/rfc822" {
			name = "message.eml"
		}
		out = append(out, mailstore.Attachment{Part: partString(path), Filename: name, ContentType: sp.MediaType(), Size: int64(sp.Size)})
		return true
	})
	return out
}

// textLeaf returns the part a snippet is taken from: the first text/plain
// leaf, else the first text/html one.
func textLeaf(bs imap.BodyStructure) ([]int, *imap.BodyStructureSinglePart) {
	var plainPath, htmlPath []int
	var plain, html *imap.BodyStructureSinglePart
	if bs == nil {
		return nil, nil
	}
	bs.Walk(func(path []int, part imap.BodyStructure) bool {
		sp, ok := part.(*imap.BodyStructureSinglePart)
		if !ok || !isTextLeaf(sp) {
			return true
		}
		switch {
		case sp.MediaType() == "text/plain" && plain == nil:
			plain, plainPath = sp, slices.Clone(path)
		case sp.MediaType() == "text/html" && html == nil:
			html, htmlPath = sp, slices.Clone(path)
		}
		return true
	})
	if plain != nil {
		return plainPath, plain
	}
	return htmlPath, html
}

// findPart returns the leaf at path, or nil.
func findPart(bs imap.BodyStructure, path []int) *imap.BodyStructureSinglePart {
	var found *imap.BodyStructureSinglePart
	if bs == nil {
		return nil
	}
	bs.Walk(func(p []int, part imap.BodyStructure) bool {
		if sp, ok := part.(*imap.BodyStructureSinglePart); ok && slices.Equal(p, path) {
			found = sp
		}
		return found == nil
	})
	return found
}

// textSections lists what to fetch to rebuild a message for rendering: the
// capped TEXT of a single-part text message, or the MIME header and capped
// body of every text leaf of a multipart one.
func textSections(bs imap.BodyStructure) []*imap.FetchItemBodySection {
	switch root := bs.(type) {
	case *imap.BodyStructureSinglePart:
		if isTextLeaf(root) {
			return []*imap.FetchItemBodySection{textSection()}
		}
	case *imap.BodyStructureMultiPart:
		var out []*imap.FetchItemBodySection
		root.Walk(func(path []int, part imap.BodyStructure) bool {
			if sp, ok := part.(*imap.BodyStructureSinglePart); ok && isTextLeaf(sp) {
				p := slices.Clone(path)
				out = append(out, mimeSection(p), bodySection(p))
			}
			return true
		})
		return out
	}
	return nil
}

// rebuild assembles a MIME message from the original header and the
// fetched text leaves, keeping the multipart structure and dropping every
// other part. part returns a fetched section, or nil. clipped reports a text
// part that reached the fetch cap.
func rebuild(header []byte, bs imap.BodyStructure, part func(*imap.FetchItemBodySection) []byte) (raw []byte, clipped bool, err error) {
	var buf bytes.Buffer
	switch root := bs.(type) {
	case *imap.BodyStructureMultiPart:
		h := readHeader(header)
		boundary := newBoundary()
		h.Set("Content-Type", mime.FormatMediaType("multipart/"+strings.ToLower(root.Subtype), map[string]string{"boundary": boundary}))
		h.Del("Content-Transfer-Encoding")
		if err := textproto.WriteHeader(&buf, h); err != nil {
			return nil, false, fmt.Errorf("writing header: %w", err)
		}
		writeMultipart(&buf, root, nil, boundary, part, &clipped)
	case *imap.BodyStructureSinglePart:
		buf.Write(header)
		if isTextLeaf(root) {
			body := part(textSection())
			clipped = len(body) >= mailstore.TextPartCap
			buf.Write(body)
		}
	default:
		buf.Write(header)
	}
	return buf.Bytes(), clipped, nil
}

func writeMultipart(w *bytes.Buffer, mp *imap.BodyStructureMultiPart, path []int, boundary string, part func(*imap.FetchItemBodySection) []byte, clipped *bool) {
	for i, child := range mp.Children {
		var body bytes.Buffer
		if !writeNode(&body, child, append(slices.Clone(path), i+1), part, clipped) {
			continue
		}
		w.WriteString("--" + boundary + "\r\n")
		w.Write(body.Bytes())
		w.WriteString("\r\n")
	}
	w.WriteString("--" + boundary + "--\r\n")
}

func writeNode(w *bytes.Buffer, node imap.BodyStructure, path []int, part func(*imap.FetchItemBodySection) []byte, clipped *bool) bool {
	switch n := node.(type) {
	case *imap.BodyStructureSinglePart:
		if !isTextLeaf(n) {
			return false
		}
		body := part(bodySection(path))
		if len(body) >= mailstore.TextPartCap {
			*clipped = true
		}
		w.Write(part(mimeSection(path)))
		w.Write(body)
		return true
	case *imap.BodyStructureMultiPart:
		if len(textSections(n)) == 0 {
			return false
		}
		boundary := newBoundary()
		fmt.Fprintf(w, "Content-Type: %s\r\n\r\n", mime.FormatMediaType("multipart/"+strings.ToLower(n.Subtype), map[string]string{"boundary": boundary}))
		writeMultipart(w, n, path, boundary, part, clipped)
		return true
	default:
		return false
	}
}

func newBoundary() string { return "chainmail-" + rand.Text() }

// msgIDList reads a message-ID list header, such as References, from a header block.
func msgIDList(header []byte, key string) []string {
	if len(header) == 0 {
		return nil
	}
	h := readHeader(header)
	mh := mail.Header{Header: message.Header{Header: h}}
	ids, err := mh.MsgIDList(key)
	if err != nil {
		return nil
	}
	return ids
}

// readHeader parses a header block leniently. A line that is not a header
// field, such as one without a colon, is dropped rather than making the
// whole message unreadable.
func readHeader(raw []byte) textproto.Header {
	h, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err == nil {
		return h
	}
	var kept bytes.Buffer
	field := false // whether the last line kept was a field, which a continuation may extend
	for line := range bytes.Lines(raw) {
		content := bytes.TrimRight(line, "\r\n")
		if len(content) == 0 {
			break // the blank line ends the header
		}
		switch {
		case content[0] == ' ' || content[0] == '\t':
			if !field {
				continue // it continues a line that was dropped, or none
			}
		case headerField(content):
			field = true
		default:
			field = false
			continue
		}
		kept.Write(content)
		kept.WriteString("\r\n")
	}
	kept.WriteString("\r\n")
	// What is left is well formed. Were it not, h would still hold the
	// fields parsed before the bad line, which is the best there is.
	h, _ = textproto.ReadHeader(bufio.NewReader(&kept))
	return h
}

// headerField reports whether line starts with a field name and a colon.
func headerField(line []byte) bool {
	name, _, ok := bytes.Cut(line, []byte(":"))
	if !ok || len(name) == 0 {
		return false
	}
	for _, b := range name {
		if b <= ' ' || b > '~' {
			return false
		}
	}
	return true
}
