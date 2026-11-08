package fake

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"

	"github.com/yaklabco/chainmail/internal/mailstore"
)

// maxMIMEDepth bounds how deep mimeLeaves follows nested multiparts, as an
// IMAP part number has at most 16 levels.
const maxMIMEDepth = 16

// mimeLeaf is one leaf of a message's MIME tree, numbered as IMAP numbers
// it: "1" for a single-part message, "2" or "1.2" inside a multipart one.
// An attached message is a leaf, as imapstore's body structure walk does not
// enter it either.
type mimeLeaf struct {
	part      string
	mediaType string
	params    map[string]string
	header    textproto.MIMEHeader
	// body is the part's body as the message has it, still in its transfer
	// encoding, so len(body) is the size a server reports.
	body []byte
}

// mimeLeaves returns the leaves of raw's MIME tree in part order. A message
// that does not parse has none, and a multipart that stops parsing keeps
// the leaves before the fault.
func mimeLeaves(raw []byte) []mimeLeaf {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	body, err := io.ReadAll(msg.Body)
	if err != nil {
		return nil
	}
	var out []mimeLeaf
	walkMIME(textproto.MIMEHeader(msg.Header), body, nil, &out)
	return out
}

func walkMIME(h textproto.MIMEHeader, body []byte, path []int, out *[]mimeLeaf) {
	mt, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		mt, params = "text/plain", nil
	}
	if boundary := params["boundary"]; strings.HasPrefix(mt, "multipart/") && boundary != "" && len(path) < maxMIMEDepth {
		mr := multipart.NewReader(bytes.NewReader(body), boundary)
		for i := 1; ; i++ {
			p, err := mr.NextRawPart()
			if err != nil {
				return
			}
			data, err := io.ReadAll(p)
			if err != nil {
				return
			}
			walkMIME(p.Header, data, append(path[:len(path):len(path)], i), out)
		}
	}
	if path == nil {
		path = []int{1}
	}
	if len(params) == 0 {
		params = nil
	}
	*out = append(*out, mimeLeaf{part: partString(path), mediaType: mt, params: params, header: h, body: body})
}

func partString(path []int) string {
	s := make([]string, len(path))
	for i, n := range path {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ".")
}

// isCalendarType reports whether mediaType holds iCalendar data.
func isCalendarType(mediaType string) bool {
	return mediaType == "text/calendar" || mediaType == "application/ics"
}

// calendarPart is the CalendarPart of a calendar leaf.
func calendarPart(part string, params map[string]string, size int64) mailstore.CalendarPart {
	return mailstore.CalendarPart{
		Part:    part,
		Method:  strings.ToUpper(strings.TrimSpace(params["method"])),
		Charset: strings.ToLower(strings.TrimSpace(params["charset"])),
		Size:    size,
	}
}

// calendarParts lists m's calendar parts as imapstore reads them from a
// body structure: from the MIME tree of an appended message, and from the
// attachments of a seeded one, under their Part numbers, which StreamPart
// opens.
func calendarParts(m *Message) []mailstore.CalendarPart {
	var out []mailstore.CalendarPart
	if m.raw != nil {
		for _, l := range mimeLeaves(m.raw) {
			if isCalendarType(l.mediaType) {
				out = append(out, calendarPart(l.part, l.params, int64(len(l.body))))
			}
		}
		return out
	}
	for _, a := range m.Attachments {
		mt, params, err := mime.ParseMediaType(a.ContentType)
		if err == nil && isCalendarType(mt) {
			out = append(out, calendarPart(a.Part, params, encodedSize(a.Data)))
		}
	}
	return out
}

// rawPart finds the leaf of an appended message by its part number, with
// the PartInfo imapstore gives and its body with the transfer encoding
// decoded.
func rawPart(raw []byte, part string) (mailstore.PartInfo, []byte, bool) {
	for _, l := range mimeLeaves(raw) {
		if l.part != part {
			continue
		}
		info := mailstore.PartInfo{ContentType: l.mediaType, Params: l.params, Size: int64(len(l.body))}
		info.ContentID = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(l.header.Get("Content-ID")), "<"), ">")
		disp, dparams, err := mime.ParseMediaType(l.header.Get("Content-Disposition"))
		name := l.params["name"]
		if err == nil {
			if f := dparams["filename"]; f != "" {
				name = f
			}
			info.Inline = disp == "inline"
		} else {
			info.Inline = info.ContentID != ""
		}
		if dec, err := new(mime.WordDecoder).DecodeHeader(name); err == nil {
			name = dec
		}
		info.Filename = name
		var rd io.Reader = bytes.NewReader(l.body)
		switch strings.ToLower(strings.TrimSpace(l.header.Get("Content-Transfer-Encoding"))) {
		case "base64":
			rd = base64.NewDecoder(base64.StdEncoding, rd)
		case "quoted-printable":
			rd = quotedprintable.NewReader(rd)
		}
		data, err := io.ReadAll(rd)
		if err != nil {
			return mailstore.PartInfo{}, nil, false
		}
		return info, data, true
	}
	return mailstore.PartInfo{}, nil, false
}
